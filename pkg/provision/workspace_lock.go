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
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockWorkspace serializes native worktree provisioning and retirement, even
// across broker/CLI processes. The sibling lock inode outlives worktrees and is
// never removed: unlinking it would allow concurrent holders of different inodes.
// The existing store advisory lock still guards cross-node NFS provisioning.
func LockWorkspace(ctx context.Context, base string) (func() error, error) {
	ctx, cancel := context.WithTimeout(ctx, provisionLockRetries*provisionLockRetryDelay)
	defer cancel()
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(base)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(parent, ".scion-workspace-provision-"+filepath.Base(base)+".lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("workspace lock is not a regular file: %s", path)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, fmt.Errorf("waiting for workspace provisioning lock: %w", err)
		}
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(syscall.Flock(fd, syscall.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("acquire workspace lock: %w", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
