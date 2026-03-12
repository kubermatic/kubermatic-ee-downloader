package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// fetchFailTarget satisfies oras.ReadOnlyTarget: Resolve succeeds so that Pull
// proceeds past the initial check, but Fetch always fails so that oras.Copy errors.
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

// buildStore constructs an in-memory OCI store holding a single binary blob
// tagged at the given tag, simulating what a real OCI registry serves.
func buildStore(t *testing.T, tag, binaryName string, binaryData []byte) *memory.Store {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	// Push binary blob.
	blobDesc := content.NewDescriptorFromBytes("application/octet-stream", binaryData)
	blobDesc.Annotations = map[string]string{ocispec.AnnotationTitle: binaryName}
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

	return store
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// --- Pull tests ---

func TestPull_HappyPath(t *testing.T) {
	binaryData := []byte("fake conformance-tester binary content")
	store := buildStore(t, "latest", "conformance-tester", binaryData)

	got, err := Pull(context.Background(), discardLogger(), store, "latest", "conformance-tester")
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !bytes.Equal(got, binaryData) {
		t.Errorf("Pull() = %q, want %q", got, binaryData)
	}
}

func TestPull_TagNotFound(t *testing.T) {
	store := memory.New()

	_, err := Pull(context.Background(), discardLogger(), store, "nonexistent", "conformance-tester")
	if err == nil {
		t.Error("Pull() expected error for unknown tag, got nil")
	}
}

func TestPull_NoBinaryLayerMatch(t *testing.T) {
	// Layer exists but its title does not match the requested binary name.
	store := buildStore(t, "latest", "unrelated-tool", []byte("other content"))

	_, err := Pull(context.Background(), discardLogger(), store, "latest", "conformance-tester")
	if err == nil {
		t.Error("Pull() expected error when no binary layer matches, got nil")
	}
}

func TestPull_CopyFails(t *testing.T) {
	// Resolve succeeds but every Fetch call fails, so oras.Copy returns an error.
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte(`{"schemaVersion":2}`))
	target := &fetchFailTarget{desc: desc}

	_, err := Pull(context.Background(), discardLogger(), target, "latest", "conformance-tester")
	if err == nil {
		t.Error("Pull() expected error when fetch fails, got nil")
	}
}

func TestPullFromRegistry_InvalidRegistry(t *testing.T) {
	_, err := PullFromRegistry(context.Background(), discardLogger(), "://invalid-registry", "latest", "tool", "user", "pass")
	if err == nil {
		t.Error("PullFromRegistry() expected error for invalid registry string, got nil")
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
			layer:      ocispec.Descriptor{MediaType: "application/json", Annotations: map[string]string{ocispec.AnnotationTitle: "conformance-tester"}},
			binaryName: "conformance-tester",
			want:       true,
		},
		{
			name:       "title annotation with .bin suffix",
			layer:      ocispec.Descriptor{MediaType: "application/json", Annotations: map[string]string{ocispec.AnnotationTitle: "tool.bin"}},
			binaryName: "other",
			want:       true,
		},
		{
			name:       "non-matching title overrides octet-stream media type",
			layer:      ocispec.Descriptor{MediaType: "application/octet-stream", Annotations: map[string]string{ocispec.AnnotationTitle: "unrelated"}},
			binaryName: "conformance-tester",
			want:       false,
		},
		{
			name:       "no annotation, octet-stream media type",
			layer:      ocispec.Descriptor{MediaType: "application/octet-stream"},
			binaryName: "conformance-tester",
			want:       true,
		},
		{
			name:       "no annotation, oci image layer media type",
			layer:      ocispec.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"},
			binaryName: "conformance-tester",
			want:       true,
		},
		{
			name:       "no annotation, unknown media type",
			layer:      ocispec.Descriptor{MediaType: "application/json"},
			binaryName: "conformance-tester",
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

// --- Save tests ---

func TestSave(t *testing.T) {
	dir := t.TempDir()
	data := []byte("binary content")
	binaryName := "test-tool"

	if err := Save(data, dir, binaryName); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path := filepath.Join(dir, binaryName)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Save() wrote %q, want %q", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("Save() file mode = %v, want executable bits set", info.Mode())
	}
}

func TestSave_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := Save([]byte("x"), dir, "tool"); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tool")); err != nil {
		t.Errorf("expected file to exist after Save(): %v", err)
	}
}

func TestSave_MkdirFails(t *testing.T) {
	// Use an existing regular file as the target directory — MkdirAll must fail.
	f, err := os.CreateTemp(t.TempDir(), "not-a-dir")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := Save([]byte("x"), f.Name(), "tool"); err == nil {
		t.Error("Save() expected error when dir path is an existing file, got nil")
	}
}
