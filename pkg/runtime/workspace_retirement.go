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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WorkspaceRetirementGuard is optional runtime authority for destructive native
// worktree cleanup. Runtimes without complete mount inspection fail closed.
type WorkspaceRetirementGuard interface {
	AssertWorkspaceUnused(context.Context, string) error
}

func (r *DockerRuntime) AssertWorkspaceUnused(ctx context.Context, target string) error {
	return assertContainerWorkspaceUnused(ctx, r.Command, r, target)
}

func (r *PodmanRuntime) AssertWorkspaceUnused(ctx context.Context, target string) error {
	return assertContainerWorkspaceUnused(ctx, r.Command, r, target)
}

func assertContainerWorkspaceUnused(ctx context.Context, command string, rt Runtime, target string) error {
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
	canonicalTarget, err := retirementMountTarget(target)
	if err != nil {
		return fmt.Errorf("resolve worktree mount authority: %w", err)
	}
	identities, err := retirementObjectIdentities(target)
	if err != nil {
		return fmt.Errorf("read worktree object authority: %w", err)
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
				Source      string `json:"Source"`
				Destination string `json:"Destination"`
				Type        string `json:"Type"`
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
			if (mount.Type != "bind" && mount.Type != "volume") || !filepath.IsAbs(mount.Destination) || filepath.Clean(mount.Destination) != mount.Destination {
				return fmt.Errorf("runtime mount destination/type authority is unavailable")
			}
			source := filepath.Clean(mount.Source)
			if retirementMountWithin(target, source) {
				return fmt.Errorf("container %s still mounts the selected worktree", facts.ID)
			}
			// Docker inspection preserves lexical bind sources, including aliases.
			// An invisible/unresolvable daemon-host path is not proof that its
			// mounted bytes are unrelated. Refuse instead of falling back to Clean.
			canonicalSource, err := filepath.EvalSymlinks(source)
			if err != nil {
				return fmt.Errorf("resolve container %s host mount authority: %w", facts.ID, err)
			}
			if retirementMountWithin(canonicalTarget, canonicalSource) {
				return fmt.Errorf("container %s still mounts the selected worktree through an alias", facts.ID)
			}
			sourceInfo, err := os.Stat(canonicalSource)
			if err != nil {
				return fmt.Errorf("read current mount source identity: %w", err)
			}
			sourceIdentity, err := retirementObjectIdentity(sourceInfo)
			if err != nil {
				return err
			}
			// Observe the established mounted object, not just the current Source
			// pathname. Retarget/rename/replacement can change that pathname while
			// a foreign bind mount keeps the original worktree inode alive.
			mounted, err := rt.Exec(ctx, facts.ID, []string{"stat", "-L", "-c", "%d:%i", "--", mount.Destination})
			if err != nil {
				return fmt.Errorf("container %s mounted-object ownership unverifiable (runtime exec/stat user, tool or permission capability): %w", facts.ID, err)
			}
			mountedIdentity, err := retirementObservedIdentity(mounted)
			if err != nil {
				return fmt.Errorf("container %s mounted-object ownership unverifiable: %w", facts.ID, err)
			}
			if identities[mountedIdentity] {
				return fmt.Errorf("container %s still mounts an actual worktree object", facts.ID)
			}
			if mountedIdentity != sourceIdentity {
				return fmt.Errorf("container %s mounted-object ownership unverifiable: established object differs from current source", facts.ID)
			}
			if retirementMountWithin(canonicalSource, canonicalTarget) {
				// No trusted immutable self-container ID is wired to the authenticated
				// broker. Labels/hostname are cross-checks, not ownership authority;
				// inventing a broad-mount exemption would endanger foreign workspaces.
				return fmt.Errorf("container %s ancestor mount ownership unverifiable: trusted owning-broker identity is unavailable", facts.ID)
			}
		}
	}
	if len(remaining) != 0 {
		return fmt.Errorf("runtime omitted containers from mount inspection")
	}
	return nil
}

type retirementIdentityKey struct {
	device, inode uint64
}

func retirementObjectIdentity(info os.FileInfo) (retirementIdentityKey, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return retirementIdentityKey{}, fmt.Errorf("filesystem object identity capability unavailable")
	}
	return retirementIdentityKey{uint64(stat.Dev), uint64(stat.Ino)}, nil
}

func retirementObservedIdentity(out string) (retirementIdentityKey, error) {
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) != 2 {
		return retirementIdentityKey{}, fmt.Errorf("invalid mounted stat identity")
	}
	device, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return retirementIdentityKey{}, fmt.Errorf("invalid mounted stat device: %w", err)
	}
	inode, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return retirementIdentityKey{}, fmt.Errorf("invalid mounted stat inode: %w", err)
	}
	return retirementIdentityKey{device, inode}, nil
}

func retirementObjectIdentities(target string) (map[retirementIdentityKey]bool, error) {
	identities := make(map[retirementIdentityKey]bool)
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return identities, nil // Prior partial Git retirement, with retained marker/ref.
	} else if err != nil {
		return nil, err
	}
	err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info() // Lstat semantics: never follow shared-cache symlinks.
		if err != nil {
			return err
		}
		key, err := retirementObjectIdentity(info)
		if err != nil {
			return err
		}
		identities[key] = true
		return nil
	})
	return identities, err
}

func retirementMountWithin(target, source string) bool {
	rel, err := filepath.Rel(target, source)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func retirementMountTarget(target string) (string, error) {
	resolved, err := filepath.EvalSymlinks(target)
	if err == nil {
		return resolved, nil
	}
	// A previous partial retirement may already have removed the worktree while
	// retaining its marker/ref. Resolve the existing parent for branch-only retry;
	// an existing dangling symlink or other resolution failure remains refused.
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) || !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(target)), nil
}
