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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func validRetirementID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\n\r")
}

// Broker provisioning persists its Hub UUID independently of the agent slug.
func retirementIdentity(slug string, dirs []string) (string, error) {
	identity := ""
	for _, dir := range dirs {
		if _, err := os.Lstat(dir); err == nil {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", err
			}
			if resolved != dir {
				return "", fmt.Errorf("delete: agent directory is redirected: %s", dir)
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		cfg, err := (&config.Template{Path: dir}).LoadConfig()
		if err != nil {
			return "", fmt.Errorf("delete: load persisted identity: %w", err)
		}
		id := cfg.Env["SCION_AGENT_ID"]
		if id == "" {
			continue
		}
		if !validRetirementID(id) || (identity != "" && identity != id) {
			return "", fmt.Errorf("delete: invalid or conflicting persisted agent identity")
		}
		identity = id
	}
	if identity == "" {
		identity = slug // Older local agents register by name.
	}
	return identity, nil
}

func retirementGit(base string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", base}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("delete: git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func validateRetirementPath(base, target string, agentDirs []string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("delete: invalid registered worktree path %q", target)
	}
	allowed := filepath.Dir(target) == filepath.Join(base, "worktrees") && validRetirementID(filepath.Base(target))
	for _, dir := range agentDirs {
		allowed = allowed || target == filepath.Join(dir, "workspace")
	}
	if !allowed {
		return fmt.Errorf("delete: registered worktree escapes agent storage: %s", target)
	}
	// Resolve each existing ancestor without following the selected worktree.
	// A redirected parent must not turn an owned lexical path into other storage.
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	if parent != filepath.Dir(target) {
		return fmt.Errorf("delete: registered worktree parent is redirected: %s", target)
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("delete: registered worktree is a symlink: %s", target)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func retirementWorktreeState(base, target, branch string) (bool, error) {
	out, err := retirementGit(base, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	for _, record := range strings.Split(out, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		if len(fields) == 0 || fields[0] != "worktree "+target {
			continue
		}
		matchedBranch := false
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "locked") {
				return false, fmt.Errorf("delete: registered worktree is locked: %s", target)
			}
			matchedBranch = matchedBranch || field == "branch refs/heads/"+branch
		}
		if !matchedBranch {
			return false, fmt.Errorf("delete: worktree branch does not match ownership marker: %s", target)
		}
		return true, nil
	}
	return false, nil
}

// Ignored model/dependency bytes are not automatically disposable. Permit
// empty directories and symlinks only; WalkDir never follows cache symlinks.
func validateRetirementContents(target string) error {
	out, err := retirementGit(target, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "!! ") {
			return fmt.Errorf("delete: worktree contains recoverable changes: %s", target)
		}
		// Git quotes unusual paths. Fail closed instead of parsing an ambiguous
		// deletion target; ordinary provisioned UUID worktrees do not need them.
		name := strings.TrimSuffix(strings.TrimPrefix(line, "!! "), "/")
		if strings.HasPrefix(name, "\"") || !filepath.IsLocal(name) {
			return fmt.Errorf("delete: ambiguous ignored path: %s", line)
		}
		if err := filepath.WalkDir(filepath.Join(target, name), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				return fmt.Errorf("delete: ignored bytes need recovery before retirement: %s", path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func retireRegisteredWorktree(ctx context.Context, rt runtime.Runtime, base, target, branch string, removeBranch bool, agentDirs []string) (bool, error) {
	if err := validateRetirementPath(base, target, agentDirs); err != nil {
		return false, err
	}
	if strings.HasPrefix(branch, "-") || branch == "" {
		return false, fmt.Errorf("delete: invalid owned branch")
	}
	if _, err := retirementGit(base, "check-ref-format", "--branch", branch); err != nil {
		return false, err
	}
	current, err := retirementGit(base, "branch", "--show-current")
	if err != nil {
		return false, err
	}
	if current == branch {
		return false, fmt.Errorf("delete: ownership marker refers to base branch %s", branch)
	}
	registered, err := retirementWorktreeState(base, target, branch)
	if err != nil {
		return false, err
	}
	_, statErr := os.Lstat(target)
	if statErr == nil {
		if !registered {
			return false, fmt.Errorf("delete: worktree is not registered in selected project: %s", target)
		}
		if err := validateRetirementContents(target); err != nil {
			return false, err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return false, statErr
	}
	guard, ok := rt.(runtime.WorkspaceRetirementGuard)
	if !ok {
		return false, fmt.Errorf("delete: runtime cannot establish worktree mount authority")
	}
	if err := guard.AssertWorkspaceUnused(ctx, target); err != nil {
		return false, fmt.Errorf("delete: worktree runtime inspection: %w", err)
	}
	if registered {
		// One force permits ignored symlink removal but does not override locks.
		// Native Git removes precisely this registration; never prune other agents.
		if _, err := retirementGit(base, "worktree", "remove", "--force", target); err != nil {
			return false, err
		}
	}
	if !removeBranch {
		return false, nil
	}
	cmd := exec.Command("git", "-C", base, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil // Earlier attempt already removed the branch.
		}
		return false, fmt.Errorf("delete: inspect owned branch: %w", err)
	}
	_, err = retirementGit(base, "branch", "-D", "--", branch)
	return err == nil, err
}

func retirementWorktreeHasOwners(base, target string, agentDirs []string) (bool, error) {
	if err := validateRetirementPath(base, target, agentDirs); err != nil {
		return false, err
	}
	branch, err := retirementGit(target, "branch", "--show-current")
	if err != nil {
		return false, err
	}
	owners, registeredPath, err := provision.ListSharers(base, branch)
	return len(owners) > 0 && registeredPath == target, err
}
