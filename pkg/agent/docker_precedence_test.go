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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func boolPtr(b bool) *bool {
	return &b
}

func setupPrecedenceEnv(t *testing.T, profilePrivileged *bool, templatePrivileged *bool) (string, string) {
	t.Helper()
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	_ = os.MkdirAll(globalTemplatesDir, 0755)

	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	// Project dir
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	seedTestHarnessConfig(t, projectScionDir, "test-harness", "test-harness")

	// Settings with onprem profile
	settingsContent := `schema_version: "1"
active_profile: onprem
profiles:
  onprem:
    runtime: docker
`
	if profilePrivileged != nil {
		settingsContent += fmt.Sprintf("    docker:\n      privileged: %v\n", *profilePrivileged)
	}
	settingsContent += `harness_configs:
  test-harness:
    harness: test-harness
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	// Template dir in global templates so it's always found
	tplDir := filepath.Join(globalTemplatesDir, "test-template")
	_ = os.MkdirAll(tplDir, 0755)

	tplMap := map[string]interface{}{
		"schema_version":         "1",
		"default_harness_config": "test-harness",
	}
	if templatePrivileged != nil {
		tplMap["docker"] = map[string]interface{}{
			"privileged": *templatePrivileged,
		}
	}
	tplData, err := json.Marshal(tplMap)
	if err != nil {
		t.Fatalf("failed to marshal template json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), tplData, 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}

	return projectDir, "test-template"
}

func TestProvisionAgent_DockerPrivileged_Precedence(t *testing.T) {
	tests := []struct {
		name               string
		profilePrivileged  *bool
		templatePrivileged *bool
		inlinePrivileged   *bool
		wantPrivileged     bool
	}{
		{
			name:               "profile true, template unset, inline unset -> true",
			profilePrivileged:  boolPtr(true),
			templatePrivileged: nil,
			inlinePrivileged:   nil,
			wantPrivileged:     true,
		},
		{
			name:               "profile true, template false, inline unset -> false (template overrides profile)",
			profilePrivileged:  boolPtr(true),
			templatePrivileged: boolPtr(false),
			inlinePrivileged:   nil,
			wantPrivileged:     false,
		},
		{
			name:               "profile false, template true, inline unset -> true (template overrides profile)",
			profilePrivileged:  boolPtr(false),
			templatePrivileged: boolPtr(true),
			inlinePrivileged:   nil,
			wantPrivileged:     true,
		},
		{
			name:               "profile true, template true, inline false -> false (inline false overrides profile and template)",
			profilePrivileged:  boolPtr(true),
			templatePrivileged: boolPtr(true),
			inlinePrivileged:   boolPtr(false),
			wantPrivileged:     false,
		},
		{
			name:               "profile false, template false, inline true -> true (inline true overrides profile and template)",
			profilePrivileged:  boolPtr(false),
			templatePrivileged: boolPtr(false),
			inlinePrivileged:   boolPtr(true),
			wantPrivileged:     true,
		},
		{
			name:               "profile unset, template unset, inline unset -> false (default unprivileged)",
			profilePrivileged:  nil,
			templatePrivileged: nil,
			inlinePrivileged:   nil,
			wantPrivileged:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			projectDir, tplName := setupPrecedenceEnv(t, tc.profilePrivileged, tc.templatePrivileged)

			var inlineConfig *api.ScionConfig
			if tc.inlinePrivileged != nil {
				inlineConfig = &api.ScionConfig{
					Docker: &api.DockerConfig{
						Privileged: tc.inlinePrivileged,
					},
				}
			}

			workspaceDir := filepath.Join(projectDir, "custom-workspace")
			_ = os.MkdirAll(workspaceDir, 0755)

			agentHome, _, finalCfg, err := ProvisionAgent(
				context.Background(),
				"agent-1",
				tplName,
				"",
				"test-harness",
				projectDir,
				"onprem",
				"",
				"",
				workspaceDir,
				inlineConfig,
			)
			if err != nil {
				t.Fatalf("ProvisionAgent failed: %v", err)
			}

			// Verify in-memory resolved config
			isPriv := finalCfg != nil && finalCfg.Docker != nil && finalCfg.Docker.Privileged != nil && *finalCfg.Docker.Privileged
			if isPriv != tc.wantPrivileged {
				t.Errorf("in-memory finalCfg Privileged = %v, want %v", isPriv, tc.wantPrivileged)
			}

			// Verify persisted scion-agent.json on disk (in agentDir = filepath.Dir(agentHome))
			agentDir := filepath.Dir(agentHome)
			diskCfgData, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
			if err != nil {
				t.Fatalf("failed to read persisted scion-agent.json: %v", err)
			}
			var diskCfg api.ScionConfig
			if err := json.Unmarshal(diskCfgData, &diskCfg); err != nil {
				t.Fatalf("failed to unmarshal persisted scion-agent.json: %v", err)
			}
			diskPriv := diskCfg.Docker != nil && diskCfg.Docker.Privileged != nil && *diskCfg.Docker.Privileged
			if diskPriv != tc.wantPrivileged {
				t.Errorf("persisted scion-agent.json Privileged = %v, want %v", diskPriv, tc.wantPrivileged)
			}
		})
	}

	t.Run("empty profile name resolves active_profile and supplies docker.privileged", func(t *testing.T) {
		projectDir, tplName := setupPrecedenceEnv(t, boolPtr(true), nil)
		workspaceDir := filepath.Join(projectDir, "custom-workspace")
		_ = os.MkdirAll(workspaceDir, 0755)

		agentHome, _, finalCfg, err := ProvisionAgent(
			context.Background(),
			"agent-active-profile",
			tplName,
			"",
			"test-harness",
			projectDir,
			"", // empty profileName -> resolves settings.ActiveProfile ("onprem")
			"",
			"",
			workspaceDir,
			nil,
		)
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}

		// Verify in-memory resolved config
		isPriv := finalCfg != nil && finalCfg.Docker != nil && finalCfg.Docker.Privileged != nil && *finalCfg.Docker.Privileged
		if !isPriv {
			t.Errorf("expected in-memory finalCfg.Docker.Privileged=true from ActiveProfile, got false/nil")
		}

		// Verify persisted scion-agent.json on disk
		agentDir := filepath.Dir(agentHome)
		diskCfgData, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
		if err != nil {
			t.Fatalf("failed to read persisted scion-agent.json: %v", err)
		}
		var diskCfg api.ScionConfig
		if err := json.Unmarshal(diskCfgData, &diskCfg); err != nil {
			t.Fatalf("failed to unmarshal persisted scion-agent.json: %v", err)
		}
		diskPriv := diskCfg.Docker != nil && diskCfg.Docker.Privileged != nil && *diskCfg.Docker.Privileged
		if !diskPriv {
			t.Errorf("expected persisted scion-agent.json Privileged=true from ActiveProfile, got false/nil")
		}
	})
}

func TestManager_Start_RunConfig_Privileged_And_Persistence(t *testing.T) {
	projectDir, tplName := setupPrecedenceEnv(t, boolPtr(true), nil)
	workspaceDir := filepath.Join(projectDir, "workspace")
	_ = os.MkdirAll(workspaceDir, 0755)

	var capturedConfig runtime.RunConfig
	var containerID string
	var containerPhase string

	mockRt := &runtime.MockRuntime{
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			containerID = "container-" + config.Name
			containerPhase = "running"
			return containerID, nil
		},
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			if containerID == "" {
				return nil, nil
			}
			return []api.AgentInfo{
				{
					ContainerID:     containerID,
					Name:            "agent-test",
					Phase:           containerPhase,
					ContainerStatus: "Up 1 minute",
				},
			}, nil
		},
		DeleteFunc: func(ctx context.Context, id string) error {
			containerID = ""
			containerPhase = ""
			return nil
		},
	}

	mgr := NewManager(mockRt)

	// 1. Start a new agent provisioned with profile privileged=true
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "agent-test",
		Template:      tplName,
		HarnessConfig: "test-harness",
		ProjectPath:   projectDir,
		Profile:       "onprem",
		Workspace:     workspaceDir,
	})
	if err != nil {
		t.Fatalf("mgr.Start failed: %v", err)
	}

	if !capturedConfig.Privileged {
		t.Errorf("expected RunConfig.Privileged=true on initial start, got false")
	}

	// 2. Simulate container recreation / restart from persisted scion-agent.json
	capturedConfig = runtime.RunConfig{}
	containerID = "container-agent-test"
	containerPhase = "stopped"

	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:          "agent-test",
		Template:      tplName,
		HarnessConfig: "test-harness",
		ProjectPath:   projectDir,
		Profile:       "onprem",
		Workspace:     workspaceDir,
		Task:          "re-run task", // triggers container recreation
	})
	if err != nil {
		t.Fatalf("mgr.Start (recreation) failed: %v", err)
	}

	if !capturedConfig.Privileged {
		t.Errorf("expected RunConfig.Privileged=true on recreation/restart from persisted config, got false")
	}
}
