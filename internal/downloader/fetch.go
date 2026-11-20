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
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

const (
	// partSuffix names the unfinished download, kept next to the final file.
	partSuffix = ".part"
	// maxStalledAttempts bounds the attempts in a row that bring no new bytes. An attempt that
	// does bring new bytes is followed by the next one at once: quay.io, for example, ends every
	// transfer after about ten minutes, so a large file needs many attempts.
	maxStalledAttempts = 6
	// maxRetryWait caps the wait between attempts that bring no new bytes.
	maxRetryWait = time.Minute
	// copyBufferSize is the size of the one buffer a download reads through.
	copyBufferSize = 256 << 10
)

// Variables so that tests can shorten them.
var (
	// retryWait is the wait after the first attempt in a row that brought no new bytes. It
	// doubles with every further such attempt, up to maxRetryWait.
	retryWait = 2 * time.Second
	// stallTimeout ends an attempt that has received nothing for this long.
	stallTimeout = time.Minute
	// progressInterval is how often a running download logs its progress.
	progressInterval = 10 * time.Second
)

// errStalled ends an attempt that has received nothing for stallTimeout.
var errStalled = errors.New("no data received")

// diskError is a failure on this machine, such as a full disk, that another attempt would not fix.
type diskError struct {
	err error
}

func (e *diskError) Error() string { return e.err.Error() }

func (e *diskError) Unwrap() error { return e.err }

// fetchLayer streams layer from src into dest by way of dest+".part". Bytes already in the part
// file are kept, a transfer that ends early is continued with a byte range request, and dest
// appears only once the whole file matches the layer's digest.
func fetchLayer(ctx context.Context, log *logrus.Logger, src content.Fetcher, layer ocispec.Descriptor, dest string, mode os.FileMode) error {
	buf := make([]byte, copyBufferSize)
	p, err := openPart(ctx, dest+partSuffix, layer, mode, buf)
	if err != nil {
		return err
	}
	defer p.closeFile()

	if p.written > 0 {
		log.WithFields(sizeFields(p.written, layer.Size)).Info("Continuing the download kept from an earlier run")
	}

	f := &fetcher{
		log:      log,
		src:      src,
		layer:    layer,
		part:     p,
		progress: &progress{log: log, total: layer.Size},
		buf:      buf,
	}
	if err := f.run(ctx); err != nil {
		return err
	}
	// A cancelled download is not published, even when its file is complete: the part file
	// stays for the next run.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("download stopped: %w", err)
	}

	if got := p.sum(); got != layer.Digest {
		p.remove()
		return fmt.Errorf("downloaded file does not match the layer digest %s (got %s), so it was removed; run the command again", layer.Digest, got)
	}
	return p.commit(dest)
}

// fetcher downloads one layer into its part file, over as many attempts as it takes.
type fetcher struct {
	log      *logrus.Logger
	src      content.Fetcher
	layer    ocispec.Descriptor
	part     *part
	progress *progress
	buf      []byte
}

// run makes attempts until the part file holds the whole layer. It gives up on an error that
// another attempt would not fix, and after maxStalledAttempts attempts in a row that bring no
// bytes beyond the most the part file has held.
func (f *fetcher) run(ctx context.Context) error {
	highWater := f.part.written
	stalled := 0
	for f.part.written < f.layer.Size {
		err := f.attempt(ctx)
		if err == nil && f.part.written < f.layer.Size {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			break
		}
		if !retryable(ctx, err) {
			return fmt.Errorf("download stopped at %s of %s: %w", formatBytes(f.part.written), formatBytes(f.layer.Size), err)
		}

		if f.part.written > highWater {
			highWater = f.part.written
			stalled = 0
			f.log.WithError(err).WithFields(sizeFields(f.part.written, f.layer.Size)).
				Warn("Download interrupted, continuing from the bytes on disk")
			continue
		}

		stalled++
		if stalled >= maxStalledAttempts {
			return fmt.Errorf("download stopped at %s of %s after %d attempts without progress (run the command again to continue): %w",
				formatBytes(f.part.written), formatBytes(f.layer.Size), stalled, err)
		}
		wait := min(retryWait<<(stalled-1), maxRetryWait)
		f.log.WithError(err).WithField("retry_in", wait).Warn("Download attempt failed")
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	return nil
}

// attempt reads the rest of the layer over one connection and appends it to the part file.
// Every attempt asks the registry afresh, because a download link may have expired.
func (f *fetcher) attempt(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer watchdog.Stop()

	rc, err := f.src.Fetch(ctx, f.layer)
	if err != nil {
		return stallCause(ctx, err)
	}
	defer rc.Close()

	if err := f.skipKept(ctx, rc); err != nil {
		return err
	}

	f.progress.restart(f.part.written)
	r := io.LimitReader(rc, f.layer.Size-f.part.written)
	for {
		n, readErr := r.Read(f.buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			if err := f.part.write(f.buf[:n]); err != nil {
				return err
			}
			f.progress.update(f.part.written)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return stallCause(ctx, readErr)
		}
	}
}

// skipKept moves rc past the bytes already in the part file: oras-go returns a seekable reader for
// a registry that accepts byte ranges, and seeking sends a new request for the rest of the layer
// only. When the registry cannot continue a download, skipKept empties the part file instead, and
// the download starts again from the beginning.
func (f *fetcher) skipKept(ctx context.Context, rc io.Reader) error {
	if f.part.written == 0 {
		return nil
	}
	if seeker, ok := rc.(io.Seeker); ok {
		_, err := seeker.Seek(f.part.written, io.SeekStart)
		if err == nil {
			return nil
		}
		if !rangeIgnored(err) {
			return stallCause(ctx, err)
		}
	}
	f.log.Warn("The registry cannot continue a download, starting again from the beginning")
	return f.part.reset()
}

// rangeIgnored reports whether err is oras-go's report that the registry, or a proxy on the way,
// answered a byte range request with the whole layer (status 200). oras-go says so only in the
// message, and its reader then still stands at the start of the layer.
func rangeIgnored(err error) bool {
	return strings.HasSuffix(err.Error(), fmt.Sprintf("unexpected status code %d", http.StatusOK))
}

// stallCause turns an error caused by the watchdog ending the attempt into errStalled.
func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStalled) {
		return fmt.Errorf("%w for %s", errStalled, stallTimeout)
	}
	return err
}

// retryable reports whether another attempt may succeed after err: not when the download was
// cancelled, the disk failed, or the registry refused the request for good (such as 401 or 404).
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var de *diskError
	if errors.As(err, &de) {
		return false
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return false
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode >= http.StatusInternalServerError ||
			resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusRequestTimeout
	}
	return true
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// part is the unfinished download: the file next to the final one, how many bytes it holds and
// their running digest.
type part struct {
	path    string
	file    *os.File
	alg     digest.Algorithm
	hash    hash.Hash
	written int64
}

// openPart opens the part file at path, or creates it with mode, and hashes the bytes an earlier
// run left in it; hashing stops when ctx is done. A file longer than the layer cannot be the start
// of it and is emptied. A file kept from an earlier run gets the permissions a new one would get,
// so that the saved file's permissions do not depend on how that run created it.
func openPart(ctx context.Context, path string, layer ocispec.Descriptor, mode os.FileMode, buf []byte) (*part, error) {
	_, statErr := os.Stat(path)
	kept := statErr == nil
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, mode)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	p := &part{path: path, file: file, alg: layer.Digest.Algorithm(), hash: layer.Digest.Algorithm().Hash()}
	if kept {
		if err := setNewFileMode(file, mode); err != nil {
			p.closeFile()
			return nil, fmt.Errorf("failed to set the permissions of %s: %w", path, err)
		}
	}

	info, err := file.Stat()
	if err != nil {
		p.closeFile()
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if info.Size() > layer.Size {
		if err := p.reset(); err != nil {
			p.closeFile()
			return nil, err
		}
		return p, nil
	}
	n, err := hashFrom(ctx, p.hash, file, buf)
	if err != nil {
		p.closeFile()
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	p.written = n
	return p, nil
}

// setNewFileMode gives file the permissions that a file created next to it with mode gets: mode
// less the umask, or what a default ACL of the directory gives. A probe file shows what that is,
// as the go command does for the programs it installs, because the umask cannot be read without
// changing it for the whole process. Windows has no such permissions (Go maps only the read-only
// flag), so nothing is done there.
func setNewFileMode(file *os.File, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	probePath := file.Name() + ".mode"
	_ = os.Remove(probePath) // left by a run that ended while it existed
	probe, err := os.OpenFile(probePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	probeInfo, err := probe.Stat()
	_ = probe.Close()
	_ = os.Remove(probePath)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if want := probeInfo.Mode().Perm(); info.Mode().Perm() != want {
		return file.Chmod(want)
	}
	return nil
}

// hashFrom feeds what is left in r into h and returns how many bytes that was. It checks ctx
// before every read, so that verifying a large file can be cancelled.
func hashFrom(ctx context.Context, h hash.Hash, r io.Reader, buf []byte) (int64, error) {
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		k, err := r.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// write appends b to the file and keeps the digest and the byte count in step with what was written.
func (p *part) write(b []byte) error {
	n, err := p.file.Write(b)
	p.hash.Write(b[:n])
	p.written += int64(n)
	if err != nil {
		return &diskError{err: fmt.Errorf("failed to write %s: %w", p.path, err)}
	}
	return nil
}

// reset empties the file, to start the download again.
func (p *part) reset() error {
	if err := p.file.Truncate(0); err != nil {
		return &diskError{err: fmt.Errorf("failed to empty %s: %w", p.path, err)}
	}
	if _, err := p.file.Seek(0, io.SeekStart); err != nil {
		return &diskError{err: fmt.Errorf("failed to rewind %s: %w", p.path, err)}
	}
	p.hash.Reset()
	p.written = 0
	return nil
}

// sum returns the digest of the bytes in the file.
func (p *part) sum() digest.Digest {
	return digest.NewDigest(p.alg, p.hash)
}

// commit makes the verified download the final file, replacing any file of that name.
func (p *part) commit(dest string) error {
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("failed to write %s: %w", p.path, err)
	}
	err := p.file.Close()
	p.file = nil
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", p.path, err)
	}
	if err := os.Rename(p.path, dest); err != nil {
		return fmt.Errorf("failed to move the download into place: %w", err)
	}
	return nil
}

// closeFile closes the file if it is still open.
func (p *part) closeFile() {
	if p.file != nil {
		_ = p.file.Close()
		p.file = nil
	}
}

// remove deletes the file.
func (p *part) remove() {
	p.closeFile()
	_ = os.Remove(p.path)
}

// progress logs how far a download got, at most once per progressInterval.
type progress struct {
	log   *logrus.Logger
	total int64
	since time.Time
	from  int64
}

// restart starts measuring the speed afresh, at the start of an attempt.
func (p *progress) restart(done int64) {
	p.since, p.from = time.Now(), done
}

// update logs the progress when progressInterval has passed since the last report.
func (p *progress) update(done int64) {
	elapsed := time.Since(p.since)
	if elapsed < progressInterval {
		return
	}
	p.log.WithFields(sizeFields(done, p.total)).WithFields(logrus.Fields{
		"percent": fmt.Sprintf("%.1f%%", float64(done)*100/float64(p.total)),
		"speed":   formatBytes(int64(float64(done-p.from)/elapsed.Seconds())) + "/s",
	}).Info("Download progress")
	p.restart(done)
}

// sizeFields returns the log fields for how much of a download is on disk.
func sizeFields(done, total int64) logrus.Fields {
	return logrus.Fields{"downloaded": formatBytes(done), "total": formatBytes(total)}
}

// formatBytes renders n bytes with a binary unit, such as "27.04 GiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
