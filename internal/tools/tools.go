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
	_ "embed"
	"sort"

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

//go:embed tools.yaml
var toolsYAML []byte

// KnownTools is the central registry of all downloadable enterprise tools.
// Add new tools to internal/tools/tools.yaml.
var KnownTools = func() map[string]Tool {
	var m map[string]Tool
	if err := yaml.Unmarshal(toolsYAML, &m); err != nil {
		panic("tools: failed to parse tools.yaml: " + err.Error())
	}
	return m
}()

// Names returns the sorted list of known tool names.
func Names() []string {
	names := make([]string, 0, len(KnownTools))
	for k := range KnownTools {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
