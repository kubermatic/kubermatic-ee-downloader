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
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
)

const (
	// lockSuffix names the lock file kept next to a file while it is being downloaded.
	lockSuffix = ".lock"
	// lockAttempts bounds how often lockDestination opens the lock file again because the
	// process that held it removed it in the meantime.
	lockAttempts = 10
)

// errBusy reports that another process is downloading the same file.
var errBusy = errors.New("another download of this file is running")

// tryLock takes an exclusive lock on f without waiting, and returns errBusy when another process
// holds one. It is a variable so that tests can stand in for a file system without locks.
var tryLock = lockFile

// destLock is the lock on one destination file. Download holds it from the check for a file that
// is already there to the rename of the finished download, so that two processes never write
// into the same part file. The lock belongs to the open lock file, so the operating system
// releases it when the process ends, however it ends: a crash leaves no stale lock, only an empty
// lock file that the next download of the file takes over.
type destLock struct {
	path string
	file *os.File
}

// lockDestination takes the lock on dest. When another process holds it, it returns errBusy at
// once instead of waiting, because a download can run for hours. Where the lock cannot be taken
// for another reason, such as a file system without locks, the download goes on without it and a
// warning says so.
func lockDestination(log *logrus.Logger, dest string) (*destLock, error) {
	path := dest + lockSuffix
	for range lockAttempts {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return withoutLock(log, dest, err), nil
		}
		if err := tryLock(file); err != nil {
			_ = file.Close()
			if errors.Is(err, errBusy) {
				return nil, busyError(dest, path)
			}
			// Nobody can lock the file here, so it is of no use to anybody.
			_ = os.Remove(path)
			return withoutLock(log, dest, err), nil
		}
		// The lock counts only while path still names the locked file: the process that held
		// it before removes the file before it lets go (see releaseLock), and a new lock file
		// may already have taken its place.
		if sameFile(file, path) {
			return &destLock{path: path, file: file}, nil
		}
		_ = file.Close()
	}
	return nil, busyError(dest, path)
}

// withoutLock warns that dest is downloaded without the lock, and returns a lock that holds nothing.
func withoutLock(log *logrus.Logger, dest string, err error) *destLock {
	log.WithError(err).WithField("path", dest).
		Warn("Could not lock the file, so make sure that no other download of it runs at the same time")
	return &destLock{}
}

// busyError says that another process is downloading dest.
func busyError(dest, lockPath string) error {
	return fmt.Errorf("%s: %w (it holds %s); run the command again when it has finished", dest, errBusy, lockPath)
}

// sameFile reports whether path names the open file f.
func sameFile(f *os.File, path string) bool {
	fileInfo, err := f.Stat()
	if err != nil {
		return false
	}
	pathInfo, err := os.Stat(path)
	return err == nil && os.SameFile(fileInfo, pathInfo)
}

// release removes the lock file and gives the lock up. It does nothing for a download that runs
// without the lock.
func (l *destLock) release() {
	if l.file == nil {
		return
	}
	releaseLock(l.file, l.path)
	l.file = nil
}
