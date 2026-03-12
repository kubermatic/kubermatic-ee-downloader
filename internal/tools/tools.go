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
}

// KnownTools is the central registry of all downloadable enterprise tools.
// Add new tools here as they are published.
var KnownTools = map[string]Tool{
	"conformance-tester": {
		Description: "Kubermatic conformance cli",
		Registry:    "quay.io/kubermatic/conformance-ee",
		BinaryName:  "conformance-tester",
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
