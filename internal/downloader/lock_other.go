//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

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
)

// lockFile reports that files cannot be locked on this system. It is not a release target; a
// download runs without the lock here, as it did before the lock was added.
func lockFile(*os.File) error {
	return errors.ErrUnsupported
}

// releaseLock removes the lock file and closes it. Without locks it is never called.
func releaseLock(f *os.File, path string) {
	_ = os.Remove(path)
	_ = f.Close()
}
