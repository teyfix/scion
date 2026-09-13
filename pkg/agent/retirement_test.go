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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type retirementFixture struct {
	project, base, target, agentDir, uuid, branch, survivor, globalSentinel, gitDir string
}

func retirementTestGit(t *testing.T, base string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", base}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func setupRetirement(t *testing.T, linked ...bool) retirementFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HOST_UID", "1000") // Native retirement must not depend on broad prune.
	f := retirementFixture{project: filepath.Join(home, "project"), uuid: "16ecac0d-ba1b-4936-b231-efc5e4dec178", branch: "feature/native-retirement"}
	f.base = filepath.Join(f.project, "workspace")
	f.target = provision.WorktreePath(f.base, f.uuid)
	f.survivor = provision.WorktreePath(f.base, "surviving-uuid")
	f.agentDir = filepath.Join(f.project, ".scion", "agents", "display-slug")
	for _, dir := range []string{f.base, f.agentDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if len(linked) > 0 && linked[0] {
		commonBase := t.TempDir()
		setupGitRepo(t, commonBase)
		retirementTestGit(t, commonBase, "worktree", "add", "-b", "shared-base", f.base)
	} else {
		setupGitRepo(t, f.base)
	}
	f.gitDir = strings.TrimSpace(retirementTestGit(t, f.base, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if err := os.MkdirAll(filepath.Join(f.gitDir, "info"), 0755); err != nil {
		t.Fatal(err)
	}
	retirementTestGit(t, f.base, "branch", "-M", "main")
	retirementTestGit(t, f.base, "branch", "display-slug")
	retirementTestGit(t, f.base, "worktree", "add", "--relative-paths", "-b", f.branch, f.target)
	retirementTestGit(t, f.base, "worktree", "add", "--relative-paths", "-b", "survivor", f.survivor)
	if err := provision.RegisterSharer(f.base, f.branch, f.target, f.uuid); err != nil {
		t.Fatal(err)
	}
	if err := provision.RegisterSharer(f.base, "survivor", f.survivor, "surviving-uuid"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.agentDir, "scion-agent.json"), []byte(`{"env":{"SCION_AGENT_ID":"`+f.uuid+`"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	// The same display slug elsewhere is unrelated to this project.
	f.globalSentinel = filepath.Join(home, ".scion", "agents", "display-slug", "retained")
	if err := os.MkdirAll(filepath.Dir(f.globalSentinel), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.globalSentinel, []byte("unrelated"), 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func assertRetirementSurvivors(t *testing.T, f retirementFixture) {
	t.Helper()
	for _, path := range []string{f.base, f.gitDir, f.survivor, f.globalSentinel} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated artifact %s: %v", path, err)
		}
	}
	if out := retirementTestGit(t, f.base, "branch", "--list", "display-slug"); strings.TrimSpace(out) == "" {
		t.Error("unowned display-slug branch was deleted")
	}
	if _, _, found, err := provision.FindBranchForAgent(f.base, "surviving-uuid"); err != nil || !found {
		t.Errorf("survivor ownership: found=%v err=%v", found, err)
	}
}

func TestNativeRetirement_UUIDSymlinkAndFlags(t *testing.T) {
	for _, removeBranch := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep-branch", true: "remove-branch"}[removeBranch], func(t *testing.T) {
			f := setupRetirement(t)
			modelTarget := t.TempDir()
			model := filepath.Join(modelTarget, "model.bin")
			if err := os.WriteFile(model, []byte("shared immutable model"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.gitDir, "info", "exclude"), []byte(".volumes/\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(f.target, ".volumes", "transcription"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(modelTarget, filepath.Join(f.target, ".volumes", "transcription", "models")); err != nil {
				t.Fatal(err)
			}
			deleted, err := DeleteAgentFiles("display-slug", f.project, removeBranch)
			if err != nil || deleted != removeBranch {
				t.Fatalf("delete: branchDeleted=%v err=%v", deleted, err)
			}
			for _, path := range []string{f.target, f.agentDir} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("owned path remains %s: %v", path, err)
				}
			}
			if strings.Contains(listWorktrees(t, f.base), f.target) {
				t.Error("exact Git registration remains")
			}
			if _, _, found, err := provision.FindBranchForAgent(f.base, f.uuid); err != nil || found {
				t.Errorf("UUID marker remains: %v %v", found, err)
			}
			branch := strings.TrimSpace(retirementTestGit(t, f.base, "branch", "--list", f.branch))
			if (branch == "") != removeBranch {
				t.Errorf("branch flag was not respected: %q", branch)
			}
			if data, err := os.ReadFile(model); err != nil || string(data) != "shared immutable model" {
				t.Errorf("shared model changed: %q %v", data, err)
			}
			if _, err := DeleteAgentFiles("display-slug", f.project, removeBranch); err != nil {
				t.Fatalf("repeat delete: %v", err)
			}
			assertRetirementSurvivors(t, f)
		})
	}
}

func TestNativeRetirement_RefusesRecoverableOrMismatchedArtifacts(t *testing.T) {
	for _, failure := range []string{"dirty", "ignored-model", "locked", "mismatched-branch", "escaped-path", "redirected-parent", "invalid-uuid"} {
		t.Run(failure, func(t *testing.T) {
			f := setupRetirement(t)
			switch failure {
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.target, "unpublished.txt"), []byte("recover me"), 0644); err != nil {
					t.Fatal(err)
				}
			case "ignored-model":
				if err := os.WriteFile(filepath.Join(f.gitDir, "info", "exclude"), []byte("model.bin\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.target, "model.bin"), []byte("retain cached model"), 0644); err != nil {
					t.Fatal(err)
				}
			case "locked":
				retirementTestGit(t, f.base, "worktree", "lock", f.target)
			case "mismatched-branch":
				retirementTestGit(t, f.target, "checkout", "-b", "unrelated-branch")
			case "escaped-path":
				if err := provision.RegisterSharer(f.base, f.branch, f.survivor+"/../..", f.uuid); err != nil {
					t.Fatal(err)
				}
			case "redirected-parent":
				parent := filepath.Dir(f.target)
				if err := os.Rename(parent, parent+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-retained", parent); err != nil {
					t.Fatal(err)
				}
			case "invalid-uuid":
				if err := os.WriteFile(filepath.Join(f.agentDir, "scion-agent.json"), []byte(`{"env":{"SCION_AGENT_ID":"../../escape"}}`), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DeleteAgentFiles("display-slug", f.project, true); err == nil {
				t.Fatal("unsafe retirement succeeded")
			}
			for _, path := range []string{f.target, f.agentDir} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("failure lost recoverable artifact %s: %v", path, err)
				}
			}
			if _, _, found, err := provision.FindBranchForAgent(f.base, f.uuid); err != nil || !found {
				t.Errorf("failure lost retry identity: %v %v", found, err)
			}
			assertRetirementSurvivors(t, f)
		})
	}
}

func TestNativeRetirement_PartialBranchFailureRetainsIdentityForRetry(t *testing.T) {
	f := setupRetirement(t)
	// A reference lock injects failure after exact worktree removal.
	lock := filepath.Join(f.gitDir, "refs", "heads", f.branch+".lock")
	if err := os.WriteFile(lock, []byte("injected branch lock"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteAgentFiles("display-slug", f.project, true); err == nil {
		t.Fatal("injected partial failure was swallowed")
	}
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Errorf("worktree was not retired before branch failure: %v", err)
	}
	if _, err := os.Stat(f.agentDir); err != nil {
		t.Errorf("persisted UUID was lost: %v", err)
	}
	if _, _, found, err := provision.FindBranchForAgent(f.base, f.uuid); err != nil || !found {
		t.Errorf("retry marker lost: %v %v", found, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	deleted, err := DeleteAgentFiles("display-slug", f.project, true)
	if err != nil || !deleted {
		t.Fatalf("retry did not finish: %v %v", deleted, err)
	}
	assertRetirementSurvivors(t, f)
}

func TestNativeRetirement_UUIDJoinerPreservesSharedWorktreeUntilLastRetirement(t *testing.T) {
	f := setupRetirement(t)
	joinerUUID := "e64e61de-d4d6-471b-ae85-58af234ab9ce"
	joinerDir := filepath.Join(f.project, ".scion", "agents", "joiner-slug")
	if err := os.MkdirAll(joinerDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(joinerDir, "scion-agent.yaml"), []byte("env:\n  SCION_AGENT_ID: "+joinerUUID+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := provision.RegisterSharer(f.base, f.branch, f.target, joinerUUID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := DeleteAgentFiles("display-slug", f.project, true); err != nil || deleted {
		t.Fatalf("creator deletion: %v %v", deleted, err)
	}
	if _, err := os.Stat(f.target); err != nil {
		t.Fatalf("creator deletion lost active joiner's worktree: %v", err)
	}
	sharers, target, err := provision.ListSharers(f.base, f.branch)
	if err != nil || len(sharers) != 1 || sharers[0] != joinerUUID || target != f.target {
		t.Fatalf("joiner ownership lost: %v %q %v", sharers, target, err)
	}
	// Recreate only the creator's private identity as if its directory cleanup
	// failed after detach. Retry must preserve the joiner's UUID-owned worktree.
	if err := os.MkdirAll(f.agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.agentDir, "scion-agent.json"), []byte(`{"env":{"SCION_AGENT_ID":"`+f.uuid+`"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if deleted, err := DeleteAgentFiles("display-slug", f.project, true); err != nil || deleted {
		t.Fatalf("creator detach retry: %v %v", deleted, err)
	}
	if deleted, err := DeleteAgentFiles("joiner-slug", f.project, true); err != nil || !deleted {
		t.Fatalf("last UUID joiner deletion: %v %v", deleted, err)
	}
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Errorf("last joiner left worktree: %v", err)
	}
	assertRetirementSurvivors(t, f)
}

func TestNativeRetirement_DeleteFilesFalsePreservesFilesystemAndBranch(t *testing.T) {
	f := setupRetirement(t)
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{}}
	deleted, err := mgr.Delete(context.Background(), "display-slug", false, f.project, false)
	if err != nil || deleted {
		t.Fatalf("preserving delete: %v %v", deleted, err)
	}
	for _, path := range []string{f.target, f.agentDir} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("deleteFiles=false lost %s: %v", path, err)
		}
	}
	if _, _, found, err := provision.FindBranchForAgent(f.base, f.uuid); err != nil || !found {
		t.Errorf("deleteFiles=false lost identity: %v %v", found, err)
	}
	assertRetirementSurvivors(t, f)
}

func TestNativeRetirement_RuntimeEnumerationFailurePreservesArtifacts(t *testing.T) {
	f := setupRetirement(t)
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, fmt.Errorf("injected runtime enumeration failure")
	}}}
	if _, err := mgr.Delete(context.Background(), "display-slug", true, f.project, true); err == nil {
		t.Fatal("runtime enumeration failure was swallowed")
	}
	for _, path := range []string{f.target, f.agentDir} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("runtime uncertainty lost %s: %v", path, err)
		}
	}
	assertRetirementSurvivors(t, f)
}

func TestNativeRetirement_LinkedBaseRetiresOnlySelectedArtifacts(t *testing.T) {
	f := setupRetirement(t, true)
	if deleted, err := DeleteAgentFiles("display-slug", f.project, true); err != nil || !deleted {
		t.Fatalf("linked base retirement: %v %v", deleted, err)
	}
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Errorf("linked base retained target worktree: %v", err)
	}
	if strings.Contains(listWorktrees(t, f.base), f.target) {
		t.Error("linked base retained target Git registration")
	}
	if _, _, found, err := provision.FindBranchForAgent(f.base, f.uuid); err != nil || found {
		t.Errorf("linked base retained target ownership: %v %v", found, err)
	}
	assertRetirementSurvivors(t, f)
}

func TestNativeRetirement_LegacyLocalSlugRegistrationWithPersistedUUID(t *testing.T) {
	project := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_HOST_UID", "1000")
	setupGitRepo(t, project)
	agentDir := filepath.Join(project, ".scion", "agents", "local-slug")
	workspace := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	retirementTestGit(t, project, "worktree", "add", "-b", "local-slug", workspace)
	if err := provision.RegisterSharer(project, "local-slug", workspace, "local-slug"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"env":{"SCION_AGENT_ID":"16ecac0d-ba1b-4936-b231-efc5e4dec178"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := provision.RegisterSharer(project, "local-slug", workspace, "joiner-slug"); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteAgentFiles("local-slug", project, true); err == nil {
		t.Fatal("private directory retirement would delete active joiner's workspace")
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("private shared workspace was lost: %v", err)
	}
	if sharers, _, err := provision.ListSharers(project, "local-slug"); err != nil || len(sharers) != 2 {
		t.Fatalf("failed retirement lost retry ownership: %v %v", sharers, err)
	}
	if _, _, err := provision.UnregisterSharer(project, "local-slug", "joiner-slug"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := DeleteAgentFiles("local-slug", project, true); err != nil || !deleted {
		t.Fatalf("legacy slug registration retirement: %v %v", deleted, err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("legacy agent directory remains: %v", err)
	}
	if strings.Contains(listWorktrees(t, project), workspace) {
		t.Error("legacy UUID configuration left Git registration")
	}
}
