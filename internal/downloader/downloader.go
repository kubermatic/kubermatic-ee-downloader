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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// PullFromRegistry creates an authenticated remote repository and calls Pull.
func PullFromRegistry(ctx context.Context, log *logrus.Logger, registry, tag, binaryName, username, password string) ([]byte, error) {
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
	return Pull(ctx, log, repo, tag, binaryName)
}

// Pull fetches the binary layer for binaryName from src at the given tag.
// src is satisfied by *remote.Repository in production and by a memory store in tests.
func Pull(ctx context.Context, log *logrus.Logger, src oras.ReadOnlyTarget, tag, binaryName string) ([]byte, error) {
	if _, err := src.Resolve(ctx, tag); err != nil {
		return nil, fmt.Errorf("failed to resolve tag %q: %w", tag, err)
	}

	memStore := memory.New()
	manifestDesc, err := oras.Copy(ctx, src, tag, memStore, tag, oras.CopyOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve OCI artifact: %w", err)
	}

	manifestBlob, err := content.FetchAll(ctx, memStore, manifestDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBlob, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.WithField("layers", len(manifest.Layers)).Debug("Manifest retrieved")

	for _, layer := range manifest.Layers {
		if IsBinaryLayer(layer, binaryName) {
			log.WithFields(logrus.Fields{
				"size":       fmt.Sprintf("%.2f MB", float64(layer.Size)/(1024*1024)),
				"media_type": layer.MediaType,
			}).Info("Downloading binary layer")
			return content.FetchAll(ctx, memStore, layer)
		}
	}

	return nil, fmt.Errorf("no binary layer found in artifact manifest")
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

// Save writes data to <dir>/<binaryName> with executable permissions,
// creating the directory if it does not exist.
func Save(data []byte, dir, binaryName string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, binaryName), data, 0755)
}
