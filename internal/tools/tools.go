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

	var m map[string]Tool
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parsing catalog YAML: %w", err)
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
