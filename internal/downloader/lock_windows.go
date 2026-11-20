//go:build windows

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
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive LockFileEx lock on all of f without waiting. The lock belongs to the
// open file, so Windows releases it when the file is closed or the process ends, however it ends.
func lockFile(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := conn.Control(func(fd uintptr) {
		lockErr = windows.LockFileEx(windows.Handle(fd), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
	}); err != nil {
		return err
	}
	if errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) {
		return errBusy
	}
	return lockErr
}

// releaseLock gives the lock up and then removes the lock file. Windows does not delete a file
// that is open, and Go opens files without FILE_SHARE_DELETE, so the lock has to go first. While
// another process has the lock file open, the removal fails, which is harmless: the lock file
// stays, empty and unlocked, and the next download of the file takes it over.
func releaseLock(f *os.File, path string) {
	_ = f.Close()
	_ = os.Remove(path)
}
