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
package tools

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Tool describes a downloadable Kubermatic enterprise binary.
type Tool struct {
	Description string `yaml:"description"`
	// Registry is the default OCI registry reference for this tool.
	// Can be overridden with --registry on the get command.
	Registry string `yaml:"registry"`
	// BinaryName is the filename written to the output directory.
	BinaryName string `yaml:"binary_name"`
	// Tag is the default tag to pull if none is specified in the registry reference.
	// Optional, defaults to "latest".
	Tag []string `yaml:"tags"`
	// Architectures lists the supported CPU architectures. Optional, defaults to "amd64".
	Architectures []string `yaml:"architectures"`
	// OS lists the supported operating systems. Optional, defaults to "linux".
	OS []string `yaml:"os"`
	// SimpleTag disables the automatic "{version}-{os}_{arch}" tag construction.
	// When true, the version string is used as the OCI tag directly.
	SimpleTag bool `yaml:"simple_tag"`
	// Kind is KindBinary (the default when empty) for an executable, or KindFile for a data
	// file such as an ISO image. A file is pulled by its version tag, keeps the file name it
	// was published with and is not made executable.
	//
	// Releases that predate this field ignore it and build the "{version}-{os}_{arch}" tag,
	// which is never published for a file, so they stop with "not found" instead of loading
	// a large file into memory. A file entry must therefore not set SimpleTag.
	Kind string `yaml:"kind"`
}

const (
	// KindBinary marks an executable, saved with mode 0755.
	KindBinary = "binary"
	// KindFile marks a data file such as an ISO image, saved with mode 0644.
	KindFile = "file"
)

// IsFile reports whether the tool is a data file rather than an executable.
func (t Tool) IsFile() bool {
	return t.Kind == KindFile
}

// KnownKind reports whether this version of the downloader knows how to save the tool.
func (t Tool) KnownKind() bool {
	return t.Kind == "" || t.Kind == KindBinary || t.Kind == KindFile
}

// OCITag returns the tag to pull for the given version, OS and architecture.
func (t Tool) OCITag(version, goos, arch string) string {
	if t.SimpleTag || t.IsFile() {
		return version
	}
	return fmt.Sprintf("%s-%s_%s", version, goos, arch)
}

// OutputName returns the file name to save the download under. It is empty for a file,
// which keeps the name it was published with.
func (t Tool) OutputName(version, goos, arch string) string {
	switch {
	case t.IsFile():
		return ""
	case t.SimpleTag:
		return t.BinaryName
	default:
		return fmt.Sprintf("%s_%s-%s_%s", t.BinaryName, version, goos, arch)
	}
}

// FileMode returns the permissions of the saved file: executable for a binary, not for a file.
func (t Tool) FileMode() os.FileMode {
	if t.IsFile() {
		return 0o644
	}
	return 0o755
}

// DefaultCatalogURL is the catalog served from the main branch of this repository.
const DefaultCatalogURL = "https://raw.githubusercontent.com/kubermatic/kubermatic-ee-downloader/main/internal/tools/tools.yaml"

// KnownTools is the active tool catalog, populated at startup via FetchCatalog.
var KnownTools map[string]Tool

// FetchCatalog downloads and parses a tools catalog from the given URL.
func FetchCatalog(url string, timeout time.Duration) (map[string]Tool, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("fetching catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching catalog: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading catalog response: %w", err)
	}

	return parseCatalog(body)
}

// LoadCatalogFile reads and parses a tools catalog from a local file.
func LoadCatalogFile(path string) (map[string]Tool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading catalog file: %w", err)
	}

	return parseCatalog(body)
}

func parseCatalog(body []byte) (map[string]Tool, error) {
	var m map[string]Tool
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parsing catalog YAML: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("catalog contains no tools")
	}

	return m, nil
}

// Names returns the sorted list of known tool names.
func Names() []string {
	names := make([]string, 0, len(KnownTools))
	for k := range KnownTools {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
