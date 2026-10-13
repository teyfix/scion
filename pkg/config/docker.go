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
	"fmt"
	"regexp"
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

// CloneDockerConfig returns a deep copy of a DockerConfig.
func CloneDockerConfig(d *api.DockerConfig) *api.DockerConfig {
	if d == nil {
		return nil
	}
	res := &api.DockerConfig{}
	if d.NvidiaGPU != nil {
		v := *d.NvidiaGPU
		res.NvidiaGPU = &v
	}
	if d.Privileged != nil {
		v := *d.Privileged
		res.Privileged = &v
	}
	if d.Networks != nil {
		res.Networks = append([]string(nil), d.Networks...)
	}
	if d.Devices != nil {
		res.Devices = append([]string(nil), d.Devices...)
	}
	if d.Labels != nil {
		res.Labels = make(map[string]string, len(d.Labels))
		for k, v := range d.Labels {
			res.Labels[k] = v
		}
	}
	return res
}

func MergeDockerConfig(base, override *api.DockerConfig) *api.DockerConfig {
	if override == nil {
		return base
	}

	if base == nil {
		base = &api.DockerConfig{}
	}

	result := *base

	if override.NvidiaGPU != nil {
		v := *override.NvidiaGPU
		result.NvidiaGPU = &v
	}

	if override.Privileged != nil {
		v := *override.Privileged
		result.Privileged = &v
	}

	// Networks: Profile networks are required attachments and must be retained.
	// Perform union of base and override networks with deduplication, preserving order.
	if len(base.Networks) > 0 || len(override.Networks) > 0 {
		seen := make(map[string]struct{}, len(base.Networks)+len(override.Networks))
		var mergedNets []string
		for _, net := range base.Networks {
			if net == "" {
				continue
			}
			if _, exists := seen[net]; !exists {
				seen[net] = struct{}{}
				mergedNets = append(mergedNets, net)
			}
		}
		for _, net := range override.Networks {
			if net == "" {
				continue
			}
			if _, exists := seen[net]; !exists {
				seen[net] = struct{}{}
				mergedNets = append(mergedNets, net)
			}
		}
		result.Networks = mergedNets
	}

	// Devices: Profile device grants are required and must be retained. Docker
	// accepts both host device mappings and CDI selectors (for example,
	// nvidia.com/gpu=all) through repeated --device arguments.
	if len(base.Devices) > 0 || len(override.Devices) > 0 {
		seen := make(map[string]struct{}, len(base.Devices)+len(override.Devices))
		var mergedDevices []string
		for _, device := range base.Devices {
			if device == "" {
				continue
			}
			if _, exists := seen[device]; !exists {
				seen[device] = struct{}{}
				mergedDevices = append(mergedDevices, device)
			}
		}
		for _, device := range override.Devices {
			if device == "" {
				continue
			}
			if _, exists := seen[device]; !exists {
				seen[device] = struct{}{}
				mergedDevices = append(mergedDevices, device)
			}
		}
		result.Devices = mergedDevices
	}

	// Labels: deterministic map merge where override keys overwrite base keys.
	if len(base.Labels) > 0 || len(override.Labels) > 0 {
		mergedLabels := make(map[string]string, len(base.Labels)+len(override.Labels))
		for k, v := range base.Labels {
			mergedLabels[k] = v
		}
		for k, v := range override.Labels {
			mergedLabels[k] = v
		}
		result.Labels = mergedLabels
	}

	return &result
}

// ValidateDockerConfig rejects malformed device requests and labels that could
// replace Scion identity or launch ownership metadata.
func ValidateDockerConfig(d *api.DockerConfig) error {
	if d == nil {
		return nil
	}
	for key := range d.Labels {
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return fmt.Errorf("invalid docker label key %q", key)
		}
		if strings.HasPrefix(strings.ToLower(key), "scion.") {
			return fmt.Errorf("docker label %q is reserved for Scion", key)
		}
	}
	for _, network := range d.Networks {
		if strings.TrimSpace(network) == "" || strings.ContainsAny(network, "\x00\r\n") {
			return fmt.Errorf("invalid docker network %q", network)
		}
	}
	for _, device := range d.Devices {
		if strings.TrimSpace(device) != device || device == "" || strings.ContainsAny(device, "\x00\r\n") {
			return fmt.Errorf("invalid docker device %q", device)
		}
		if strings.Contains(device, "=") {
			if !dockerCDIDevicePattern.MatchString(device) {
				return fmt.Errorf("invalid Docker CDI selector %q: expected vendor/class=device", device)
			}
		} else if !strings.HasPrefix(device, "/") {
			return fmt.Errorf("invalid docker device %q: expected an absolute host path or CDI selector", device)
		}
	}
	return nil
}

var dockerCDIDevicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*/[a-zA-Z0-9][a-zA-Z0-9_.-]*=[a-zA-Z0-9][a-zA-Z0-9_.:-]*$`)
