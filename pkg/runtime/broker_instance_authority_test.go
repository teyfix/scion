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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authorityFixtureFile(t *testing.T, authority BrokerInstanceAuthority) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instance.json")
	writeAuthorityFixture(t, path, authority)
	return path
}

func writeAuthorityFixture(t *testing.T, path string, authority BrokerInstanceAuthority) {
	t.Helper()
	data, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the directory entry to model the actual launcher refresh contract.
	if err := os.WriteFile(path+".new", data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func authorityFixture(t *testing.T, path string, authority BrokerInstanceAuthority, selected, daemon string, running, readonly bool, source string, mounted ...string) string {
	t.Helper()
	id := authority.ContainerID
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	guardFacts, _ := json.Marshal(map[string]any{"id": id, "mounts": []map[string]string{{"Source": source, "Destination": "/held-workspace", "Type": "bind"}}})
	observation := retirementFixtureObservedIdentity(t, info)
	if len(mounted) > 0 {
		observation = mounted[0]
	}
	base := retirementMountedFixture(t, "printf '%s\\n' '"+id+"'", "printf '%s\\n' '"+string(guardFacts)+"'", "printf '%s\\n' '"+observation+"'")
	baseScript, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte(strings.ReplaceAll(string(baseScript), strings.Repeat("a", 64), id)), 0755); err != nil {
		t.Fatal(err)
	}
	facts, _ := json.Marshal(map[string]any{"id": id, "config": map[string]any{"Hostname": authority.Hostname, "Labels": map[string]string{"com.docker.compose.project": authority.ComposeProject, "com.docker.compose.service": authority.ComposeService}}, "state": map[string]bool{"Running": running}, "mounts": []map[string]any{{"Destination": filepath.Dir(path), "Type": "bind", "RW": !readonly}}})
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\ncase \"$1\" in\ninfo) [ \"$2\" = '--format' ] && [ \"$3\" = '{{.ID}}' ] || exit 90; printf '%s\\n' " + quote(daemon) + "; exit;;\nps) if [ \"$4\" = '--filter' ]; then [ \"$5\" = " + quote("label=com.docker.compose.project="+authority.ComposeProject) + " ] && [ \"$7\" = " + quote("label=com.docker.compose.service="+authority.ComposeService) + " ] || exit 91; printf '%s\\n' " + quote(selected) + "; exit; fi;;\ninspect) case \"$3\" in *config*) [ \"$4\" = " + quote(id) + " ] || exit 92; printf '%s\\n' " + quote(string(facts)) + "; exit;; esac;;\nesac\nexec " + quote(base) + " \"$@\"\n"
	command := filepath.Join(t.TempDir(), "authority-runtime-fixture")
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return command
}

func validAuthorityFixture() BrokerInstanceAuthority {
	return BrokerInstanceAuthority{Version: 1, BrokerID: "broker-authenticated", Runtime: "docker", DaemonID: "daemon-selected", ContainerID: strings.Repeat("a", 64), ComposeProject: "animatrix", ComposeService: "scion-broker", Hostname: "manga"}
}

func TestWorkspaceRetirement_TrustedBrokerInstance(t *testing.T) {
	for _, name := range []string{"current", "missing", "stale", "wrong-daemon", "wrong-broker", "ambiguous", "stopped", "writable-directory-mount", "direct-target", "child-target", "foreign-parent", "hostname-mismatch", "held-child", "mounted-source-mismatch", "direct-file-bind", "nested-file-bind"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "worktrees", "owned")
			if err := os.MkdirAll(filepath.Join(target, "child"), 0755); err != nil {
				t.Fatal(err)
			}
			authority := validAuthorityFixture()
			path := authorityFixtureFile(t, authority)
			selected, daemon, broker := authority.ContainerID, authority.DaemonID, authority.BrokerID
			running, readonly, source := true, true, root
			switch name {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "stale":
				selected = strings.Repeat("b", 64)
			case "wrong-daemon":
				daemon = "different-daemon"
			case "wrong-broker":
				broker = "arbitrary-agent-claim"
			case "ambiguous":
				selected += "\n" + strings.Repeat("b", 64)
			case "stopped":
				running = false
			case "writable-directory-mount":
				readonly = false
			case "direct-target":
				source = target
			case "child-target":
				source = filepath.Join(target, "child")
			case "hostname-mismatch":
				authority.Hostname = "other-instance"
			}
			mounted := []string{}
			if name == "held-child" {
				info, err := os.Stat(filepath.Join(target, "child"))
				if err != nil {
					t.Fatal(err)
				}
				mounted = append(mounted, retirementFixtureObservedIdentity(t, info))
			}
			if name == "mounted-source-mismatch" {
				mounted = append(mounted, "0:0")
			}
			command := authorityFixture(t, path, authority, selected, daemon, running, readonly, source, mounted...)
			if name == "direct-file-bind" || name == "nested-file-bind" {
				data, err := os.ReadFile(command)
				if err != nil {
					t.Fatal(err)
				}
				old := `"Destination":"` + filepath.Dir(path) + `","RW":false,"Type":"bind"`
				replacement := `"Destination":"` + path + `","RW":false,"Type":"bind"`
				if name == "nested-file-bind" {
					replacement = old + `},{` + replacement
				}
				if !strings.Contains(string(data), old) {
					t.Fatal("directory bind fixture was not found")
				}
				if err := os.WriteFile(command, []byte(strings.Replace(string(data), old, replacement, 1)), 0755); err != nil {
					t.Fatal(err)
				}
			}
			rt := &DockerRuntime{Command: command}
			rt.ConfigureBrokerInstanceAuthority(path, func() (string, error) { return broker, nil })
			if name == "foreign-parent" {
				rt.ConfigureBrokerInstanceAuthority("", nil)
			}
			err := rt.AssertWorkspaceUnused(context.Background(), target)
			if (err != nil) != (name != "current") {
				t.Fatalf("expected only trusted current parent succeeds: %v", err)
			}
		})
	}
}

func TestWorkspaceRetirement_AuthorityRefresh(t *testing.T) {
	authority := validAuthorityFixture()
	path := authorityFixtureFile(t, authority)
	root := t.TempDir()
	target := filepath.Join(root, "worktrees", "owned")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	rt := &DockerRuntime{Command: authorityFixture(t, path, authority, authority.ContainerID, authority.DaemonID, true, true, root)}
	rt.ConfigureBrokerInstanceAuthority(path, func() (string, error) { return authority.BrokerID, nil })
	if err := rt.AssertWorkspaceUnused(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	old := authority
	authority.ContainerID = strings.Repeat("b", 64)
	rt.Command = authorityFixture(t, path, authority, authority.ContainerID, authority.DaemonID, true, true, root)
	if err := rt.AssertWorkspaceUnused(context.Background(), target); err == nil {
		t.Fatal("old instance authority accepted after recreation")
	}
	writeAuthorityFixture(t, path, authority)
	// Existing runtime sees atomic refresh, retaining stable authenticated ID.
	if err := rt.AssertWorkspaceUnused(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	writeAuthorityFixture(t, path, old)
	if err := rt.AssertWorkspaceUnused(context.Background(), target); err == nil {
		t.Fatal("stale replacement accepted")
	}
}

func TestWorkspaceRetirement_ProtectedAuthorityInput(t *testing.T) {
	for _, name := range []string{"symlink", "writable-file", "writable-directory", "invalid-version", "invalid-runtime", "trailing-json", "unknown-field", "no-authenticated-broker"} {
		t.Run(name, func(t *testing.T) {
			authority := validAuthorityFixture()
			path := authorityFixtureFile(t, authority)
			switch name {
			case "symlink":
				original := path
				path += ".alias"
				if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			case "writable-file":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "writable-directory":
				if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
					t.Fatal(err)
				}
			case "invalid-version":
				authority.Version = 2
				writeAuthorityFixture(t, path, authority)
			case "invalid-runtime":
				authority.Runtime = "podman"
				writeAuthorityFixture(t, path, authority)
			case "trailing-json", "unknown-field":
				content := "{} {}"
				if name == "unknown-field" {
					content = `{"agentContainerId":"untrusted"}`
				}
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			rt := &DockerRuntime{Command: "must-not-run"}
			rt.ConfigureBrokerInstanceAuthority(path, func() (string, error) { return "", fmt.Errorf("unavailable") })
			if err := rt.AssertWorkspaceUnused(context.Background(), t.TempDir()); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
