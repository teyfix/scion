// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Exercise the same YAML decoder as CLI --config and the actual provisioning
// and Start assembly. Runtime.Run is mocked so no Hub or container is needed.
func TestStartInlineRoleConfigPreservesTemplateNetworkAndExplicitRuntime(t *testing.T) {
	const image = "animatrix.local/scions-agent:a4296d6-dind"
	for _, tc := range []struct {
		name, cpu, memory string
		gpu               bool
	}{
		{"worker", "2", "2Gi", false},
		{"build", "4", "4Gi", false},
		{"heavy", "4", "6Gi", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, _ := envFixture(t, envSettingsV1)
			templateDir := filepath.Join(os.Getenv("HOME"), ".scion", "templates", tc.name)
			mustMkdir(t, templateDir)
			mustWrite(t, filepath.Join(templateDir, "scion-agent.yaml"), `docker:
  networks: [traefik_proxy]
resources:
  gpu: 1
`)
			inlineYAML := fmt.Sprintf(`image: %s
resources:
  limits:
    cpu: %q
    memory: %s
docker:
  privileged: true
`, image, tc.cpu, tc.memory)
			if tc.gpu {
				inlineYAML += "  nvidia_gpu: true\n"
			}
			var inline api.ScionConfig
			require.NoError(t, yaml.Unmarshal([]byte(inlineYAML), &inline))
			first := startEnv(t, project, func(opts *api.StartOptions) {
				opts.Template = tc.name
				opts.InlineConfig = &inline
			})
			require.Equal(t, image, first.Image)
			require.NotNil(t, first.Resources)
			require.Equal(t, api.ResourceList{CPU: tc.cpu, Memory: tc.memory}, first.Resources.Limits)
			require.Equal(t, []string{"traefik_proxy"}, first.Networks)
			require.True(t, first.Privileged)
			if tc.gpu {
				require.Contains(t, first.Devices, "nvidia.com/gpu=all")
			} else {
				require.Empty(t, first.Devices, "legacy template resources.gpu must not grant devices")
			}

			// Requester values survive an ordinary restart; the live network
			// remains a template default instead of being pinned in own config.
			saved, _ := persistedEnvAgentConfig(t, project)
			require.Equal(t, inline.Docker, saved.Docker)
			require.Equal(t, inline.Resources, saved.Resources)
			require.Equal(t, image, saved.Image)
			second := startEnv(t, project, nil)
			require.Equal(t, first.Image, second.Image)
			require.Equal(t, first.Resources.Limits, second.Resources.Limits)
			require.Equal(t, first.Networks, second.Networks)
			require.Equal(t, first.Devices, second.Devices)
			require.True(t, second.Privileged)
		})
	}
}
