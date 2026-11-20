//go:build unix

/*
Copyright 2026 The Kubermatic Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package downloader

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestDownload_KeptPartFollowsUmask continues a part file under a stricter umask than usual: the
// saved file must get what a new file gets under that umask, not simply the requested mode.
func TestDownload_KeptPartFollowsUmask(t *testing.T) {
	shortWaits(t, time.Minute)
	old := syscall.Umask(0o027)
	t.Cleanup(func() { syscall.Umask(old) })
	if got := wantPerm(t, 0o755); got != 0o750 {
		t.Fatalf("a new file gets %v under umask 027, want -rwxr-x---", got)
	}

	const size = 1<<20 + 9
	const kept = 1 << 19
	reg := newTestRegistry(t, size, isoTitle)
	dir := t.TempDir()
	path := filepath.Join(dir, isoTitle)
	if err := os.WriteFile(path+partSuffix, patternBytes(kept), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+partSuffix, 0o644); err != nil {
		t.Fatal(err)
	}
	req := isoRequest(dir)
	req.Mode = 0o755

	if _, err := Download(context.Background(), discardLogger(), reg.repository(t), req); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	checkFile(t, path, patternBytes(size), 0o750)
	checkOnlyFile(t, dir, isoTitle)
}

// TestDownload_CompleteFileInReadOnlyDirectory finds a complete file in a directory this user
// cannot write to. No lock file can be created there, and the file must still be found complete
// instead of the run failing.
func TestDownload_CompleteFileInReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to any directory")
	}
	data := []byte("binary content that is already installed")
	store, _ := buildStore(t, latestTag, conformanceTester, data)
	dir := t.TempDir()
	path := filepath.Join(dir, conformanceTester)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // so that the directory can be removed

	got, err := Download(context.Background(), discardLogger(), store, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if got != path {
		t.Errorf("Download() path = %q, want %q", got, path)
	}
	checkOnlyFile(t, dir, conformanceTester)
}
