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
	target := filepath.Join(t.TempDir(), "workspace", "worktrees", "agent-uuid")
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, source, list, inspect string
		fail                        bool
	}{
		{name: "foreign-exact", source: target, fail: true},
		{name: "foreign-child", source: filepath.Join(target, "nested"), fail: true},
		{name: "broad-broker-parent", source: filepath.Dir(filepath.Dir(target))},
		{name: "other-agent", source: filepath.Join(filepath.Dir(target), "another-uuid")},
		{name: "enumeration-error", list: "exit 7", fail: true},
		{name: "inspection-error", inspect: "exit 8", fail: true},
		{name: "incomplete-inspection", inspect: "printf '%s\\n' '{}'", fail: true},
		{name: "missing-host-source", inspect: "printf '%s\\n' '{\"id\":\"" + id + "\",\"mounts\":[{}]}'", fail: true},
		{name: "missing-container", inspect: "exit 0", fail: true},
		{name: "invalid-container-id", list: "printf '%s\\n' 'not-a-container'", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
