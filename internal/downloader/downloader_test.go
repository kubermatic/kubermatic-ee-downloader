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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

const (
	conformanceTester = "conformance-tester"
	kubevInstaller    = "kubermatic-virtualization"
	testTool          = "tool"
	latestTag         = "latest"
	octetStream       = "application/octet-stream"
	jsonMediaType     = "application/json"
)

// fetchFailTarget satisfies oras.ReadOnlyTarget: Resolve succeeds so that Download
// proceeds past the initial check, but Fetch always fails.
type fetchFailTarget struct {
	desc ocispec.Descriptor
}

func (f *fetchFailTarget) Resolve(_ context.Context, _ string) (ocispec.Descriptor, error) {
	return f.desc, nil
}

func (f *fetchFailTarget) Fetch(_ context.Context, _ ocispec.Descriptor) (io.ReadCloser, error) {
	return nil, fmt.Errorf("simulated fetch failure")
}

func (f *fetchFailTarget) Exists(_ context.Context, _ ocispec.Descriptor) (bool, error) {
	return true, nil
}

// countingTarget counts the fetches of each digest.
type countingTarget struct {
	oras.ReadOnlyTarget

	mu      sync.Mutex
	fetches map[string]int
}

func (c *countingTarget) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	c.mu.Lock()
	c.fetches[desc.Digest.String()]++
	c.mu.Unlock()
	return c.ReadOnlyTarget.Fetch(ctx, desc)
}

// buildStore constructs an in-memory OCI store holding a single binary blob
// tagged at the given tag, simulating what a real OCI registry serves.
// The blob carries title as its title annotation, or none when title is empty.
func buildStore(t *testing.T, tag, title string, binaryData []byte) (*memory.Store, ocispec.Descriptor) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	// Push binary blob.
	blobDesc := content.NewDescriptorFromBytes(octetStream, binaryData)
	if title != "" {
		blobDesc.Annotations = map[string]string{ocispec.AnnotationTitle: title}
	}
	if err := store.Push(ctx, blobDesc, bytes.NewReader(binaryData)); err != nil {
		t.Fatalf("push blob: %v", err)
	}

	// Push an empty config (required by OCI image manifest spec).
	configData := []byte("{}")
	configDesc := content.NewDescriptorFromBytes("application/vnd.oci.image.config.v1+json", configData)
	if err := store.Push(ctx, configDesc, bytes.NewReader(configData)); err != nil {
		t.Fatalf("push config: %v", err)
	}

	// Build and push the OCI image manifest.
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{blobDesc},
	}
	manifest.SchemaVersion = 2
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestData)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		t.Fatalf("tag manifest: %v", err)
	}

	return store, blobDesc
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// fileModes reports whether files have Unix permissions here; on Windows they have none worth checking.
var fileModes = runtime.GOOS != "windows"

// wantPerm returns the permissions a file created with mode gets under the current umask. Where
// files have no Unix modes it returns 0, and checkFile leaves the mode out.
func wantPerm(t testing.TB, mode os.FileMode) os.FileMode {
	t.Helper()
	if !fileModes {
		return 0
	}
	probe := filepath.Join(t.TempDir(), "probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o777)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	return mode & info.Mode().Perm()
}

// checkFile fails the test unless path holds data with permissions perm and no part file is left.
func checkFile(t testing.TB, path string, data []byte, perm os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("file %s holds %d bytes that differ from the %d expected", path, len(got), len(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if fileModes && info.Mode().Perm() != perm {
		t.Errorf("file mode = %v, want %v", info.Mode().Perm(), perm)
	}
	if _, err := os.Stat(path + partSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("part file %s still exists (err = %v)", path+partSuffix, err)
	}
}

// checkOnlyFile fails the test unless dir holds the file name and nothing else: no part, lock or
// probe file is left behind.
func checkOnlyFile(t testing.TB, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != name {
		t.Errorf("output directory holds %q, want only %q", names, name)
	}
}

// recordingT stands in for a test's T, to see what the helpers report.
type recordingT struct {
	testing.TB

	errors  []string
	skipped bool
}

func (r *recordingT) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingT) Skip(...any) { r.skipped = true }

func (r *recordingT) Skipf(string, ...any) { r.skipped = true }

func (r *recordingT) SkipNow() { r.skipped = true }

// TestCheckFile_WithoutFileModes checks the helpers where files have no Unix modes, as on
// Windows: only the mode comparison may be left out. A wrong content and a part file left behind
// must still be reported, and the test must not be skipped.
func TestCheckFile_WithoutFileModes(t *testing.T) {
	old := fileModes
	fileModes = false
	t.Cleanup(func() { fileModes = old })

	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+partSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &recordingT{TB: t}
	checkFile(rec, path, []byte("expected bytes"), wantPerm(rec, 0o755))
	if rec.skipped {
		t.Error("the helpers skipped the test instead of leaving out only the mode")
	}
	if len(rec.errors) != 2 {
		t.Errorf("the helpers reported %d problems, want 2 (content and part file): %q", len(rec.errors), rec.errors)
	}
}

// --- Download tests ---

func TestDownload_HappyPath(t *testing.T) {
	binaryData := []byte("fake conformance-tester binary content")
	store, _ := buildStore(t, latestTag, conformanceTester, binaryData)
	dir := t.TempDir()

	path, err := Download(context.Background(), discardLogger(), store, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if want := filepath.Join(dir, conformanceTester); path != want {
		t.Errorf("Download() path = %q, want %q", path, want)
	}
	checkFile(t, path, binaryData, wantPerm(t, 0o755))
	checkOnlyFile(t, dir, conformanceTester)
}

// TestDownload_KeepsBinaryNameAndMode checks that the existing binaries are saved as before:
// under the name the caller builds from the catalog (not the layer's title), executable.
func TestDownload_KeepsBinaryNameAndMode(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		binaryName string
		fileName   string
	}{
		{
			name:       "simple tag: saved under binary_name",
			title:      kubevInstaller,
			binaryName: kubevInstaller,
			fileName:   kubevInstaller,
		},
		{
			name:       "platform tag: saved under the built name, not the title",
			title:      "conformance-tester.exe",
			binaryName: conformanceTester,
			fileName:   "conformance-tester_v0.1.1-cli-windows_amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("binary for " + tt.name)
			store, _ := buildStore(t, "v1", tt.title, data)
			dir := t.TempDir()

			path, err := Download(context.Background(), discardLogger(), store, Request{
				Tag: "v1", BinaryName: tt.binaryName, Dir: dir, FileName: tt.fileName, Mode: 0o755,
			})
			if err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			if want := filepath.Join(dir, tt.fileName); path != want {
				t.Errorf("Download() path = %q, want %q", path, want)
			}
			checkFile(t, path, data, wantPerm(t, 0o755))
		})
	}
}

// TestDownload_FileName checks the name a file gets when the caller leaves it to the artifact.
func TestDownload_FileName(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		wantName string
		wantErr  bool
	}{
		{name: "title", title: isoTitle, wantName: isoTitle},
		{name: "title with a directory", title: "out/kubev-mirror-v1.3.0-amd64.iso", wantName: isoTitle},
		{name: "title with a Windows directory", title: `out\kubev-mirror.iso`, wantName: "kubev-mirror.iso"},
		{name: "no title", wantName: isoName},
		{name: "title leaving the directory", title: "kubev-mirror/..", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("iso for " + tt.name)
			store, _ := buildStore(t, testTag, tt.title, data)
			dir := t.TempDir()

			path, err := Download(context.Background(), discardLogger(), store, Request{
				Tag: testTag, BinaryName: isoName, Dir: dir, Mode: 0o644,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Download() saved %q, want an error", path)
				}
				return
			}
			if err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			if want := filepath.Join(dir, tt.wantName); path != want {
				t.Errorf("Download() path = %q, want %q", path, want)
			}
			checkFile(t, path, data, wantPerm(t, 0o644))
		})
	}
}

func TestDownload_SkipsCompleteFile(t *testing.T) {
	data := []byte("binary content already on disk")
	store, layer := buildStore(t, latestTag, conformanceTester, data)
	target := &countingTarget{ReadOnlyTarget: store, fetches: map[string]int{}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, conformanceTester), data, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Download(context.Background(), discardLogger(), target, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if n := target.fetches[layer.Digest.String()]; n != 0 {
		t.Errorf("layer fetched %d times, want 0 for a complete file", n)
	}
}

// TestDownload_CancelledWhileVerifying cancels a download whose file is already complete on disk:
// checking it must stop, nothing may be published, and a part file must stay for the next run.
func TestDownload_CancelledWhileVerifying(t *testing.T) {
	data := []byte("content that is already complete on disk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := func(dir string) Request {
		return Request{Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester}
	}

	t.Run("complete part file", func(t *testing.T) {
		store, _ := buildStore(t, latestTag, conformanceTester, data)
		dir := t.TempDir()
		path := filepath.Join(dir, conformanceTester)
		if err := os.WriteFile(path+partSuffix, data, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := Download(ctx, discardLogger(), store, req(dir))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Download() error = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the file was published although the download was cancelled (err = %v)", err)
		}
		if got, err := os.ReadFile(path + partSuffix); err != nil || !bytes.Equal(got, data) {
			t.Errorf("the part file was not kept for the next run (err = %v)", err)
		}
	})

	t.Run("complete file", func(t *testing.T) {
		store, _ := buildStore(t, latestTag, conformanceTester, data)
		dir := t.TempDir()
		path := filepath.Join(dir, conformanceTester)
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := Download(ctx, discardLogger(), store, req(dir))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Download() error = %v, want context.Canceled", err)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
			t.Errorf("the complete file was changed (err = %v)", err)
		}
	})
}

// cancelAtEndTarget hands the layer out through a reader that cancels the download once the last
// byte is read, like a user who stops the program just as the transfer completes. Like any source
// that ignores the context, it lets the transfer itself finish.
type cancelAtEndTarget struct {
	oras.ReadOnlyTarget

	layer  ocispec.Descriptor
	cancel context.CancelFunc
}

func (c *cancelAtEndTarget) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := c.ReadOnlyTarget.Fetch(ctx, desc)
	if err != nil || desc.Digest != c.layer.Digest {
		return rc, err
	}
	return &cancelAtEnd{ReadCloser: rc, left: desc.Size, cancel: c.cancel}, nil
}

// cancelAtEnd calls cancel once left bytes have been read.
type cancelAtEnd struct {
	io.ReadCloser

	left   int64
	cancel context.CancelFunc
}

func (r *cancelAtEnd) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.left -= int64(n); r.left <= 0 {
		r.cancel()
	}
	return n, err
}

// TestDownload_CancelledAsTransferCompletes cancels a download just as its last byte arrives:
// the complete file must not be published, and the part file must stay for the next run.
func TestDownload_CancelledAsTransferCompletes(t *testing.T) {
	data := []byte("content whose transfer completes as the download is cancelled")
	store, layer := buildStore(t, latestTag, conformanceTester, data)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &cancelAtEndTarget{ReadOnlyTarget: store, layer: layer, cancel: cancel}
	dir := t.TempDir()
	path := filepath.Join(dir, conformanceTester)

	_, err := Download(ctx, discardLogger(), target, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Download() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file was published although the download was cancelled (err = %v)", err)
	}
	if got, err := os.ReadFile(path + partSuffix); err != nil || !bytes.Equal(got, data) {
		t.Errorf("the complete part file was not kept for the next run (err = %v)", err)
	}
}

func TestDownload_ReplacesDifferentFile(t *testing.T) {
	data := []byte("the new binary content")
	store, _ := buildStore(t, latestTag, conformanceTester, data)
	dir := t.TempDir()
	path := filepath.Join(dir, conformanceTester)
	// Same size, other bytes: an older version of the binary.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(data)), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Download(context.Background(), discardLogger(), store, Request{
		Tag: latestTag, BinaryName: conformanceTester, Dir: dir, FileName: conformanceTester,
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	checkFile(t, path, data, wantPerm(t, 0o755))
}

func TestDownload_TagNotFound(t *testing.T) {
	store := memory.New()

	_, err := Download(context.Background(), discardLogger(), store, Request{Tag: "nonexistent", BinaryName: conformanceTester, Dir: t.TempDir()})
	if err == nil {
		t.Error("Download() expected error for unknown tag, got nil")
	}
}

func TestDownload_NoBinaryLayerMatch(t *testing.T) {
	// Layer exists but its title does not match the requested binary name.
	store, _ := buildStore(t, latestTag, "unrelated-tool", []byte("other content"))

	_, err := Download(context.Background(), discardLogger(), store, Request{Tag: latestTag, BinaryName: conformanceTester, Dir: t.TempDir()})
	if err == nil {
		t.Error("Download() expected error when no binary layer matches, got nil")
	}
}

func TestDownload_FetchFails(t *testing.T) {
	// Resolve succeeds but every Fetch call fails.
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte(`{"schemaVersion":2}`))
	target := &fetchFailTarget{desc: desc}

	_, err := Download(context.Background(), discardLogger(), target, Request{Tag: latestTag, BinaryName: conformanceTester, Dir: t.TempDir()})
	if err == nil {
		t.Error("Download() expected error when fetch fails, got nil")
	}
}

func TestDownloadFromRegistry_InvalidRegistry(t *testing.T) {
	_, err := DownloadFromRegistry(context.Background(), discardLogger(), "://invalid-registry", "user", "pass", Request{Tag: latestTag, BinaryName: testTool, Dir: t.TempDir()})
	if err == nil {
		t.Error("DownloadFromRegistry() expected error for invalid registry string, got nil")
	}
}

func TestDownload_CreatesDirectory(t *testing.T) {
	store, _ := buildStore(t, latestTag, testTool, []byte("x"))
	dir := filepath.Join(t.TempDir(), "nested", "dir")

	if _, err := Download(context.Background(), discardLogger(), store, Request{Tag: latestTag, BinaryName: testTool, Dir: dir, FileName: testTool}); err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, testTool)); err != nil {
		t.Errorf("expected file to exist after Download(): %v", err)
	}
}

func TestDownload_MkdirFails(t *testing.T) {
	// Use an existing regular file as the target directory — MkdirAll must fail.
	f, err := os.CreateTemp(t.TempDir(), "not-a-dir")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	store, _ := buildStore(t, latestTag, testTool, []byte("x"))

	if _, err := Download(context.Background(), discardLogger(), store, Request{Tag: latestTag, BinaryName: testTool, Dir: f.Name(), FileName: testTool}); err == nil {
		t.Error("Download() expected error when dir path is an existing file, got nil")
	}
}

// --- IsUnauthorized tests ---

func TestIsUnauthorized(t *testing.T) {
	registryError := func(status int) error {
		return &errcode.ErrorResponse{Method: http.MethodHead, URL: &url.URL{Scheme: "https", Host: "quay.io"}, StatusCode: status}
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "registry answered 401", err: fmt.Errorf("failed to resolve tag: %w", registryError(http.StatusUnauthorized)), want: true},
		{name: "registry answered 404", err: registryError(http.StatusNotFound), want: false},
		{name: "401 inside a size", err: errors.New("download stopped at 401.25 MiB of 27.04 GiB: no space left on device"), want: false},
		{name: "unauthorized in the message", err: errors.New("fetching token: unauthorized"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnauthorized(tt.err); got != tt.want {
				t.Errorf("IsUnauthorized(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- IsBinaryLayer tests ---

func TestIsBinaryLayer(t *testing.T) {
	tests := []struct {
		name       string
		layer      ocispec.Descriptor
		binaryName string
		want       bool
	}{
		{
			name:       "title annotation matches binary name",
			layer:      ocispec.Descriptor{MediaType: jsonMediaType, Annotations: map[string]string{ocispec.AnnotationTitle: conformanceTester}},
			binaryName: conformanceTester,
			want:       true,
		},
		{
			name:       "title annotation with .bin suffix",
			layer:      ocispec.Descriptor{MediaType: jsonMediaType, Annotations: map[string]string{ocispec.AnnotationTitle: "tool.bin"}},
			binaryName: "other",
			want:       true,
		},
		{
			name:       "non-matching title overrides octet-stream media type",
			layer:      ocispec.Descriptor{MediaType: "application/octet-stream", Annotations: map[string]string{ocispec.AnnotationTitle: "unrelated"}},
			binaryName: conformanceTester,
			want:       false,
		},
		{
			name:       "no annotation, octet-stream media type",
			layer:      ocispec.Descriptor{MediaType: "application/octet-stream"},
			binaryName: conformanceTester,
			want:       true,
		},
		{
			name:       "no annotation, oci image layer media type",
			layer:      ocispec.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"},
			binaryName: conformanceTester,
			want:       true,
		},
		{
			name:       "no annotation, unknown media type",
			layer:      ocispec.Descriptor{MediaType: jsonMediaType},
			binaryName: conformanceTester,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBinaryLayer(tt.layer, tt.binaryName); got != tt.want {
				t.Errorf("IsBinaryLayer() = %v, want %v", got, tt.want)
			}
		})
	}
}
