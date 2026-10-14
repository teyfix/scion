// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func stubAgentStateBind(t *testing.T, verified bool) {
	t.Helper()
	original := isAgentLocalStateBind
	isAgentLocalStateBind = func(target, source string) bool {
		if filepath.Base(target) != ".state" || source != "/var/lib/scion-agent-state/workspace" {
			t.Fatalf("unexpected state observation: %q, %q", target, source)
		}
		return verified
	}
	t.Cleanup(func() { isAgentLocalStateBind = original })
}

func TestIsWorkspaceEmptyAgentLocalState(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		verified, stateContent, symlink bool
		other                           string
		want                            bool
	}{
		{name: "verified empty bind", verified: true, want: true},
		{name: "ordinary empty state"},
		{name: "ordinary populated state", stateContent: true},
		{name: "verified populated state", verified: true, stateContent: true},
		{name: "symlink", verified: true, symlink: true},
		{name: "retained tracked file", verified: true, other: "README.md"},
		{name: "retained untracked file", verified: true, other: "unique-untracked.txt"},
		{name: "existing repository", verified: true, other: ".git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubAgentStateBind(t, tc.verified)
			workspace := t.TempDir()
			state := filepath.Join(workspace, ".state")
			if tc.symlink {
				if err := os.Symlink(t.TempDir(), state); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(state, 0755); err != nil {
				t.Fatal(err)
			}
			if tc.stateContent {
				if err := os.WriteFile(filepath.Join(state, "sentinel"), []byte("retained"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.other != "" {
				if err := os.WriteFile(filepath.Join(workspace, tc.other), []byte("retained"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := isWorkspaceEmpty(workspace); got != tc.want {
				t.Fatalf("empty=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestGitCloneWorkspaceWithAgentLocalState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	origin := t.TempDir()
	runGitForTest(t, origin, "init", "-b", "main")
	runGitForTest(t, origin, "config", "user.email", "test@example.com")
	runGitForTest(t, origin, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("native source\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, origin, "add", "README.md")
	runGitForTest(t, origin, "commit", "-m", "source")
	command := exec.Command("git", "-C", origin, "rev-parse", "HEAD")
	command.Env = filterHubEnv(os.Environ())
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"clone", "late failure", "retained content"} {
		t.Run(mode, func(t *testing.T) {
			scrubHubEnv(t)
			stubAgentStateBind(t, true)
			workspace := t.TempDir()
			state := filepath.Join(workspace, ".state")
			if err := os.Mkdir(state, 0755); err != nil {
				t.Fatal(err)
			}
			stateBefore, err := os.Stat(state)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCION_GIT_CLONE_URL", "file://"+origin)
			t.Setenv("SCION_GIT_BRANCH", "main")
			t.Setenv("SCION_GIT_DEPTH", "1")
			t.Setenv("SCION_AGENT_NAME", "native-state-proof")
			t.Setenv("SCION_AGENT_BRANCH", "native-state-proof")
			t.Setenv("SCION_WORKSPACE_PATH", workspace)
			t.Setenv("GITHUB_TOKEN", "")
			agentHome := t.TempDir()
			if mode == "late failure" {
				agentHome = filepath.Join(t.TempDir(), "missing", "home")
			}
			if mode == "retained content" {
				if err := os.WriteFile(filepath.Join(workspace, "unique.txt"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("SCION_GIT_CLONE_URL", "file:///nonexistent/clone-must-not-run")
			}
			err = gitCloneWorkspace(0, 0, agentHome, false)
			stateAfter, statErr := os.Stat(state)
			if statErr != nil || !os.SameFile(stateBefore, stateAfter) {
				t.Fatalf("state mountpoint replaced or removed: %v", statErr)
			}
			switch mode {
			case "clone":
				if err != nil {
					t.Fatal(err)
				}
				for _, check := range []struct {
					args []string
					want string
				}{
					{[]string{"rev-parse", "HEAD"}, strings.TrimSpace(string(head))},
					{[]string{"remote", "get-url", "origin"}, "file://" + origin},
					{[]string{"branch", "--show-current"}, "native-state-proof"},
				} {
					command := exec.Command("git", append([]string{"-C", workspace}, check.args...)...)
					command.Env = filterHubEnv(os.Environ())
					got, err := command.Output()
					if err != nil || strings.TrimSpace(string(got)) != check.want {
						t.Fatalf("git %v=%q, %v; want %q", check.args, got, err, check.want)
					}
				}
				content, err := os.ReadFile(filepath.Join(workspace, "README.md"))
				if err != nil || string(content) != "native source\n" {
					t.Fatalf("clone content=%q, %v", content, err)
				}
			case "late failure":
				if err == nil || !strings.Contains(err.Error(), "credential helper") {
					t.Fatalf("wanted native late failure, got %v", err)
				}
				entries, readErr := os.ReadDir(workspace)
				if readErr != nil || len(entries) != 1 || entries[0].Name() != ".state" {
					t.Fatalf("failure cleanup lost state or retained clone data: %v, %v", entries, readErr)
				}
			case "retained content":
				content, readErr := os.ReadFile(filepath.Join(workspace, "unique.txt"))
				if err != nil || readErr != nil || string(content) != "preserve" {
					t.Fatalf("retained content changed: %q, %v, %v", content, err, readErr)
				}
				if _, statErr := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(statErr) {
					t.Fatal("clone ran against retained workspace")
				}
			}
		})
	}
}
