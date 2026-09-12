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

	// Template with networks and labels (using scoped variable references)
	tplDir := filepath.Join(globalTemplatesDir, "docker-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness",
		"env": {
			"APP_DOMAIN": "prod.corp.net"
		},
		"docker": {
			"networks": ["shared_net", "template_net"],
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

	// 2. Verify Labels precedence and scoped dynamic expansion
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

	// 3. Verify on-disk persistence in scion-agent.json
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
	for k, wantVal := range wantLabels {
		if diskCfg.Docker.Labels[k] != wantVal {
			t.Errorf("persisted label[%q] = %q, want %q", k, diskCfg.Docker.Labels[k], wantVal)
		}
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
