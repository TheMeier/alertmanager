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

package common

import (
	commoncfg "github.com/prometheus/common/config"
)

// GlobalDefaults carries the global configuration values that individual
// notifier configs may inherit when a local value is not set.
// It lives in config/common so that notifier packages can reference it
// without creating an import cycle against the top-level config package.
type GlobalDefaults struct {
	HTTPConfig *commoncfg.HTTPClientConfig
}

// GlobalDefaultsMerger is implemented by any notifier config that wants to
// inherit values from the global configuration.
type GlobalDefaultsMerger interface {
	// MergeGlobalDefaults fills in any unset fields on the receiver from g.
	// It returns an error if the resulting configuration is invalid.
	MergeGlobalDefaults(g *GlobalDefaults) error
}
