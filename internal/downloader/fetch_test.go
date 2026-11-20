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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	testRepo   = "kubermatic/test"
	testTag    = "v1.3.0"
	isoName    = "kubev-mirror"
	isoTitle   = "kubev-mirror-v1.3.0-amd64.iso"
	blockShift = 20 // the synthetic layer repeats a block of 1 MiB
)

// patternBlock holds pseudo-random bytes from which the synthetic layers are made.
var patternBlock = func() []byte {
	b := make([]byte, 1<<blockShift)
	x := uint64(88172645463325252)
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x)
	}
	return b
}()

// fillPattern fills b with the synthetic layer's bytes from offset off on: the block, with every
// repetition changed by its number so that bytes at a wrong offset never match.
func fillPattern(b []byte, off int64) {
	for len(b) > 0 {
		n := copy(b, patternBlock[off%int64(len(patternBlock)):])
		if k := byte(off >> blockShift); k != 0 {
			for i := range b[:n] {
				b[i] ^= k
			}
		}
		b = b[n:]
		off += int64(n)
	}
}

// patternBytes returns the first size bytes of the synthetic layer.
func patternBytes(size int64) []byte {
	b := make([]byte, size)
	fillPattern(b, 0)
	return b
}

// fillLayer fills b with the bytes of the synthetic layer's variant xor from offset off on. Variant
// 0 is the pattern itself; another variant has the same size and other bytes, like another version
// of a file published under the same name.
func fillLayer(b []byte, off int64, xor byte) {
	fillPattern(b, off)
	if xor != 0 {
		for i := range b {
			b[i] ^= xor
		}
	}
}

// fileSize returns the size of the file at path, or -1 when there is none.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

// waitFor polls cond until it holds and fails the test when it does not within 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// transfer is one request for the layer's bytes as the test registry saw it.
type transfer struct {
	start  int64
	ranged bool
}

// testRegistry serves one artifact with a single layer of synthetic bytes, never held in memory,
// the way quay.io does: the blob URL redirects to a download link that accepts byte ranges.
type testRegistry struct {
	server       *httptest.Server
	manifest     []byte
	manifestDesc ocispec.Descriptor
	layer        ocispec.Descriptor

	// xor selects the variant of the synthetic layer served (see fillLayer).
	xor byte
	// cut returns after how many bytes the given transfer (counted from 0) ends early, like a
	// quay.io link that expires; 0 sends everything.
	cut func(n int) int64
	// hang keeps the connection of a cut transfer open, sending nothing, instead of closing it.
	hang bool
	// hideRanges leaves out the Accept-Ranges header that says the server accepts byte ranges.
	hideRanges bool
	// ignoreRanges answers a byte range request with the whole layer (status 200).
	ignoreRanges bool
	// corrupt serves one wrong byte in the middle of the layer.
	corrupt bool
	// blobStatus answers the blob URL with this status instead of the redirect, when set.
	blobStatus int
	// hold, when set, stops every transfer that reaches offset holdAt there until it is closed.
	hold   chan struct{}
	holdAt int64

	mu           sync.Mutex
	transfers    []transfer
	blobRequests int
}

// newTestRegistry starts a registry serving a layer of size bytes, titled title when not empty.
func newTestRegistry(t *testing.T, size int64, title string) *testRegistry {
	t.Helper()
	return newVariantRegistry(t, size, title, 0)
}

// newVariantRegistry starts a registry serving variant xor of the synthetic layer (see fillLayer).
func newVariantRegistry(t *testing.T, size int64, title string, xor byte) *testRegistry {
	t.Helper()

	h := sha256.New()
	buf := make([]byte, 1<<blockShift)
	for off := int64(0); off < size; off += int64(len(buf)) {
		n := min(int64(len(buf)), size-off)
		fillLayer(buf[:n], off, xor)
		h.Write(buf[:n])
	}
	r := &testRegistry{xor: xor, layer: ocispec.Descriptor{
		MediaType: octetStream,
		Digest:    digest.NewDigest(digest.SHA256, h),
		Size:      size,
	}}
	if title != "" {
		r.layer.Annotations = map[string]string{ocispec.AnnotationTitle: title}
	}

	config := []byte("{}")
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    content.NewDescriptorFromBytes("application/vnd.oci.empty.v1+json", config),
		Layers:    []ocispec.Descriptor{r.layer},
	}
	manifest.SchemaVersion = 2
	var err error
	if r.manifest, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	r.manifestDesc = content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, r.manifest)

	r.server = httptest.NewServer(r)
	t.Cleanup(r.server.Close)
	return r
}

// holdTransfers makes every transfer that reaches offset at stop there until the returned
// function is called; the test's cleanup calls it too, so that the server can shut down.
func (r *testRegistry) holdTransfers(t *testing.T, at int64) (release func()) {
	t.Helper()
	r.hold, r.holdAt = make(chan struct{}), at
	var once sync.Once
	release = func() { once.Do(func() { close(r.hold) }) }
	t.Cleanup(release)
	return release
}

// repository returns the test registry's repository, with the client the downloader uses.
func (r *testRegistry) repository(t *testing.T) *remote.Repository {
	t.Helper()
	repo, err := newRepository(strings.TrimPrefix(r.server.URL, "http://")+"/"+testRepo, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	return repo
}

func (r *testRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/v2/" + testRepo + "/manifests/" + testTag, "/v2/" + testRepo + "/manifests/" + r.manifestDesc.Digest.String():
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", r.manifestDesc.Digest.String())
		w.Header().Set("Content-Length", strconv.Itoa(len(r.manifest)))
		if req.Method != http.MethodHead {
			_, _ = w.Write(r.manifest)
		}
	case "/v2/" + testRepo + "/blobs/" + r.layer.Digest.String():
		r.mu.Lock()
		r.blobRequests++
		r.mu.Unlock()
		if r.blobStatus != 0 {
			w.WriteHeader(r.blobStatus)
			return
		}
		http.Redirect(w, req, "/download/"+r.layer.Digest.Encoded(), http.StatusFound)
	case "/download/" + r.layer.Digest.Encoded():
		r.serveLayer(w, req)
	default:
		http.NotFound(w, req)
	}
}

// serveLayer serves the layer's bytes, from the start of the range when asked for one.
func (r *testRegistry) serveLayer(w http.ResponseWriter, req *http.Request) {
	size := r.layer.Size
	start, ranged := int64(0), false
	if v := req.Header.Get("Range"); v != "" && !r.ignoreRanges {
		if _, err := fmt.Sscanf(v, "bytes=%d-", &start); err != nil || start < 0 || start >= size {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		ranged = true
	}

	r.mu.Lock()
	n := len(r.transfers)
	r.transfers = append(r.transfers, transfer{start: start, ranged: ranged})
	r.mu.Unlock()

	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, size-1, size))
		w.Header().Set("Content-Length", strconv.FormatInt(size-start, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		if !r.hideRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}

	end := size
	if r.cut != nil {
		if c := r.cut(n); c > 0 && start+c < end {
			end = start + c
		}
	}
	buf := make([]byte, 64<<10)
	for off := start; off < end; {
		k := min(int64(len(buf)), end-off)
		if r.hold != nil && off < r.holdAt && r.holdAt < off+k {
			k = r.holdAt - off
		}
		fillLayer(buf[:k], off, r.xor)
		if mid := size / 2; r.corrupt && off <= mid && mid < off+k {
			buf[mid-off] ^= 0xff
		}
		if _, err := w.Write(buf[:k]); err != nil {
			return // the client went away
		}
		off += k
		if r.hold != nil && off == r.holdAt {
			w.(http.Flusher).Flush()
			select {
			case <-r.hold:
			case <-req.Context().Done():
				return
			}
		}
	}
	if end < size {
		w.(http.Flusher).Flush()
		if r.hang {
			<-req.Context().Done()
		}
		panic(http.ErrAbortHandler) // ends the transfer early
	}
}

// blobRequestCount returns how often the blob URL was requested.
func (r *testRegistry) blobRequestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blobRequests
}

// rangeStarts returns where each range request started.
func (r *testRegistry) rangeStarts() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	starts := []int64{}
	for _, tr := range r.transfers {
		if tr.ranged {
			starts = append(starts, tr.start)
		}
	}
	return starts
}

// shortWaits makes failed attempts retry at once and ends stalled ones after stall.
func shortWaits(t *testing.T, stall time.Duration) {
	t.Helper()
	oldRetry, oldStall := retryWait, stallTimeout
	retryWait, stallTimeout = time.Millisecond, stall
	t.Cleanup(func() { retryWait, stallTimeout = oldRetry, oldStall })
}

// isoRequest asks for the synthetic layer as a file named after its title.
func isoRequest(dir string) Request {
	return Request{Tag: testTag, BinaryName: isoName, Dir: dir, Mode: 0o644}
}

// heapObjectBytes returns the bytes of the heap held by objects, live or not yet swept.
func heapObjectBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// TestDownload_LargeLayerBoundedMemory downloads a layer of several hundred MiB (or
// DOWNLOADER_TEST_LAYER_MIB) over HTTP and checks that memory use does not grow with it:
// the heap stays small and the whole download allocates only a few MiB in total.
func TestDownload_LargeLayerBoundedMemory(t *testing.T) {
	mib := int64(256)
	if v := os.Getenv("DOWNLOADER_TEST_LAYER_MIB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("invalid DOWNLOADER_TEST_LAYER_MIB %q", v)
		}
		mib = n
	}
	const limit = 32 << 20

	reg := newTestRegistry(t, mib<<20, isoTitle)
	repo := reg.repository(t)
	dir := t.TempDir()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baseHeap := heapObjectBytes()

	var peakHeap uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			if h := heapObjectBytes(); h > peakHeap {
				peakHeap = h
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()

	start := time.Now()
	path, err := Download(context.Background(), discardLogger(), repo, isoRequest(dir))
	elapsed := time.Since(start)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	peakGrowth := uint64(0)
	if peakHeap > baseHeap {
		peakGrowth = peakHeap - baseHeap
	}
	t.Logf("layer %d MiB in %s: heap grew by at most %.1f MiB, %.1f MiB allocated in total, process memory obtained from the OS %.1f MiB",
		mib, elapsed.Round(time.Millisecond), float64(peakGrowth)/(1<<20), float64(allocated)/(1<<20), float64(after.Sys)/(1<<20))

	if peakGrowth > limit {
		t.Errorf("heap grew by %d bytes during the download, want at most %d", peakGrowth, limit)
	}
	if allocated > limit {
		t.Errorf("download allocated %d bytes, want at most %d", allocated, limit)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != reg.layer.Size || filepath.Base(path) != isoTitle {
		t.Errorf("saved %s with %d bytes, want %s with %d", filepath.Base(path), info.Size(), isoTitle, reg.layer.Size)
	}
}

// TestDownload_ContinuesCutTransfers ends every transfer after a few MiB, as quay.io does when a
// download link expires, and checks that the download continues each time with a range request.
func TestDownload_ContinuesCutTransfers(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 16<<20 + 777
	const chunk = 3<<20 + 12345
	reg := newTestRegistry(t, size, isoTitle)
	reg.cut = func(int) int64 { return chunk }
	dir := t.TempDir()

	path, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir))
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	want := []int64{}
	for off := int64(chunk); off < size; off += chunk {
		want = append(want, off)
	}
	if got := reg.rangeStarts(); !reflect.DeepEqual(got, want) {
		t.Errorf("range requests started at %v, want %v", got, want)
	}
}

// TestDownload_RefusesDigestMismatch serves one wrong byte: the file must not be saved, and no
// part file may be left for a later run to continue.
func TestDownload_RefusesDigestMismatch(t *testing.T) {
	shortWaits(t, time.Minute)
	reg := newTestRegistry(t, 4<<20, isoTitle)
	reg.corrupt = true
	dir := t.TempDir()

	_, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir))
	if err == nil || !strings.Contains(err.Error(), "does not match the layer digest") {
		t.Fatalf("Download() error = %v, want a digest mismatch", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("output directory holds %d entries after a digest mismatch, want none (first: %s)", len(entries), entries[0].Name())
	}
}

// TestDownload_ContinuesPartFile starts with the beginning of the layer in the part file, as an
// earlier run that was interrupted leaves it, and checks that only the rest is requested.
func TestDownload_ContinuesPartFile(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 8<<20 + 5
	const kept = 5<<20 + 333
	reg := newTestRegistry(t, size, isoTitle)
	dir := t.TempDir()
	path := filepath.Join(dir, isoTitle)
	if err := os.WriteFile(path+partSuffix, patternBytes(kept), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir)); err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	if got, want := reg.rangeStarts(), []int64{kept}; !reflect.DeepEqual(got, want) {
		t.Errorf("range requests started at %v, want %v", got, want)
	}
}

// TestDownload_RestartsLongPartFile starts with a part file longer than the layer, which cannot
// be the start of it.
func TestDownload_RestartsLongPartFile(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 1<<20 + 1
	reg := newTestRegistry(t, size, isoTitle)
	dir := t.TempDir()
	path := filepath.Join(dir, isoTitle)
	if err := os.WriteFile(path+partSuffix, patternBytes(size+10), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir)); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
}

// TestDownload_ContinuesStalledTransfer stops sending in the middle of the first transfer
// without closing the connection: the attempt must end and the download continue.
func TestDownload_ContinuesStalledTransfer(t *testing.T) {
	shortWaits(t, 300*time.Millisecond)
	const size = 4<<20 + 3
	const sent = 1<<20 + 99
	reg := newTestRegistry(t, size, isoTitle)
	reg.cut = func(n int) int64 {
		if n == 0 {
			return sent
		}
		return 0
	}
	reg.hang = true
	dir := t.TempDir()

	path, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir))
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	if got, want := reg.rangeStarts(), []int64{sent}; !reflect.DeepEqual(got, want) {
		t.Errorf("range requests started at %v, want %v", got, want)
	}
}

// TestDownload_RegistryWithoutRanges serves the layer without byte ranges: a cut download starts
// over, and gives up when starting over never gets further.
func TestDownload_RegistryWithoutRanges(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 2<<20 + 1
	const chunk = 1 << 20

	t.Run("first transfer cut", func(t *testing.T) {
		reg := newTestRegistry(t, size, isoTitle)
		reg.hideRanges, reg.ignoreRanges = true, true
		reg.cut = func(n int) int64 {
			if n == 0 {
				return chunk
			}
			return 0
		}
		path, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(t.TempDir()))
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	})

	t.Run("every transfer cut", func(t *testing.T) {
		reg := newTestRegistry(t, size, isoTitle)
		reg.hideRanges, reg.ignoreRanges = true, true
		reg.cut = func(int) int64 { return chunk }
		_, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(t.TempDir()))
		if err == nil || !strings.Contains(err.Error(), "without progress") {
			t.Fatalf("Download() error = %v, want it to give up", err)
		}
		// The first attempt brings bytes, then maxStalledAttempts get no further.
		if got, want := reg.blobRequestCount(), 1+maxStalledAttempts; got != want {
			t.Errorf("blob requested %d times, want %d", got, want)
		}
	})
}

// TestDownload_RegistryIgnoresRanges serves the layer like a server that says it accepts byte
// ranges but answers a range request with the whole layer (status 200), as some proxies do. A cut
// download, or one continued from an earlier run, must start over as with a registry without
// ranges, instead of failing on every attempt and on every later run; starting over that never
// gets further must give up without publishing anything.
func TestDownload_RegistryIgnoresRanges(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 2<<20 + 1
	const chunk = 1 << 20

	t.Run("first transfer cut", func(t *testing.T) {
		reg := newTestRegistry(t, size, isoTitle)
		reg.ignoreRanges = true
		reg.cut = func(n int) int64 {
			if n == 0 {
				return chunk
			}
			return 0
		}
		path, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(t.TempDir()))
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	})

	t.Run("part file from an earlier run", func(t *testing.T) {
		reg := newTestRegistry(t, size, isoTitle)
		reg.ignoreRanges = true
		dir := t.TempDir()
		path := filepath.Join(dir, isoTitle)
		if err := os.WriteFile(path+partSuffix, patternBytes(chunk), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir)); err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	})

	t.Run("every transfer cut", func(t *testing.T) {
		reg := newTestRegistry(t, size, isoTitle)
		reg.ignoreRanges = true
		reg.cut = func(int) int64 { return chunk }
		dir := t.TempDir()
		path := filepath.Join(dir, isoTitle)
		_, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(dir))
		if err == nil || !strings.Contains(err.Error(), "without progress") {
			t.Fatalf("Download() error = %v, want it to give up", err)
		}
		// The first attempt brings bytes; each of the maxStalledAttempts after it asks for the
		// layer and for the range, and gets no further.
		if got, want := reg.blobRequestCount(), 1+2*maxStalledAttempts; got != want {
			t.Errorf("blob requested %d times, want %d", got, want)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a file was published although the download gave up (err = %v)", err)
		}
		if got, err := os.ReadFile(path + partSuffix); err != nil || !bytes.Equal(got, patternBytes(int64(len(got)))) {
			t.Errorf("the part file kept for a later run does not hold the start of the layer (%d bytes, err = %v)", len(got), err)
		}
	})
}

// TestDownload_ConcurrentDownloadsOfOneFile starts a second download of the same file name while
// the first one is part-way through, as two versions of a simple_tag tool saved under one name
// do. The second must stop at once without touching the first one's part file, and the file the
// first one publishes must hold exactly the bytes it verified.
func TestDownload_ConcurrentDownloadsOfOneFile(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 4<<20 + 11
	const heldA = 1<<20 + 7  // where the first download is held
	const heldB = 2<<20 + 13 // where the second one would be held, had it started
	regA := newTestRegistry(t, size, isoTitle)
	regB := newVariantRegistry(t, size, isoTitle, 0x5a) // another version: same name and size, other bytes
	releaseA := regA.holdTransfers(t, heldA)
	releaseB := regB.holdTransfers(t, heldB)
	repoA, repoB := regA.repository(t), regB.repository(t)
	dir := t.TempDir()
	path := filepath.Join(dir, isoTitle)
	part := path + partSuffix

	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := Download(context.Background(), discardLogger(), repoA, isoRequest(dir))
		errA <- err
	}()
	waitFor(t, "the first download to write its first bytes", func() bool { return fileSize(part) == heldA })

	go func() {
		_, err := Download(context.Background(), discardLogger(), repoB, isoRequest(dir))
		errB <- err
	}()
	waitFor(t, "the second download to stop or to write", func() bool { return len(errB) > 0 || fileSize(part) >= heldB })
	stopped, partSize := len(errB) > 0, fileSize(part)

	// Let the first download finish and publish its file, then let the second one go on.
	releaseA()
	if err := <-errA; err != nil {
		t.Errorf("first download: %v", err)
	}
	releaseB()
	second := <-errB

	if !stopped || partSize != heldA {
		t.Errorf("the second download wrote into the first one's part file (%d bytes, the first one had written %d)", partSize, heldA)
	}
	if !errors.Is(second, errBusy) || !strings.Contains(second.Error(), path) {
		t.Errorf("second download error = %v, want one saying that %s is being downloaded", second, path)
	}
	checkFile(t, path, patternBytes(size), wantPerm(t, 0o644))
	checkOnlyFile(t, dir, isoTitle)
}

// TestDownload_KeptPartGetsNewFileMode continues part files that an earlier run left with other
// permissions: the saved file must get the permissions a new file gets, executable for a binary
// and not for a file, and the kept bytes must still be used.
func TestDownload_KeptPartGetsNewFileMode(t *testing.T) {
	shortWaits(t, time.Minute)
	const size = 2<<20 + 3
	const kept = 1<<20 + 5
	tests := []struct {
		name     string
		mode     os.FileMode // asked for by the request
		partMode os.FileMode // of the part file kept from an earlier run
	}{
		{name: "binary kept without the executable bits", mode: 0o755, partMode: 0o644},
		{name: "file kept with executable bits", mode: 0o644, partMode: 0o755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newTestRegistry(t, size, isoTitle)
			dir := t.TempDir()
			path := filepath.Join(dir, isoTitle)
			if err := os.WriteFile(path+partSuffix, patternBytes(kept), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path+partSuffix, tt.partMode); err != nil {
				t.Fatal(err)
			}
			req := isoRequest(dir)
			req.Mode = tt.mode

			if _, err := Download(context.Background(), discardLogger(), reg.repository(t), req); err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			checkFile(t, path, patternBytes(size), wantPerm(t, tt.mode))
			checkOnlyFile(t, dir, isoTitle)
			if got, want := reg.rangeStarts(), []int64{kept}; !reflect.DeepEqual(got, want) {
				t.Errorf("range requests started at %v, want %v", got, want)
			}
		})
	}
}

// cancellingReader cancels its context on the first read, like a user who stops the program
// while a file is being verified.
type cancellingReader struct {
	r      io.Reader
	cancel context.CancelFunc
	reads  int
}

func (c *cancellingReader) Read(p []byte) (int, error) {
	c.reads++
	c.cancel()
	return c.r.Read(p)
}

// TestHashFrom_StopsWhenCancelled cancels while a file is being hashed: hashing must stop at the
// next read instead of reading the file to its end.
func TestHashFrom_StopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancellingReader{r: bytes.NewReader(make([]byte, 1<<20)), cancel: cancel}

	n, err := hashFrom(ctx, sha256.New(), r, make([]byte, 1<<10))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("hashFrom() error = %v, want context.Canceled", err)
	}
	if r.reads != 1 || n != 1<<10 {
		t.Errorf("hashFrom() made %d reads (%d bytes), want it to stop after the read that cancelled (1 read, 1024 bytes)", r.reads, n)
	}
}

func TestDownload_BlobNotFoundIsNotRetried(t *testing.T) {
	shortWaits(t, time.Minute)
	reg := newTestRegistry(t, 1024, isoTitle)
	reg.blobStatus = http.StatusNotFound

	_, err := Download(context.Background(), discardLogger(), reg.repository(t), isoRequest(t.TempDir()))
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("Download() error = %v, want not found", err)
	}
	if n := reg.blobRequestCount(); n != 1 {
		t.Errorf("blob requested %d times, want 1", n)
	}
}

func TestDownload_LogsProgress(t *testing.T) {
	shortWaits(t, time.Minute)
	old := progressInterval
	progressInterval = 0
	t.Cleanup(func() { progressInterval = old })
	reg := newTestRegistry(t, 2<<20, isoTitle)
	var out bytes.Buffer
	log := logrus.New()
	log.SetOutput(&out)

	if _, err := Download(context.Background(), log, reg.repository(t), isoRequest(t.TempDir())); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	for _, want := range []string{"Download progress", `percent="100.0%"`, `total="2.00 MiB"`, "speed="} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := map[int64]string{
		0:               "0 B",
		1023:            "1023 B",
		1024:            "1.00 KiB",
		5<<20 + 1<<19:   "5.50 MiB",
		29_039_329_280:  "27.04 GiB",
		3 << 40:         "3.00 TiB",
		1<<62 + 1<<61:   "6.00 EiB",
		64<<30 + 64<<20: "64.06 GiB",
	}
	for n, want := range tests {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
