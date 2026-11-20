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
	// Register the hash functions that layer digests are verified with.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Request describes one download.
type Request struct {
	// Tag is the OCI tag of the artifact.
	Tag string
	// BinaryName selects the layer to download (see IsBinaryLayer).
	BinaryName string
	// Dir is the output directory. It is created if it does not exist.
	Dir string
	// FileName is the name to save the layer under. When empty, the file name the layer was
	// published with (its title annotation) is used, or BinaryName if the layer has none.
	FileName string
	// Mode is the permission of the saved file; 0755 when zero.
	Mode os.FileMode
}

// DownloadFromRegistry creates an authenticated remote repository and calls Download.
func DownloadFromRegistry(ctx context.Context, log *logrus.Logger, registry, username, password string, req Request) (string, error) {
	repo, err := newRepository(registry, username, password)
	if err != nil {
		return "", err
	}
	return Download(ctx, log, repo, req)
}

// newRepository returns the remote repository at registry, authenticating with username and password.
func newRepository(registry, username, password string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(registry)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize repository: %w", err)
	}
	repo.PlainHTTP = false
	repo.Client = &auth.Client{
		Credential: func(_ context.Context, _ string) (auth.Credential, error) {
			return auth.Credential{Username: username, Password: password}, nil
		},
	}
	return repo, nil
}

// IsUnauthorized reports whether the registry refused the credentials. It checks the status code
// rather than searching the message for "401", which sizes and digests in a message can contain.
func IsUnauthorized(err error) bool {
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == http.StatusUnauthorized
	}
	return strings.Contains(strings.ToLower(err.Error()), "unauthorized")
}

// Download saves the layer selected by req.BinaryName from the artifact at req.Tag into req.Dir
// and returns the path of the saved file. src is satisfied by *remote.Repository in production
// and by a memory store in tests.
//
// The layer is streamed to "<file>.part", so memory use does not grow with its size, and renamed
// to its final name once the whole file matches the layer's digest. A transfer that ends early is
// continued from the bytes already on disk, also by a later run, and a file that is already
// complete is not downloaded again. Only one process at a time downloads a file: another one
// stops with an error that says so (see lockDestination).
func Download(ctx context.Context, log *logrus.Logger, src oras.ReadOnlyTarget, req Request) (string, error) {
	manifestDesc, err := src.Resolve(ctx, req.Tag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve tag %q: %w", req.Tag, err)
	}

	manifestBlob, err := content.FetchAll(ctx, src, manifestDesc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBlob, &manifest); err != nil {
		return "", fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.WithField("layers", len(manifest.Layers)).Debug("Manifest retrieved")

	layer, ok := findLayer(manifest.Layers, req.BinaryName)
	if !ok {
		return "", fmt.Errorf("no binary layer found in artifact manifest")
	}
	if err := layer.Digest.Validate(); err != nil {
		return "", fmt.Errorf("invalid layer digest %q: %w", layer.Digest, err)
	}

	name, err := fileName(req, layer)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(req.Dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create output directory: %w", err)
	}
	dest := filepath.Join(req.Dir, name)

	lock, err := lockDestination(log, dest)
	if err != nil {
		return "", err
	}
	defer lock.release()

	done, err := alreadyDownloaded(ctx, log, dest, layer)
	if err != nil {
		return "", err
	}
	if done {
		log.WithField("path", dest).Info("File already downloaded and verified")
		return dest, nil
	}

	log.WithFields(logrus.Fields{
		"size":       formatBytes(layer.Size),
		"media_type": layer.MediaType,
	}).Info("Downloading binary layer")

	mode := req.Mode
	if mode == 0 {
		mode = 0o755
	}
	if err := fetchLayer(ctx, log, src, layer, dest, mode); err != nil {
		return "", err
	}
	return dest, nil
}

// findLayer returns the first layer that belongs to binaryName.
func findLayer(layers []ocispec.Descriptor, binaryName string) (ocispec.Descriptor, bool) {
	for _, layer := range layers {
		if IsBinaryLayer(layer, binaryName) {
			return layer, true
		}
	}
	return ocispec.Descriptor{}, false
}

// IsBinaryLayer reports whether layer belongs to the named binary.
// Title annotation takes precedence over media type so that multi-layer
// artifacts with mixed content are handled correctly.
func IsBinaryLayer(layer ocispec.Descriptor, binaryName string) bool {
	if title, ok := layer.Annotations[ocispec.AnnotationTitle]; ok {
		return strings.Contains(title, binaryName) || strings.HasSuffix(title, ".bin")
	}
	return strings.Contains(layer.MediaType, "application/octet-stream") ||
		strings.Contains(layer.MediaType, "application/vnd.oci.image.layer")
}

// fileName returns the name to save layer under: req.FileName when set, otherwise the last
// element of the layer's title annotation, otherwise req.BinaryName.
func fileName(req Request, layer ocispec.Descriptor) (string, error) {
	if req.FileName != "" {
		return req.FileName, nil
	}
	title, ok := layer.Annotations[ocispec.AnnotationTitle]
	if !ok {
		return req.BinaryName, nil
	}
	// The title comes from the registry: keep it inside the output directory.
	name := title[strings.LastIndexAny(title, `/\`)+1:]
	if name == "." || !filepath.IsLocal(name) {
		return "", fmt.Errorf("layer title %q is not a usable file name", title)
	}
	return name, nil
}

// alreadyDownloaded reports whether dest already holds layer: a regular file of the layer's
// size whose digest matches. Checking a large file takes a while and stops when ctx is done.
func alreadyDownloaded(ctx context.Context, log *logrus.Logger, dest string, layer ocispec.Descriptor) (bool, error) {
	info, err := os.Stat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check existing file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != layer.Size {
		return false, nil
	}

	log.WithField("path", dest).Info("Verifying the file already in the output directory")
	f, err := os.Open(dest)
	if err != nil {
		return false, fmt.Errorf("failed to open existing file: %w", err)
	}
	defer f.Close()
	digester := layer.Digest.Algorithm().Digester()
	if _, err := hashFrom(ctx, digester.Hash(), f, make([]byte, copyBufferSize)); err != nil {
		return false, fmt.Errorf("failed to read existing file: %w", err)
	}
	return digester.Digest() == layer.Digest, nil
}
