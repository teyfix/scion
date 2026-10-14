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

package fsutil

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal, representative mountinfo line for a bind mount of the host's
// /usr onto /workspace, in the format documented by proc(5):
// ID parentID major:minor root mountPoint options - fstype source superopts
const mountInfoBindUsrAtWorkspace = `26 25 0:22 /usr /workspace rw,relatime shared:1 - overlay overlay rw
27 25 0:22 / /home/scion rw,relatime shared:1 - overlay overlay rw
28 25 0:23 / /proc rw,relatime shared:1 - proc proc rw
`

func TestCheckMountSourceReader_RefusesCriticalBindSource(t *testing.T) {
	err := checkMountSourceReader("/workspace", strings.NewReader(mountInfoBindUsrAtWorkspace))
	if !errors.Is(err, ErrCriticalMountSource) {
		t.Fatalf("checkMountSourceReader(/workspace) = %v, want ErrCriticalMountSource", err)
	}
}

// TestCheckMountSourceReader_AllowsWholeRootBindSource proves a bind source
// of "/" is allowed here: dedicated volumes and disks legitimately report
// "/" as their source, and whole-root is already caught separately by
// CheckRoot's content heuristic.
func TestCheckMountSourceReader_AllowsWholeRootBindSource(t *testing.T) {
	err := checkMountSourceReader("/home/scion", strings.NewReader(mountInfoBindUsrAtWorkspace))
	if err != nil {
		t.Fatalf("checkMountSourceReader(/home/scion) = %v, want nil (bind source is \"/\")", err)
	}
}

func TestCheckMountSourceReader_NoOpWhenNotAMountPoint(t *testing.T) {
	err := checkMountSourceReader("/some/other/path", strings.NewReader(mountInfoBindUsrAtWorkspace))
	if err != nil {
		t.Fatalf("checkMountSourceReader(/some/other/path) = %v, want nil (not a mount point)", err)
	}
}

func TestCheckMountSourceReader_IgnoresMalformedLines(t *testing.T) {
	data := "not enough fields\n\n" + mountInfoBindUsrAtWorkspace
	err := checkMountSourceReader("/workspace", strings.NewReader(data))
	if !errors.Is(err, ErrCriticalMountSource) {
		t.Fatalf("checkMountSourceReader(/workspace) with leading malformed lines = %v, want ErrCriticalMountSource", err)
	}
}

// TestCheckMountSourceReader_FailsClosedOnScanError locks in the documented
// fail-closed behavior on CheckMountSource: a mount table that exists but
// can't be scanned (here, a single line past the scanner's default
// line-length limit) must return an error, not be treated the same as an
// absent table (which is a no-op). scanner.Err() surfaces such a failure
// after Scan() returns false without having reached a match.
func TestCheckMountSourceReader_FailsClosedOnScanError(t *testing.T) {
	overlong := strings.Repeat("x", bufio.MaxScanTokenSize+1)
	err := checkMountSourceReader("/workspace", strings.NewReader(overlong+"\n"))
	if err == nil {
		t.Fatalf("checkMountSourceReader with an over-long line = nil, want a non-nil scan error")
	}
	if errors.Is(err, ErrCriticalMountSource) {
		t.Fatalf("checkMountSourceReader with an over-long line = %v, want a scan error, not ErrCriticalMountSource", err)
	}
}

func TestCheckMountSourceReader_HandlesOctalEncodedPaths(t *testing.T) {
	// The kernel encodes space as \040 in mountinfo path fields.
	data := "26 25 0:22 /etc /work\\040space rw - overlay overlay rw\n"
	err := checkMountSourceReader("/work space", strings.NewReader(data))
	if !errors.Is(err, ErrCriticalMountSource) {
		t.Fatalf("checkMountSourceReader with an octal-encoded mount point = %v, want ErrCriticalMountSource", err)
	}
}

// TestCheckMountSource_NoOpForNonMountPoint exercises the real-file wrapper
// (not the test-supplied reader) against an ordinary, non-mounted root,
// proving it reads the real mount table and reports no match rather than
// erroring.
func TestCheckMountSource_NoOpForNonMountPoint(t *testing.T) {
	if err := CheckMountSource("/definitely/not/a/mount/point/for/this/test"); err != nil {
		t.Fatalf("CheckMountSource = %v, want nil", err)
	}
}

func TestRootFSBindMountEvidence(t *testing.T) {
	const source = "/var/lib/scion-agent-state/workspace"
	const root = "20 1 0:22 / / rw - overlay overlay rw\n"
	const state = "21 20 0:22 /var/lib/scion-agent-state/workspace /workspace/.state rw - overlay overlay rw\n"
	for _, tc := range []struct {
		name, target, document string
		want                   bool
	}{
		{"local bind", "/workspace/.state", root + state, true},
		{"escaped target", "/work space/.state", root + strings.ReplaceAll(state, "/workspace/", "/work\\040space/"), true},
		{"ordinary directory", "/workspace/.state", root, false},
		{"missing root", "/workspace/.state", state, false},
		{"wrong source", "/workspace/.state", root + strings.ReplaceAll(state, source, "/host/project/.state"), false},
		{"host device", "/workspace/.state", root + strings.ReplaceAll(state, "0:22", "8:1"), false},
		{"source beneath host bind", "/workspace/.state", root + state + "22 20 8:1 /host/state /var/lib/scion-agent-state rw - ext4 /dev/sda rw\n", false},
		{"source is separate mount", "/workspace/.state", root + state + "22 20 0:22 /other /var/lib/scion-agent-state/workspace rw - overlay overlay rw\n", false},
		{"ambiguous target", "/workspace/.state", root + state + state, false},
		{"ambiguous root", "/workspace/.state", root + root + state, false},
		{"non-root filesystem origin", "/workspace/.state", strings.Replace(root, " / / ", " /host/root / ", 1) + state, false},
		{"malformed record", "/workspace/.state", root + state + "incomplete\n", false},
		{"unreadable tail", "/workspace/.state", root + state + strings.Repeat("x", bufio.MaxScanTokenSize+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRootFSBindMountReader(tc.target, source, strings.NewReader(tc.document)); got != tc.want {
				t.Fatalf("mount evidence = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRootFSBindMountDoesNotTrustDirectoryAliases(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if IsRootFSBindMount(target, source) || IsRootFSBindMount(source, source) {
		t.Fatal("ordinary directories are not proven root-filesystem binds")
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if IsRootFSBindMount(link, source) || IsRootFSBindMount(source, link) {
		t.Fatal("symlinks must never establish state mount identity")
	}
}
