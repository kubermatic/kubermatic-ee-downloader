//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

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

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive flock(2) lock on f without waiting. The lock belongs to the open
// file, so the system releases it when the file is closed or the process ends, however it ends.
func lockFile(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := conn.Control(func(fd uintptr) {
		for {
			lockErr = unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(lockErr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	if errors.Is(lockErr, unix.EWOULDBLOCK) {
		return errBusy
	}
	return lockErr
}

// releaseLock removes the lock file while the lock is still held, and then gives the lock up. A
// process that opened the old file in the meantime then finds that it is gone, and locks a new
// one (see lockDestination).
func releaseLock(f *os.File, path string) {
	_ = os.Remove(path)
	_ = f.Close()
}
