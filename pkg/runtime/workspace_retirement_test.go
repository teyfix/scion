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
	"os"
	"path/filepath"
	"strings"
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
		{name: "broad-broker-parent", source: filepath.Dir(filepath.Dir(target))},
		{name: "other-agent", source: other},
		{name: "alias-target", source: filepath.Join(root, "alias-target"), fail: true},
		{name: "alias-child", source: filepath.Join(root, "alias-child"), fail: true},
		{name: "redirected-parent", source: filepath.Join(root, "alias-parent", "agent-uuid", "nested"), fail: true},
		{name: "resolved-broad-parent", source: filepath.Join(root, "alias-broker-parent")},
		{name: "unresolved-source", source: filepath.Join(root, "absent-source"), fail: true},
		{name: "unresolved-alias", source: filepath.Join(root, "alias-unresolved"), fail: true},
		{name: "enumeration-error", list: "exit 7", fail: true},
		{name: "inspection-error", inspect: "exit 8", fail: true},
		{name: "incomplete-inspection", inspect: "printf '%s\\n' '{}'", fail: true},
		{name: "missing-host-source", inspect: "printf '%s\\n' '{\"id\":\"" + id + "\",\"mounts\":[{}]}'", fail: true},
		{name: "missing-container", inspect: "exit 0", fail: true},
		{name: "invalid-container-id", list: "printf '%s\\n' 'not-a-container'", fail: true},
		{name: "already-retired-target", source: filepath.Dir(filepath.Dir(target)), removedTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.removedTarget {
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
			}
			mounts, err := json.Marshal(map[string]any{"id": id, "mounts": []map[string]string{{"Source": tc.source}}})
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
			// This fixture refuses filtered enumeration: unlabeled/auxiliary
			// containers must be included, and no environment data is requested.
			script := "#!/bin/sh\ncase \"$1\" in\nps) [ \"$2\" = '-a' ] && [ \"$3\" = '--no-trunc' ] && [ \"$4\" = '--format' ] && [ \"$5\" = '{{.ID}}' ] || exit 90\n" + list + ";;\ninspect) [ \"$2\" = '--format' ] && [ \"$4\" = '" + id + "' ] || exit 91\n" + inspect + ";;\n*) exit 92;;\nesac\n"
			command := filepath.Join(t.TempDir(), "container-runtime-fixture")
			if err := os.WriteFile(command, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			for _, guard := range []WorkspaceRetirementGuard{&DockerRuntime{Command: command}, &PodmanRuntime{Command: command}} {
				if err := guard.AssertWorkspaceUnused(context.Background(), target); (err != nil) != tc.fail {
					t.Fatalf("%T: fail=%v err=%v", guard, tc.fail, err)
				}
			}
		})
	}
}
