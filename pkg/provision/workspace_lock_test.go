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

package provision

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestWorkspaceLock_ContendsAcrossProcesses(t *testing.T) {
	base := filepath.Join(t.TempDir(), "workspace")
	release, err := LockWorkspace(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkspaceLock_HelperProcess$")
	cmd.Env = append(os.Environ(), "SCION_TEST_WORKSPACE_LOCK="+base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("other process must contend on the same advisory inode: %v: %s", err, out)
	}
}

func TestWorkspaceLock_NFSRetainsStoreAuthority(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ProvisionSentinelFile), []byte("ready"), 0644); err != nil {
		t.Fatal(err)
	}
	// A sibling native lock is deliberately inaccessible. NFS init containers
	// may mount only the workspace; their existing Postgres authority must win.
	if err := os.Symlink(filepath.Join(parent, "missing"), filepath.Join(parent, ".scion-workspace-provision-workspace.lock")); err != nil {
		t.Fatal(err)
	}
	locker := newTestLocker()
	if err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: base, Backend: "nfs"},
		ProjectID: "nfs-project", Mode: store.SharingModeSharedPlain,
		SentinelDir: base, Locker: locker,
	}); err != nil {
		t.Fatal(err)
	}
	if locker.acquires != 1 {
		t.Fatalf("NFS store advisory acquisition changed: %d", locker.acquires)
	}
}

func TestWorkspaceLock_HelperProcess(t *testing.T) {
	base := os.Getenv("SCION_TEST_WORKSPACE_LOCK")
	if base == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release, err := LockWorkspace(ctx, base)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent lock was not respected: %v", err)
	}
}

func TestWorkspaceLock_RejectsRedirectedLock(t *testing.T) {
	parent := t.TempDir()
	retained := filepath.Join(parent, "retained")
	if err := os.WriteFile(retained, []byte("preserved source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retained, filepath.Join(parent, ".scion-workspace-provision-workspace.lock")); err != nil {
		t.Fatal(err)
	}
	if release, err := LockWorkspace(context.Background(), filepath.Join(parent, "workspace")); err == nil {
		release()
		t.Fatal("redirected lock should fail closed")
	}
	if bytes, err := os.ReadFile(retained); err != nil || string(bytes) != "preserved source" {
		t.Fatalf("lock touched redirected source: %s %v", bytes, err)
	}
}
