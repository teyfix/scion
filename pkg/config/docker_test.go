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
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateSettings_ProfileDocker(t *testing.T) {
	tests := []struct {
		name      string
		yamlData  string
		wantValid bool
	}{
		{
			name: "profile with docker privileged true",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      privileged: true
`,
			wantValid: true,
		},
		{
			name: "profile with docker privileged false",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      privileged: false
`,
			wantValid: true,
		},
		{
			name: "profile with Docker CDI device",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      devices:
        - nvidia.com/gpu=all
`,
			wantValid: true,
		},
		{
			name: "profile with empty Docker device rejected",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      devices:
        - ""
`,
			wantValid: false,
		},
		{
			name: "profile without docker",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
`,
			wantValid: true,
		},
		{
			name: "profile with unknown docker field rejected",
			yamlData: `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      privileged: true
      unsupported_field: "bad"
`,
			wantValid: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			valErrors, err := ValidateSettings([]byte(tc.yamlData), "1")
			if err != nil {
				t.Fatalf("ValidateSettings unexpected error: %v", err)
			}
			if tc.wantValid && len(valErrors) > 0 {
				t.Errorf("expected valid, got validation errors: %v", valErrors)
			}
			if !tc.wantValid && len(valErrors) == 0 {
				t.Error("expected validation errors, got none")
			}
		})
	}
}

func TestValidateAgentConfig_Docker(t *testing.T) {
	tests := []struct {
		name      string
		yamlData  string
		wantValid bool
	}{
		{
			name: "agent config with docker privileged true",
			yamlData: `schema_version: "1"
docker:
  privileged: true
`,
			wantValid: true,
		},
		{
			name: "agent config with docker privileged false",
			yamlData: `schema_version: "1"
docker:
  privileged: false
`,
			wantValid: true,
		},
		{
			name: "agent config with Docker CDI device",
			yamlData: `schema_version: "1"
docker:
  devices:
    - nvidia.com/gpu=all
`,
			wantValid: true,
		},
		{
			name: "agent config without docker",
			yamlData: `schema_version: "1"
`,
			wantValid: true,
		},
		{
			name: "agent config with unknown docker field rejected",
			yamlData: `schema_version: "1"
docker:
  privileged: true
  unsupported_field: "bad"
`,
			wantValid: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			valErrors, err := ValidateAgentConfig([]byte(tc.yamlData), "1")
			if err != nil {
				t.Fatalf("ValidateAgentConfig unexpected error: %v", err)
			}
			if tc.wantValid && len(valErrors) > 0 {
				t.Errorf("expected valid, got validation errors: %v", valErrors)
			}
			if !tc.wantValid && len(valErrors) == 0 {
				t.Error("expected validation errors, got none")
			}
		})
	}
}

func TestSettings_DockerProfile_DecodingAndConversions(t *testing.T) {
	yamlData := `schema_version: "1"
profiles:
  onprem:
    runtime: docker
    docker:
      privileged: true
      devices:
        - nvidia.com/gpu=all
  unprivileged:
    runtime: docker
    docker:
      privileged: false
`

	var vs VersionedSettings
	if err := yaml.Unmarshal([]byte(yamlData), &vs); err != nil {
		t.Fatalf("failed to unmarshal VersionedSettings: %v", err)
	}

	onprem, ok := vs.Profiles["onprem"]
	if !ok {
		t.Fatal("expected 'onprem' profile to exist")
	}
	if onprem.Docker == nil || onprem.Docker.Privileged == nil || !*onprem.Docker.Privileged {
		t.Errorf("expected onprem.Docker.Privileged=true, got %v", onprem.Docker)
	}
	if len(onprem.Docker.Devices) != 1 || onprem.Docker.Devices[0] != "nvidia.com/gpu=all" {
		t.Errorf("expected onprem.Docker.Devices=[nvidia.com/gpu=all], got %v", onprem.Docker.Devices)
	}

	unpriv, ok := vs.Profiles["unprivileged"]
	if !ok {
		t.Fatal("expected 'unprivileged' profile to exist")
	}
	if unpriv.Docker == nil || unpriv.Docker.Privileged == nil || *unpriv.Docker.Privileged {
		t.Errorf("expected unprivileged.Docker.Privileged=false, got %v", unpriv.Docker)
	}

	// Test convertVersionedToLegacy
	legacy := convertVersionedToLegacy(&vs)
	legacyOnprem, ok := legacy.Profiles["onprem"]
	if !ok {
		t.Fatal("expected legacy 'onprem' profile to exist")
	}
	if legacyOnprem.Docker == nil || legacyOnprem.Docker.Privileged == nil || !*legacyOnprem.Docker.Privileged {
		t.Errorf("expected legacy onprem.Docker.Privileged=true, got %v", legacyOnprem.Docker)
	}
	if len(legacyOnprem.Docker.Devices) != 1 || legacyOnprem.Docker.Devices[0] != "nvidia.com/gpu=all" {
		t.Errorf("expected legacy onprem.Docker.Devices=[nvidia.com/gpu=all], got %v", legacyOnprem.Docker.Devices)
	}

	// Test AdaptLegacySettings
	adapted, warnings := AdaptLegacySettings(legacy)
	if len(warnings) > 0 {
		t.Logf("AdaptLegacySettings warnings: %v", warnings)
	}
	adaptedOnprem, ok := adapted.Profiles["onprem"]
	if !ok {
		t.Fatal("expected adapted 'onprem' profile to exist")
	}
	if adaptedOnprem.Docker == nil || adaptedOnprem.Docker.Privileged == nil || !*adaptedOnprem.Docker.Privileged {
		t.Errorf("expected adapted onprem.Docker.Privileged=true, got %v", adaptedOnprem.Docker)
	}
	if len(adaptedOnprem.Docker.Devices) != 1 || adaptedOnprem.Docker.Devices[0] != "nvidia.com/gpu=all" {
		t.Errorf("expected adapted onprem.Docker.Devices=[nvidia.com/gpu=all], got %v", adaptedOnprem.Docker.Devices)
	}

	// Verify deep copy / no aliasing between legacy and adapted
	*legacyOnprem.Docker.Privileged = false
	legacyOnprem.Docker.Devices[0] = "mutated"
	if !*adaptedOnprem.Docker.Privileged {
		t.Errorf("mutating legacy Docker mutated adapted Docker: expected deep copy")
	}
	if adaptedOnprem.Docker.Devices[0] != "nvidia.com/gpu=all" {
		t.Errorf("mutating legacy Docker devices mutated adapted Docker: expected deep copy")
	}
}
