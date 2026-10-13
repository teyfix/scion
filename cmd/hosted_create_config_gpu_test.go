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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setHostedTransportFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	f := cmd.Flags().Lookup(name)
	require.NotNil(t, f)
	oldValue, oldChanged := f.Value.String(), f.Changed
	t.Cleanup(func() {
		require.NoError(t, f.Value.Set(oldValue))
		f.Changed = oldChanged
	})
	require.NoError(t, cmd.Flags().Set(name, value))
}

func TestCreateAgentViaHub_InlineConfigOnWire(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline config", true: "explicit flags override"}[override], func(t *testing.T) {
			resetHubStartGlobals(t)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("Use the GPU for the assigned task."), 0600))
			path := filepath.Join(dir, "role.yaml")
			require.NoError(t, os.WriteFile(path, []byte(`image: role-agent:v1
harness_config: role-codex
auth_selectedType: auth-file
model: large
branch: role-branch
env:
  ROLE_KIND: heavy
resources:
  limits:
    cpu: "4"
    memory: 6Gi
docker:
  privileged: true
  nvidia_gpu: true
agent_instructions: file://instructions.md
`), 0600))
			setHostedTransportFlag(t, createCmd, "config", path)
			setHostedTransportFlag(t, createCmd, "workspace", "source/subdir")
			if override {
				setHostedTransportFlag(t, createCmd, "image", "explicit-agent:v2")
				setHostedTransportFlag(t, createCmd, "harness", "explicit-codex")
				setHostedTransportFlag(t, createCmd, "harness-auth", "api-key")
				setHostedTransportFlag(t, createCmd, "branch", "explicit-branch")
			}
			stub := newHubStartStub(t, "project-inline", "inline-agent", "")
			var err error
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, "project-inline"), "inline-agent", "do the task")
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			assert.Equal(t, true, stub.createBody["provisionOnly"])
			assert.Equal(t, "do the task", stub.createBody["task"])
			assert.Equal(t, "source/subdir", stub.createBody["workspace"])
			wantBranch := "role-branch"
			if override {
				wantBranch = "explicit-branch"
			}
			assert.Equal(t, wantBranch, stub.createBody["branch"])
			cfg, ok := stub.createBody["config"].(map[string]interface{})
			require.True(t, ok, "complete inline config must reach the Hub")
			wantImage, wantHarness, wantAuth := "role-agent:v1", "role-codex", "auth-file"
			if override {
				wantImage, wantHarness, wantAuth = "explicit-agent:v2", "explicit-codex", "api-key"
			}
			assert.Equal(t, wantImage, cfg["image"])
			assert.Equal(t, wantHarness, cfg["harness_config"])
			assert.Equal(t, wantAuth, cfg["auth_selectedType"])
			assert.Equal(t, "large", cfg["model"])
			assert.Equal(t, "Use the GPU for the assigned task.", cfg["agent_instructions"])
			assert.Equal(t, map[string]interface{}{"ROLE_KIND": "heavy"}, cfg["env"])
			assert.Equal(t, map[string]interface{}{"limits": map[string]interface{}{"cpu": "4", "memory": "6Gi"}, "requests": map[string]interface{}{}}, cfg["resources"])
			assert.Equal(t, map[string]interface{}{"privileged": true, "nvidia_gpu": true}, cfg["docker"])
		})
	}
}

func TestCreateAgentViaHub_InlineConfigStdinPreservesFalseRuntimeFlags(t *testing.T) {
	resetHubStartGlobals(t)
	path := filepath.Join(t.TempDir(), "stdin.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"image":"stdin-agent:v1","docker":{"privileged":false,"nvidia_gpu":false}}`), 0600))
	input, err := os.Open(path)
	require.NoError(t, err)
	oldStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = input.Close()
	})
	setHostedTransportFlag(t, createCmd, "config", "-")
	stub := newHubStartStub(t, "project-stdin", "stdin-agent", "")
	captureStdIO(t, func() {
		err = createAgentViaHub(stub.hubCtx(t, "project-stdin"), "stdin-agent", "")
	})
	require.NoError(t, err)
	cfg, ok := stub.createBody["config"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "stdin-agent:v1", cfg["image"])
	assert.Equal(t, map[string]interface{}{"privileged": false, "nvidia_gpu": false}, cfg["docker"])
}

func TestStartAgentViaHub_InlineConfigFlagPrecedenceOnWire(t *testing.T) {
	resetHubStartGlobals(t)
	setHostedTransportFlag(t, startCmd, "image", "explicit-agent:v2")
	setHostedTransportFlag(t, startCmd, "harness", "explicit-codex")
	setHostedTransportFlag(t, startCmd, "harness-auth", "api-key")
	inline := &api.ScionConfig{
		Image: "role-agent:v1", HarnessConfig: "role-codex", AuthSelectedType: "auth-file", Branch: "role-branch",
	}
	stub := newHubStartStub(t, "project-start-inline", "start-inline", "")
	var err error
	captureStdIO(t, func() {
		err = startAgentViaHub(startCmd, stub.hubCtx(t, "project-start-inline"), "start-inline", "", false, inline)
	})
	require.NoError(t, err)
	assert.Equal(t, "role-branch", stub.createBody["branch"])
	cfg, ok := stub.createBody["config"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "explicit-agent:v2", cfg["image"])
	assert.Equal(t, "explicit-codex", cfg["harness_config"])
	assert.Equal(t, "api-key", cfg["auth_selectedType"])
}

func TestCreateAgentViaHub_InvalidInlineConfigDoesNotPost(t *testing.T) {
	resetHubStartGlobals(t)
	setHostedTransportFlag(t, createCmd, "config", filepath.Join(t.TempDir(), "missing.yaml"))
	stub := newHubStartStub(t, "project-invalid-inline", "invalid-inline", "")
	var err error
	captureStdIO(t, func() {
		err = createAgentViaHub(stub.hubCtx(t, "project-invalid-inline"), "invalid-inline", "")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read config file")
	assert.Zero(t, stub.createCalls)
}

func TestHostedCreateAndStart_RequireGPUOnWire(t *testing.T) {
	for _, cmd := range []*cobra.Command{createCmd, startCmd} {
		for _, value := range []string{"true", "false"} {
			t.Run(cmd.Name()+"/"+value, func(t *testing.T) {
				resetHubStartGlobals(t)
				setHostedTransportFlag(t, cmd, "require-gpu", value)
				stub := newHubStartStub(t, "project-gpu", "gpu-agent", "")
				var err error
				captureStdIO(t, func() {
					if cmd == createCmd {
						err = createAgentViaHub(stub.hubCtx(t, "project-gpu"), "gpu-agent", "")
					} else {
						err = startAgentViaHub(cmd, stub.hubCtx(t, "project-gpu"), "gpu-agent", "", false, nil)
					}
				})
				require.NoError(t, err)
				require.Equal(t, 1, stub.createCalls)
				if value == "true" {
					assert.Equal(t, true, stub.createBody["requireGpu"])
				} else {
					assert.NotContains(t, stub.createBody, "requireGpu", "false matches the Hub's omitted default")
				}
			})
		}
	}
}

func TestRequireGPURejectsLocalCreateAndStart(t *testing.T) {
	for _, cmd := range []*cobra.Command{createCmd, startCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			resetHubStartGlobals(t)
			resetAgentLogLevelGlobals(t)
			state := saveCreateTestState()
			t.Cleanup(state.restore)
			t.Setenv("HOME", t.TempDir())
			projectPath, noHub = filepath.Join(t.TempDir(), ".scion"), true
			require.NoError(t, os.MkdirAll(projectPath, 0755))
			setHostedTransportFlag(t, cmd, "require-gpu", "true")
			err := cmd.RunE(cmd, []string{"local-gpu-agent"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--require-gpu requires Hub mode")
			assert.True(t, isUsageError(err), "local GPU placement must use the native usage-error marker")
			assert.Equal(t, 1, exitCodeFor(err), "native usage errors preserve the wrapped validation error's exit code")
			_, statErr := os.Stat(filepath.Join(projectPath, "agents", "local-gpu-agent"))
			assert.True(t, os.IsNotExist(statErr), "usage error must precede provisioning")
		})
	}
}

func TestRequireGPUIgnoredWarningForExistingAgent(t *testing.T) {
	resetHubStartGlobals(t)
	setHostedTransportFlag(t, startCmd, "require-gpu", "true")
	_, stderr := captureStdIO(t, func() {
		warnFlagsIgnoredForExistingAgent(startCmd, "existing-agent", "created")
	})
	assert.Contains(t, stderr, "--require-gpu")
	assert.Contains(t, stderr, "not applied to an existing agent")
}
