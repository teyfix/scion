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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
)

func TestDockerConfig_PrecedenceAndResolution(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	// Template with networks, devices, and labels (using scoped variable references)
	tplDir := filepath.Join(globalTemplatesDir, "docker-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness",
		"env": {
			"APP_DOMAIN": "prod.corp.net"
		},
		"docker": {
			"networks": ["shared_net", "template_net"],
			"devices": ["/dev/fuse", "/dev/dri"],
			"labels": {
				"router.rule": "Host(${SCION_AGENT_SLUG}.${APP_DOMAIN})",
				"overwrite.me": "from-template",
				"template.key": "template-val"
			}
		}
	}`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	// Project settings with profile docker configuration
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	projectSettings := `schema_version: "1"
profiles:
  test-profile:
    runtime: docker
    docker:
      networks:
        - profile_net
        - shared_net
      devices:
        - nvidia.com/gpu=all
        - /dev/fuse
      labels:
        service.domain: "${APP_DOMAIN}"
        profile.key: "profile-val"
        overwrite.me: "from-profile"
harness_configs:
  test-harness:
    harness: test-harness
`
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644)

	// Inline override
	inlineConfig := &api.ScionConfig{
		Docker: &api.DockerConfig{
			Networks: []string{"inline_net"},
			Devices:  []string{"/dev/dri", "/dev/kvm"},
			Labels: map[string]string{
				"overwrite.me": "from-inline",
				"inline.agent": "${SCION_AGENT_NAME}",
			},
		},
	}

	agentName := "My Test Agent"
	_, _, cfg, err := ProvisionAgent(
		context.Background(),
		agentName,
		"docker-tpl",
		"",
		"",
		projectScionDir,
		"test-profile",
		"",
		"",
		"",
		inlineConfig,
	)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	if cfg.Docker == nil {
		t.Fatal("cfg.Docker is nil")
	}

	// 1. Verify Networks union with deduplication and order retention:
	// Profile ("profile_net", "shared_net") + Template ("shared_net", "template_net") + Inline ("inline_net")
	wantNets := []string{"profile_net", "shared_net", "template_net", "inline_net"}
	if len(cfg.Docker.Networks) != len(wantNets) {
		t.Fatalf("cfg.Docker.Networks = %v, want %v", cfg.Docker.Networks, wantNets)
	}
	for i, net := range wantNets {
		if cfg.Docker.Networks[i] != net {
			t.Errorf("cfg.Docker.Networks[%d] = %q, want %q", i, cfg.Docker.Networks[i], net)
		}
	}

	// 2. Verify Devices union with deduplication and order retention.
	wantDevices := []string{"nvidia.com/gpu=all", "/dev/fuse", "/dev/dri", "/dev/kvm"}
	if len(cfg.Docker.Devices) != len(wantDevices) {
		t.Fatalf("cfg.Docker.Devices = %v, want %v", cfg.Docker.Devices, wantDevices)
	}
	for i, device := range wantDevices {
		if cfg.Docker.Devices[i] != device {
			t.Errorf("cfg.Docker.Devices[%d] = %q, want %q", i, cfg.Docker.Devices[i], device)
		}
	}

	// 3. Verify Labels precedence and scoped dynamic expansion
	wantLabels := map[string]string{
		"profile.key":    "profile-val",
		"template.key":   "template-val",
		"overwrite.me":   "from-inline",
		"service.domain": "prod.corp.net",
		"router.rule":    "Host(my-test-agent.prod.corp.net)",
		"inline.agent":   "My Test Agent",
	}

	if len(cfg.Docker.Labels) != len(wantLabels) {
		t.Fatalf("len(cfg.Docker.Labels) = %d, want %d (got %v)", len(cfg.Docker.Labels), len(wantLabels), cfg.Docker.Labels)
	}
	for k, wantVal := range wantLabels {
		if gotVal, ok := cfg.Docker.Labels[k]; !ok || gotVal != wantVal {
			t.Errorf("cfg.Docker.Labels[%q] = %q, want %q", k, gotVal, wantVal)
		}
	}

	// 4. Verify on-disk persistence in scion-agent.json
	agentDir := config.GetAgentDir(projectScionDir, agentName, false)
	savedFile := filepath.Join(agentDir, "scion-agent.json")
	savedData, err := os.ReadFile(savedFile)
	if err != nil {
		t.Fatalf("failed to read persisted scion-agent.json: %v", err)
	}

	var diskCfg api.ScionConfig
	if err := json.Unmarshal(savedData, &diskCfg); err != nil {
		t.Fatalf("failed to unmarshal persisted scion-agent.json: %v", err)
	}

	if diskCfg.Docker == nil {
		t.Fatal("persisted diskCfg.Docker is nil")
	}
	if len(diskCfg.Docker.Networks) != len(wantNets) {
		t.Errorf("persisted networks = %v, want %v", diskCfg.Docker.Networks, wantNets)
	}
	if len(diskCfg.Docker.Devices) != len(wantDevices) {
		t.Errorf("persisted devices = %v, want %v", diskCfg.Docker.Devices, wantDevices)
	}
	for i, device := range wantDevices {
		if diskCfg.Docker.Devices[i] != device {
			t.Errorf("persisted devices[%d] = %q, want %q", i, diskCfg.Docker.Devices[i], device)
		}
	}
	for k, wantVal := range wantLabels {
		if diskCfg.Docker.Labels[k] != wantVal {
			t.Errorf("persisted label[%q] = %q, want %q", k, diskCfg.Docker.Labels[k], wantVal)
		}
	}
}

func TestValidateDockerDeviceRuntime(t *testing.T) {
	tests := []struct {
		name        string
		runtimeName string
		devices     []string
		wantErr     bool
	}{
		{name: "Docker CDI", runtimeName: "docker", devices: []string{"nvidia.com/gpu=all"}},
		{name: "Podman host mapping", runtimeName: "podman", devices: []string{"/dev/dri:/dev/dri"}},
		{name: "mock supports unit tests", runtimeName: "mock", devices: []string{"nvidia.com/gpu=all"}},
		{name: "no device request", runtimeName: "kubernetes"},
		{name: "empty device request", runtimeName: "cloudrun", devices: []string{""}},
		{name: "Apple container rejected", runtimeName: "container", devices: []string{"nvidia.com/gpu=all"}, wantErr: true},
		{name: "Kubernetes rejected", runtimeName: "kubernetes", devices: []string{"nvidia.com/gpu=all"}, wantErr: true},
		{name: "Cloud Run rejected", runtimeName: "cloudrun", devices: []string{"nvidia.com/gpu=all"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDockerDeviceRuntime(tc.runtimeName, tc.devices)
			if tc.wantErr && err == nil {
				t.Fatal("validateDockerDeviceRuntime() error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDockerDeviceRuntime() error = %v, want nil", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.runtimeName) {
				t.Errorf("error %q does not identify runtime %q", err, tc.runtimeName)
			}
		})
	}
}

func TestStartPassesDockerDevicesToRuntime(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name   string
		policy *bool
		want   []string
	}{
		{"inherit", nil, []string{"nvidia.com/gpu=all", "/dev/dri"}},
		{"enabled", &enabled, []string{"nvidia.com/gpu=all", "/dev/dri"}},
		{"disabled", &disabled, []string{"/dev/dri"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			defer func() { _ = os.Chdir(oldWd) }()

			originalHome := os.Getenv("HOME")
			defer func() { _ = os.Setenv("HOME", originalHome) }()
			_ = os.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			seedTestHarnessConfig(t, globalScionDir, "generic", "generic")

			tplDir := filepath.Join(globalScionDir, "templates", "device-tpl")
			_ = os.MkdirAll(tplDir, 0755)
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "generic",
		"docker": {"devices": ["/dev/dri"]}
	}`), 0644)
			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    docker:
      devices: ["nvidia.com/gpu=all"]
`), 0644)

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			_ = os.MkdirAll(projectScionDir, 0755)

			var captured runtime.RunConfig
			mockRuntime := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return nil, nil
				},
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					captured = cfg
					return "mock-id", nil
				},
			}

			mgr := NewManager(mockRuntime)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:         "device-agent",
				Template:     "device-tpl",
				ProjectPath:  projectScionDir,
				NoAuth:       true,
				InlineConfig: &api.ScionConfig{Docker: &api.DockerConfig{NvidiaGPU: tc.policy}},
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			want := tc.want
			if len(captured.Devices) != len(want) {
				t.Fatalf("RunConfig.Devices = %v, want %v", captured.Devices, want)
			}
			for i := range want {
				if captured.Devices[i] != want[i] {
					t.Errorf("RunConfig.Devices[%d] = %q, want %q", i, captured.Devices[i], want[i])
				}
			}

			// Provisioning persists the explicit policy for a later start.
			saved, err := os.ReadFile(filepath.Join(config.GetAgentDir(projectScionDir, "device-agent", false), "scion-agent.json"))
			if err != nil {
				t.Fatal(err)
			}
			var persisted api.ScionConfig
			if err := json.Unmarshal(saved, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.Docker == nil {
				t.Fatal("missing persisted Docker config")
			}
			if tc.policy != nil && (persisted.Docker.NvidiaGPU == nil || *persisted.Docker.NvidiaGPU != *tc.policy) {
				t.Fatal("NVIDIA attachment choice was not persisted")
			}

			// Starting the saved agent without resending inline config keeps the choice.
			captured = runtime.RunConfig{}
			_, err = mgr.Start(context.Background(), api.StartOptions{
				Name: "device-agent", ProjectPath: projectScionDir, NoAuth: true,
			})
			if err != nil {
				t.Fatalf("start saved agent: %v", err)
			}
			assert.ElementsMatch(t, tc.want, captured.Devices, "saved agent device grants")
		})
	}
}

func TestDockerConfig_UnresolvedVariableFailsProvisioning(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	tplDir := filepath.Join(globalTemplatesDir, "bad-var-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness",
		"docker": {
			"labels": {
				"router.rule": "Host(${NONEXISTENT_VAR}.example.com)"
			}
		}
	}`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	_, _, _, err := ProvisionAgent(
		context.Background(),
		"bad-agent",
		"bad-var-tpl",
		"",
		"",
		projectScionDir,
		"",
		"",
		"",
		"",
	)
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail due to unresolved variable, got nil")
	}
	if !strings.Contains(err.Error(), "unresolved variable ${NONEXISTENT_VAR}") {
		t.Errorf("error %q does not contain expected text", err.Error())
	}
}

func TestDockerConfig_ReservedLabelCollisionFailsProvisioning(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	tplDir := filepath.Join(globalTemplatesDir, "reserved-label-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness",
		"docker": {
			"labels": {
				"scion.agent": "imposter"
			}
		}
	}`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	_, _, _, err := ProvisionAgent(
		context.Background(),
		"reserved-agent",
		"reserved-label-tpl",
		"",
		"",
		projectScionDir,
		"",
		"",
		"",
		"",
	)
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail due to reserved label collision, got nil")
	}
	if !strings.Contains(err.Error(), "collides with reserved internal label") {
		t.Errorf("error %q does not contain expected text", err.Error())
	}
}

func TestDockerConfig_ProfileEnvPassthrough(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	tplDir := filepath.Join(globalTemplatesDir, "base-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness"
	}`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	projectSettings := `schema_version: "1"
profiles:
  passthrough-profile:
    runtime: docker
    docker:
      labels:
        "traefik.enable": "true"
        "traefik.http.routers.${SCION_AGENT_SLUG}.rule": "Host(${SCION_AGENT_SLUG}.${APP_DOMAIN})"
    env:
      APP_DOMAIN: ""
harness_configs:
  test-harness:
    harness: test-harness
`
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644)

	t.Setenv("APP_DOMAIN", "tail.gg")

	agentName := "Worker-42"
	_, _, cfg, err := ProvisionAgent(
		context.Background(),
		agentName,
		"base-tpl",
		"",
		"",
		projectScionDir,
		"passthrough-profile",
		"",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	if cfg.Docker == nil {
		t.Fatal("cfg.Docker is nil")
	}

	wantLabels := map[string]string{
		"traefik.enable":                      "true",
		"traefik.http.routers.worker-42.rule": "Host(worker-42.tail.gg)",
	}
	for k, wantVal := range wantLabels {
		if gotVal, ok := cfg.Docker.Labels[k]; !ok || gotVal != wantVal {
			t.Errorf("cfg.Docker.Labels[%q] = %q, want %q", k, gotVal, wantVal)
		}
	}

	// G3-full guard: profile env must NOT be merged into container env
	if cfg.Env != nil && cfg.Env["APP_DOMAIN"] != "" {
		t.Errorf("APP_DOMAIN should NOT be present in container cfg.Env, got %q", cfg.Env["APP_DOMAIN"])
	}

	// Verify on-disk persistence in scion-agent.json
	agentDir := config.GetAgentDir(projectScionDir, agentName, false)
	savedData, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatalf("failed to read persisted scion-agent.json: %v", err)
	}
	var diskCfg api.ScionConfig
	if err := json.Unmarshal(savedData, &diskCfg); err != nil {
		t.Fatalf("failed to unmarshal scion-agent.json: %v", err)
	}
	if diskCfg.Docker == nil {
		t.Fatal("persisted diskCfg.Docker is nil")
	}
	for k, wantVal := range wantLabels {
		if diskCfg.Docker.Labels[k] != wantVal {
			t.Errorf("persisted label[%q] = %q, want %q", k, diskCfg.Docker.Labels[k], wantVal)
		}
	}
	if diskCfg.Env != nil && diskCfg.Env["APP_DOMAIN"] != "" {
		t.Errorf("APP_DOMAIN should NOT be present in persisted diskCfg.Env, got %q", diskCfg.Env["APP_DOMAIN"])
	}
}

func TestDockerConfig_ProfileEnvPassthroughUnsetFailsProvisioning(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	tplDir := filepath.Join(globalTemplatesDir, "base-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness"
	}`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	projectSettings := `schema_version: "1"
profiles:
  passthrough-profile:
    runtime: docker
    docker:
      labels:
        "traefik.http.routers.${SCION_AGENT_SLUG}.rule": "Host(${SCION_AGENT_SLUG}.${APP_DOMAIN})"
    env:
      APP_DOMAIN: ""
harness_configs:
  test-harness:
    harness: test-harness
`
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644)

	// Ensure APP_DOMAIN is unset on host
	orig, exists := os.LookupEnv("APP_DOMAIN")
	if exists {
		_ = os.Unsetenv("APP_DOMAIN")
		defer func() {
			if orig != "" {
				_ = os.Setenv("APP_DOMAIN", orig)
			}
		}()
	}

	_, _, _, err := ProvisionAgent(
		context.Background(),
		"worker-fail",
		"base-tpl",
		"",
		"",
		projectScionDir,
		"passthrough-profile",
		"",
		"",
		"",
	)
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail due to unset passthrough env var, got nil")
	}
	if !strings.Contains(err.Error(), "unresolved variable ${APP_DOMAIN}") {
		t.Errorf("error %q does not contain expected text %q", err.Error(), "unresolved variable ${APP_DOMAIN}")
	}
}
