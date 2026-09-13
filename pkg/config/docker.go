// Copyright 2026 Teyfix
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

package config

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ResolveDockerDevices applies the NVIDIA attachment choice after all config
// layers have merged. Keep the configured selectors intact so restarts retain
// the policy and enabling GPUs preserves an administrator's selected devices.
// This controls device grants, not host access from privileged containers or
// flags injected by an external Docker wrapper.
func ResolveDockerDevices(d *api.DockerConfig) []string {
	if d == nil {
		return nil
	}
	var devices []string
	hasNvidia := false
	for _, device := range d.Devices {
		device = strings.TrimSpace(device)
		if device == "" {
			continue
		}
		source, _, _ := strings.Cut(device, ":")
		nvidia := strings.HasPrefix(source, "nvidia.com/gpu=") || strings.HasPrefix(source, "/dev/nvidia")
		if nvidia && d.NvidiaGPU != nil && !*d.NvidiaGPU {
			continue
		}
		hasNvidia = hasNvidia || nvidia
		devices = append(devices, device)
	}
	if d.NvidiaGPU != nil && *d.NvidiaGPU && !hasNvidia {
		devices = append(devices, "nvidia.com/gpu=all")
	}
	return devices
}
