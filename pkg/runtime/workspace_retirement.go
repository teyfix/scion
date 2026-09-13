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
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkspaceRetirementGuard is optional runtime authority for destructive native
// worktree cleanup. Runtimes without complete mount inspection fail closed.
type WorkspaceRetirementGuard interface {
	AssertWorkspaceUnused(context.Context, string) error
}

func (r *DockerRuntime) AssertWorkspaceUnused(ctx context.Context, target string) error {
	return assertContainerWorkspaceUnused(ctx, r.Command, target)
}

func (r *PodmanRuntime) AssertWorkspaceUnused(ctx context.Context, target string) error {
	return assertContainerWorkspaceUnused(ctx, r.Command, target)
}

func assertContainerWorkspaceUnused(ctx context.Context, command, target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("invalid worktree mount target")
	}
	out, err := exec.CommandContext(ctx, command, "ps", "-a", "--no-trunc", "--format", "{{.ID}}").Output()
	if err != nil {
		return fmt.Errorf("enumerate all runtime containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	remaining := make(map[string]bool, len(ids))
	for _, id := range ids {
		if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" || remaining[id] {
			return fmt.Errorf("invalid or duplicate container identity during mount inspection")
		}
		remaining[id] = true
	}
	args := []string{"inspect", "--format", `{"id":{{json .Id}},"mounts":{{json .Mounts}}}`}
	out, err = exec.CommandContext(ctx, command, append(args, ids...)...).Output()
	if err != nil {
		return fmt.Errorf("inspect all runtime mounts: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var facts struct {
			ID     string `json:"id"`
			Mounts *[]struct {
				Source string `json:"Source"`
				Type   string `json:"Type"`
			} `json:"mounts"`
		}
		if err := json.Unmarshal([]byte(line), &facts); err != nil || !remaining[facts.ID] || facts.Mounts == nil {
			return fmt.Errorf("incomplete or invalid runtime mount inspection")
		}
		delete(remaining, facts.ID)
		for _, mount := range *facts.Mounts {
			if mount.Source == "" {
				if mount.Type == "tmpfs" {
					continue // Runtime-managed tmpfs has no host source.
				}
				return fmt.Errorf("runtime omitted a host mount source")
			}
			if !filepath.IsAbs(mount.Source) {
				return fmt.Errorf("runtime returned a nonabsolute mount source")
			}
			source := filepath.Clean(mount.Source)
			// Parent broker mounts provide access to managed storage and are not
			// another agent's exact worktree. Exact/child mounts retain its bytes.
			rel, err := filepath.Rel(target, source)
			if err != nil {
				return fmt.Errorf("compare runtime mount source: %w", err)
			}
			if rel == "." || filepath.IsLocal(rel) {
				return fmt.Errorf("container %s still mounts the selected worktree", facts.ID)
			}
		}
	}
	if len(remaining) != 0 {
		return fmt.Errorf("runtime omitted containers from mount inspection")
	}
	return nil
}
