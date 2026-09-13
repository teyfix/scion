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
	"syscall"
	"testing"
)

func TestWorkspaceRetirement_AllContainerMountAuthority(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace", "worktrees", "agent-uuid")
	other := filepath.Join(filepath.Dir(target), "another-uuid")
	for _, path := range []string{filepath.Join(target, "nested"), other} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	aliases := map[string]string{
		"alias-target":        target,
		"alias-child":         filepath.Join(target, "nested"),
		"alias-parent":        filepath.Dir(target),
		"alias-broker-parent": filepath.Dir(filepath.Dir(target)),
		"alias-unresolved":    filepath.Join(root, "absent-source"),
	}
	for name, destination := range aliases {
		if err := os.Symlink(destination, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, source, list, inspect string
		fail                        bool
		removedTarget               bool
	}{
		{name: "foreign-exact", source: target, fail: true},
		{name: "foreign-child", source: filepath.Join(target, "nested"), fail: true},
		{name: "unverifiable-broker-parent", source: filepath.Dir(filepath.Dir(target)), fail: true},
		{name: "other-agent", source: other},
		{name: "alias-target", source: filepath.Join(root, "alias-target"), fail: true},
		{name: "alias-child", source: filepath.Join(root, "alias-child"), fail: true},
		{name: "redirected-parent", source: filepath.Join(root, "alias-parent", "agent-uuid", "nested"), fail: true},
		{name: "unverifiable-resolved-broad-parent", source: filepath.Join(root, "alias-broker-parent"), fail: true},
		{name: "unresolved-source", source: filepath.Join(root, "absent-source"), fail: true},
		{name: "unresolved-alias", source: filepath.Join(root, "alias-unresolved"), fail: true},
		{name: "enumeration-error", list: "exit 7", fail: true},
		{name: "inspection-error", inspect: "exit 8", fail: true},
		{name: "incomplete-inspection", inspect: "printf '%s\\n' '{}'", fail: true},
		{name: "missing-host-source", inspect: "printf '%s\\n' '{\"id\":\"" + id + "\",\"mounts\":[{}]}'", fail: true},
		{name: "missing-container", inspect: "exit 0", fail: true},
		{name: "invalid-container-id", list: "printf '%s\\n' 'not-a-container'", fail: true},
		{name: "already-retired-target", source: other, removedTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.removedTarget {
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
			}
			mounts, err := json.Marshal(map[string]any{"id": id, "mounts": []map[string]string{{"Source": tc.source, "Destination": "/held-workspace", "Type": "bind"}}})
			if err != nil {
				t.Fatal(err)
			}
			list, inspect := tc.list, tc.inspect
			if list == "" {
				list = "printf '%s\\n' '" + id + "'"
			}
			if inspect == "" {
				inspect = "printf '%s\\n' '" + string(mounts) + "'"
			}
			observation := "0:0"
			if info, err := os.Stat(tc.source); err == nil {
				observation = retirementFixtureObservedIdentity(t, info)
			}
			command := retirementMountedFixture(t, list, inspect, "printf '%s\\n' '"+observation+"'")
			for _, guard := range []WorkspaceRetirementGuard{&DockerRuntime{Command: command}, &PodmanRuntime{Command: command}} {
				if err := guard.AssertWorkspaceUnused(context.Background(), target); (err != nil) != tc.fail {
					t.Fatalf("%T: fail=%v err=%v", guard, tc.fail, err)
				}
			}
		})
	}
}

func retirementFixtureObservedIdentity(t *testing.T, info os.FileInfo) string {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("fixture requires native filesystem identity")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

func retirementMountedFixture(t *testing.T, list, inspect, observation string) string {
	t.Helper()
	id := strings.Repeat("a", 64)
	// The guard must enumerate unlabeled/auxiliary containers. Runtime.Exec's
	// existing name-resolution list observes that same foreign container too.
	script := "#!/bin/sh\ncase \"$1\" in\nps) [ \"$2\" = '-a' ] && [ \"$3\" = '--no-trunc' ] && [ \"$4\" = '--format' ] || exit 90\nif [ \"$5\" = '{{json .}}' ]; then printf '%s\\n' '{\"ID\":\"" + id + "\",\"Names\":\"foreign-helper\",\"Labels\":\"\",\"Status\":\"Up\"}'; elif [ \"$5\" = 'json' ]; then printf '%s\\n' '[{\"Id\":\"" + id + "\",\"Names\":[\"foreign-helper\"],\"Labels\":{},\"Status\":\"Up\"}]'; else [ \"$5\" = '{{.ID}}' ] || exit 90\n" + list + "; fi;;\ninspect) [ \"$2\" = '--format' ] && [ \"$4\" = '" + id + "' ] || exit 91\n" + inspect + ";;\nexec) [ \"$2\" = '--user' ] && [ \"$3\" = 'scion' ] && [ \"$4\" = '" + id + "' ] && [ \"$5\" = 'stat' ] && [ \"$6\" = '-L' ] && [ \"$7\" = '-c' ] && [ \"$8\" = '%d:%i' ] && [ \"$9\" = '--' ] && [ \"${10}\" = '/held-workspace' ] || exit 92\n" + observation + ";;\n*) exit 93;;\nesac\n"
	command := filepath.Join(t.TempDir(), "container-runtime-fixture")
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return command
}

func TestWorkspaceRetirement_EstablishedMountIdentity(t *testing.T) {
	for _, name := range []string{"retarget-root", "retarget-child", "rename-replace-candidate", "rename-replace-source", "unsupported-exec", "invalid-mounted-stat"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target, unrelated := filepath.Join(root, "worktrees", "uuid"), filepath.Join(root, "unrelated")
			for _, dir := range []string{filepath.Join(target, "child"), unrelated} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			original := target
			if name == "retarget-child" {
				original = filepath.Join(target, "child")
			}
			// This opened directory is a disposable retained-object proof. The
			// fake runtime reports its actual Fstat identity after current-path
			// retarget/replacement; no live container mount is claimed.
			held, err := os.Open(original)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			alias := filepath.Join(root, "foreign-source")
			if err := os.Symlink(original, alias); err != nil {
				t.Fatal(err)
			}
			if name == "rename-replace-candidate" {
				if err := os.Rename(target, filepath.Join(root, "held-original")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(target, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if name == "rename-replace-source" {
				if err := os.Rename(alias, filepath.Join(root, "retained-source-alias")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(alias, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(unrelated, alias); err != nil {
					t.Fatal(err)
				}
			}
			info, err := held.Stat()
			if err != nil {
				t.Fatal(err)
			}
			observation := "printf '%s\\n' '" + retirementFixtureObservedIdentity(t, info) + "'"
			want := "actual worktree object"
			if name == "rename-replace-candidate" {
				want = "differs from current source"
			} else if name == "unsupported-exec" {
				observation, want = "exit 126", "exec/stat user, tool or permission capability"
			} else if name == "invalid-mounted-stat" {
				observation, want = "printf '%s\\n' 'not-an-object-id'", "ownership unverifiable"
			}
			id := strings.Repeat("a", 64)
			facts, err := json.Marshal(map[string]any{"id": id, "mounts": []map[string]string{{"Source": alias, "Destination": "/held-workspace", "Type": "bind"}}})
			if err != nil {
				t.Fatal(err)
			}
			command := retirementMountedFixture(t, "printf '%s\\n' '"+id+"'", "printf '%s\\n' '"+string(facts)+"'", observation)
			for _, guard := range []WorkspaceRetirementGuard{&DockerRuntime{Command: command}, &PodmanRuntime{Command: command}} {
				if err := guard.AssertWorkspaceUnused(context.Background(), target); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%T must refuse held-object/current-path ambiguity: %v", guard, err)
				}
			}
		})
	}
}
