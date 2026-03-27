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

import "sort"

// Tool describes a downloadable Kubermatic enterprise binary.
type Tool struct {
	Description string
	// Registry is the default OCI registry reference for this tool.
	// Can be overridden with --registry on the get command.
	Registry string
	// BinaryName is the filename written to the output directory.
	BinaryName string
	// Tag is the default tag to pull if none is specified in the registry reference.
	// Optional, defaults to "latest".
	Tag []string
}

// KnownTools is the central registry of all downloadable enterprise tools.
// Add new tools here as they are published.
var KnownTools = map[string]Tool{
	"conformance-tester": {
		Description: "Kubermatic conformance cli",
		// Registry:    "quay.io/kubermatic/conformance-ee",
		Registry:   "docker.io/soer3n/edge-router",
		BinaryName: "conformance-tester",
		Tag:        []string{"latest"},
	},
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
