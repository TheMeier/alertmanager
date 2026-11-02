// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package synthetic holds synthetic end-to-end tests that run Alertmanager
// components against real third-party services in containers. It is a
// separate Go module so that its test-only dependencies stay out of the
// main go.mod.
package synthetic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	amcommoncfg "github.com/prometheus/alertmanager/config/common"

	"github.com/prometheus/alertmanager/alert"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/notify/matrix"
	"github.com/prometheus/alertmanager/notify/test"
)

const (
	synapseImage = "matrixdotorg/synapse:latest"
	synapseUser  = "alertmanager"
	synapsePass  = "alertmanager-e2e"
)

// TestMatrixSynapse sends a notification to a real Synapse homeserver and
// reads it back from the room. Run it from this directory:
//
//	go test -run TestMatrixSynapse -v ./...
//
// With rootless podman, set TESTCONTAINERS_RYUK_DISABLED=true because Ryuk
// expects a network named "bridge".
func TestMatrixSynapse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	homeserver := startSynapse(ctx, t)
	token := login(ctx, t, homeserver)
	roomID := createRoom(ctx, t, homeserver, token)

	cfg := matrix.DefaultMatrixConfig
	cfg.HTTPConfig = &commoncfg.HTTPClientConfig{}
	cfg.HomeserverURL = &amcommoncfg.URL{URL: homeserver}
	cfg.AccessToken = commoncfg.Secret(token)
	cfg.RoomID = roomID
	require.NoError(t, cfg.Validate())
	n, err := matrix.New(&cfg, test.CreateTmpl(t), promslog.NewNopLogger())
	require.NoError(t, err)

	nctx := notify.WithNow(notify.WithGroupKey(ctx, "e2e"), time.Now())
	verdict := n.Notify(nctx, alert.New(model.Alert{
		Labels:   model.LabelSet{"alertname": "MatrixE2E"},
		StartsAt: time.Now(),
		EndsAt:   time.Now().Add(time.Hour),
	}, time.Time{}, false))
	require.NoError(t, verdict.Err())

	var messages struct {
		Chunk []struct {
			Type    string         `json:"type"`
			Content map[string]any `json:"content"`
		} `json:"chunk"`
	}
	matrixDo(ctx, t, homeserver, token, http.MethodGet,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/messages?dir=b&limit=10", nil, &messages)

	var bodies []string
	for _, ev := range messages.Chunk {
		if ev.Type == "m.room.message" {
			bodies = append(bodies, ev.Content["body"].(string))
		}
	}
	require.Len(t, bodies, 1)
	require.Contains(t, bodies[0], "MatrixE2E")
}

// startSynapse runs a throwaway Synapse and registers a test user. It
// returns the homeserver base URL.
func startSynapse(ctx context.Context, t *testing.T) *url.URL {
	t.Helper()
	ctr, err := testcontainers.Run(ctx, synapseImage,
		testcontainers.WithEnv(map[string]string{
			"SYNAPSE_SERVER_NAME":  "localhost",
			"SYNAPSE_REPORT_STATS": "no",
		}),
		// The image no longer generates a config on the fly, so generate one
		// before starting the server.
		testcontainers.WithEntrypoint("/bin/sh", "-c", "mkdir -p /data && /start.py generate && exec /start.py"),
		testcontainers.WithExposedPorts("8008/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/health").WithPort("8008/tcp").WithStartupTimeout(2*time.Minute),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	code, out, err := ctr.Exec(ctx, []string{
		"register_new_matrix_user", "-c", "/data/homeserver.yaml",
		"-u", synapseUser, "-p", synapsePass, "--no-admin", "http://localhost:8008",
	})
	require.NoError(t, err)
	if code != 0 {
		b, _ := io.ReadAll(out)
		t.Fatalf("register_new_matrix_user exited with %d: %s", code, b)
	}

	endpoint, err := ctr.PortEndpoint(ctx, "8008/tcp", "http")
	require.NoError(t, err)
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	return u
}

func login(ctx context.Context, t *testing.T, hs *url.URL) string {
	t.Helper()
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	matrixDo(ctx, t, hs, "", http.MethodPost, "/_matrix/client/v3/login", map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]string{"type": "m.id.user", "user": synapseUser},
		"password":   synapsePass,
	}, &resp)
	require.NotEmpty(t, resp.AccessToken)
	return resp.AccessToken
}

func createRoom(ctx context.Context, t *testing.T, hs *url.URL, token string) string {
	t.Helper()
	var resp struct {
		RoomID string `json:"room_id"`
	}
	matrixDo(ctx, t, hs, token, http.MethodPost, "/_matrix/client/v3/createRoom",
		map[string]any{"name": "alertmanager-e2e"}, &resp)
	require.NotEmpty(t, resp.RoomID)
	return resp.RoomID
}

// matrixDo performs a Client-Server API call and decodes the JSON response.
func matrixDo(ctx context.Context, t *testing.T, hs *url.URL, token, method, path string, in, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		require.NoError(t, err)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, hs.String()+path, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s %s: %s", method, path, b)
	require.NoError(t, json.Unmarshal(b, out))
}
