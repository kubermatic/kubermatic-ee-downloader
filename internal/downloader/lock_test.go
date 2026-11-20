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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// lockHelperEnv names the file whose lock TestLockHelperProcess takes in a second process.
const lockHelperEnv = "DOWNLOADER_TEST_LOCK_HELPER"

// TestLockDestination_SecondLockIsRefused takes the lock twice: the second time must be refused
// at once, and once the first lock is released, which removes the lock file, it must work again.
func TestLockDestination_SecondLockIsRefused(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "file")
	first, err := lockDestination(discardLogger(), dest)
	if err != nil {
		t.Fatalf("lockDestination() error = %v", err)
	}
	if _, err := lockDestination(discardLogger(), dest); !errors.Is(err, errBusy) {
		t.Errorf("second lockDestination() error = %v, want errBusy", err)
	}
	first.release()
	if _, err := os.Stat(dest + lockSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file left after the release (err = %v)", err)
	}

	again, err := lockDestination(discardLogger(), dest)
	if err != nil {
		t.Fatalf("lockDestination() after the release: error = %v", err)
	}
	again.release()
}

// TestLockDestination_LockFileReplacedMeanwhile opens the lock file, and before locking it lets
// another download take the lock, finish and release it. The lock that is then taken on the old
// file must not count: the caller must end up holding the current lock file, so that a third
// download is refused.
func TestLockDestination_LockFileReplacedMeanwhile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "file")
	old := tryLock
	t.Cleanup(func() { tryLock = old })
	first := true
	tryLock = func(f *os.File) error {
		if first {
			first = false
			other, err := lockDestination(discardLogger(), dest)
			if err != nil {
				return err
			}
			other.release()
		}
		return lockFile(f)
	}

	l, err := lockDestination(discardLogger(), dest)
	if err != nil {
		t.Fatalf("lockDestination() error = %v", err)
	}
	defer l.release()
	if !sameFile(l.file, dest+lockSuffix) {
		t.Error("the lock is held on a lock file that is no longer there")
	}
	if _, err := lockDestination(discardLogger(), dest); !errors.Is(err, errBusy) {
		t.Errorf("third lockDestination() error = %v, want errBusy", err)
	}
}

// TestLockHelperProcess is not a test of its own: TestLockDestination_ReleasedWhenTheHolderDies
// runs the test program again with it, to hold a lock in another process until that is killed.
func TestLockHelperProcess(t *testing.T) {
	dest := os.Getenv(lockHelperEnv)
	if dest == "" {
		t.Skip("runs only as the helper of TestLockDestination_ReleasedWhenTheHolderDies")
	}
	if _, err := lockDestination(discardLogger(), dest); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute) // until it is killed
	os.Exit(1)
}

// TestLockDestination_ReleasedWhenTheHolderDies holds the lock in another process and kills that
// process, as a crash would. While the process lives the lock must be refused; once it is gone the
// lock must be free, although it was never released and its lock file is still there.
func TestLockDestination_ReleasedWhenTheHolderDies(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "file")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dest)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	lines := bufio.NewScanner(out)
	for lines.Scan() && lines.Text() != "locked" {
		if strings.HasPrefix(lines.Text(), "error:") {
			t.Fatalf("helper process: %s", lines.Text())
		}
	}

	if _, err := lockDestination(discardLogger(), dest); !errors.Is(err, errBusy) {
		t.Fatalf("lockDestination() while another process holds the lock: error = %v, want errBusy", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // it was killed, so it reports an error
	if _, err := os.Stat(dest + lockSuffix); err != nil {
		t.Fatalf("the killed process's lock file is gone (err = %v), so this test cannot check for a stale lock", err)
	}
	// Windows may take a moment to release the locks of a process that ended.
	deadline := time.Now().Add(10 * time.Second)
	l, err := lockDestination(discardLogger(), dest)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		l, err = lockDestination(discardLogger(), dest)
	}
	if err != nil {
		t.Fatalf("lockDestination() after the holder died: error = %v, want the lock", err)
	}
	l.release()
}

// TestDownload_WithoutFileLocks downloads where the file system cannot lock files: the download
// must go on without the lock, say so, and leave no lock file behind.
func TestDownload_WithoutFileLocks(t *testing.T) {
	old := tryLock
	tryLock = func(*os.File) error { return errors.ErrUnsupported }
	t.Cleanup(func() { tryLock = old })

	data := []byte("binary content")
	store, _ := buildStore(t, latestTag, conformanceTester, data)
	dir := t.TempDir()
	var out bytes.Buffer
	log := logrus.New()
	log.SetOutput(&out)

	path, err := Download(context.Background(), log, store, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	checkFile(t, path, data, wantPerm(t, 0o755))
	if !strings.Contains(out.String(), "Could not lock") {
		t.Errorf("log does not say that the download ran without the lock:\n%s", out.String())
	}
	if _, err := os.Stat(path + lockSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file left behind (err = %v)", err)
	}
}
