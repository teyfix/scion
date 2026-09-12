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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestDockerRuntime_Run_NoInitFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	out, err := runtime.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}

	// sciontool handles PID 1 responsibilities, so --init should NOT be present
	if strings.Contains(out, "--init") {
		t.Errorf("expected '--init' to be absent in output, got %q", out)
	}

	if !strings.Contains(out, "run -t") {
		t.Errorf("expected 'run -t' in output, got %q", out)
	}
}

func TestDockerRuntime_Exec_UserFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	out, err := runtime.Exec(context.Background(), "test-container", []string{"whoami"})
	if err != nil {
		t.Fatalf("runtime.Exec failed: %v", err)
	}

	if !strings.Contains(out, "--user scion") {
		t.Errorf("expected '--user scion' in exec output, got %q", out)
	}
}

func TestDockerRuntime_Run_PrivilegedFlag(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	t.Run("privileged enabled emits exactly one --privileged", func(t *testing.T) {
		config := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
			Privileged:   true,
		}

		out, err := runtime.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("runtime.Run failed: %v", err)
		}

		count := strings.Count(out, "--privileged")
		if count != 1 {
			t.Errorf("expected exactly 1 '--privileged' flag in output, got %d (output: %q)", count, out)
		}

		// Verify --privileged appears before the image argument
		privIdx := strings.Index(out, "--privileged")
		imgIdx := strings.Index(out, "scion-agent:latest")
		if privIdx == -1 || imgIdx == -1 || privIdx > imgIdx {
			t.Errorf("expected '--privileged' before image name, got privIdx=%d, imgIdx=%d", privIdx, imgIdx)
		}
	})

	t.Run("privileged disabled omits --privileged", func(t *testing.T) {
		config := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
			Privileged:   false,
		}

		out, err := runtime.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("runtime.Run failed: %v", err)
		}

		if strings.Contains(out, "--privileged") {
			t.Errorf("expected '--privileged' to be absent in output, got %q", out)
		}
	})
}

func TestDockerRuntime_ListReportsObservedRuntimeFacts(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
case "$1" in
  ps)
    printf '%s\n' \
      '{"ID":"gpu-id","Names":"gpu-agent","Status":"Up 1 minute","Image":"agent:latest","Labels":"scion.agent=true"}' \
      '{"ID":"plain-id","Names":"plain-agent","Status":"Exited (0) 1 minute ago","Image":"agent:latest","Labels":"scion.agent=true"}'
    ;;
  inspect)
    printf '%s\n' \
      '{"id":"gpu-id","privileged":true,"devices":[],"deviceRequests":[{"Driver":"cdi","DeviceIDs":["nvidia.com/gpu=all"],"Capabilities":null}]}' \
      '{"id":"plain-id","privileged":false,"devices":[],"deviceRequests":[]}'
    ;;
esac
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{Command: mockDocker}
	agents, err := runtime.List(context.Background(), map[string]string{"scion.agent": "true"})
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}

	byName := make(map[string]struct {
		privileged *bool
		nvidiaGPU  *bool
	}, len(agents))
	for _, agent := range agents {
		byName[agent.Name] = struct {
			privileged *bool
			nvidiaGPU  *bool
		}{agent.Privileged, agent.NvidiaGPU}
	}

	gpu := byName["gpu-agent"]
	if gpu.privileged == nil || !*gpu.privileged {
		t.Fatalf("expected gpu-agent to report privileged=true, got %v", gpu.privileged)
	}
	if gpu.nvidiaGPU == nil || !*gpu.nvidiaGPU {
		t.Fatalf("expected gpu-agent to report nvidiaGpu=true, got %v", gpu.nvidiaGPU)
	}

	plain := byName["plain-agent"]
	if plain.privileged == nil || *plain.privileged {
		t.Fatalf("expected plain-agent to report privileged=false, got %v", plain.privileged)
	}
	if plain.nvidiaGPU == nil || *plain.nvidiaGPU {
		t.Fatalf("expected plain-agent to report nvidiaGpu=false, got %v", plain.nvidiaGPU)
	}
}
