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
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	data, err := os.ReadFile("tools.yaml")
	if err != nil {
		log.Fatalf("failed to read tools.yaml: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	catalog, err := FetchCatalog(srv.URL, 5*time.Second)
	srv.Close()
	if err != nil {
		log.Fatalf("FetchCatalog: %v", err)
	}
	KnownTools = catalog

	os.Exit(m.Run())
}

func TestNamesAreSorted(t *testing.T) {
	tests := []struct {
		name    string
		catalog map[string]Tool
		want    []string
	}{
		{
			name:    "custom unsorted",
			catalog: map[string]Tool{"zebra": {}, "alpha": {}, "middle": {}},
			want:    []string{"alpha", "middle", "zebra"},
		},
		{
			name:    "default catalog",
			catalog: KnownTools,
			want:    []string{"conformance-tester", "kubermatic-virtualization"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := KnownTools
			KnownTools = tt.catalog
			defer func() { KnownTools = prev }()

			names := Names()
			if len(names) != len(tt.want) {
				t.Fatalf("Names() = %v, want %v", names, tt.want)
			}
			for i := range tt.want {
				if names[i] != tt.want[i] {
					t.Errorf("Names()[%d] = %q, want %q", i, names[i], tt.want[i])
				}
			}
		})
	}
}

func TestNamesMatchKnownTools(t *testing.T) {
	names := Names()
	if len(names) != len(KnownTools) {
		t.Errorf("Names() returned %d names, want %d", len(names), len(KnownTools))
	}
	for _, name := range names {
		if _, ok := KnownTools[name]; !ok {
			t.Errorf("Names() returned %q which is not in KnownTools", name)
		}
	}
}

func TestKnownToolsInvariants(t *testing.T) {
	for name, tool := range KnownTools {
		if tool.Description == "" {
			t.Errorf("tool %q has empty Description", name)
		}
		if tool.Registry == "" {
			t.Errorf("tool %q has empty Registry", name)
		}
		if tool.BinaryName == "" {
			t.Errorf("tool %q has empty BinaryName", name)
		}
		if len(tool.Tag) == 0 {
			t.Errorf("tool %q has empty Tag list", name)
		}
		for i, tg := range tool.Tag {
			if tg == "" {
				t.Errorf("tool %q has empty tag at index %d", name, i)
			}
		}
		if !tool.KnownKind() {
			t.Errorf("tool %q has unknown kind %q", name, tool.Kind)
		}
		// Releases that predate "kind" would pull a file with simple_tag into memory.
		if tool.IsFile() && tool.SimpleTag {
			t.Errorf("tool %q is a file and must not set simple_tag", name)
		}
	}
}

func TestToolTagNameAndMode(t *testing.T) {
	const (
		version   = "v0.1.1-cli"
		installer = "kubermatic-virtualization"
	)
	tests := []struct {
		name     string
		tool     Tool
		wantTag  string
		wantName string
		wantMode os.FileMode
	}{
		{
			name:     "binary with platform tags",
			tool:     Tool{BinaryName: "conformance-tester"},
			wantTag:  "v0.1.1-cli-linux_amd64",
			wantName: "conformance-tester_v0.1.1-cli-linux_amd64",
			wantMode: 0o755,
		},
		{
			name:     "binary with simple tag",
			tool:     Tool{BinaryName: installer, SimpleTag: true},
			wantTag:  version,
			wantName: installer,
			wantMode: 0o755,
		},
		{
			name:     "explicit binary kind",
			tool:     Tool{BinaryName: installer, SimpleTag: true, Kind: KindBinary},
			wantTag:  version,
			wantName: installer,
			wantMode: 0o755,
		},
		{
			name:     "file keeps its published name",
			tool:     Tool{BinaryName: "kubev-mirror", Kind: KindFile},
			wantTag:  version,
			wantName: "",
			wantMode: 0o644,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tool.OCITag(version, "linux", "amd64"); got != tt.wantTag {
				t.Errorf("OCITag() = %q, want %q", got, tt.wantTag)
			}
			if got := tt.tool.OutputName(version, "linux", "amd64"); got != tt.wantName {
				t.Errorf("OutputName() = %q, want %q", got, tt.wantName)
			}
			if got := tt.tool.FileMode(); got != tt.wantMode {
				t.Errorf("FileMode() = %v, want %v", got, tt.wantMode)
			}
		})
	}
}

func TestParseCatalogKind(t *testing.T) {
	catalog, err := parseCatalog([]byte(`
test-iso:
  description: "A test ISO"
  registry: "quay.io/test/iso"
  binary_name: "test-iso"
  kind: file
  tags:
    - v1.0.0
test-tool:
  description: "A test tool"
  registry: "quay.io/test/tool"
  binary_name: "test-tool"
  kind: archive
  tags:
    - v1.0.0
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if iso := catalog["test-iso"]; !iso.IsFile() || !iso.KnownKind() || iso.SimpleTag {
		t.Errorf("test-iso = %+v, want a known file kind without simple_tag", iso)
	}
	if tool := catalog["test-tool"]; tool.KnownKind() {
		t.Errorf("test-tool kind %q reported as known", tool.Kind)
	}
}

// TestParseCatalogIgnoresUnknownFields keeps the catalog open to new fields: released
// downloaders decode it the same way, so a field they do not know must not break them.
func TestParseCatalogIgnoresUnknownFields(t *testing.T) {
	catalog, err := parseCatalog([]byte(`
test-tool:
  description: "A test tool"
  registry: "quay.io/test/tool"
  binary_name: "test-tool"
  field_from_the_future: true
  tags:
    - latest
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tool := catalog["test-tool"]; tool.BinaryName != "test-tool" {
		t.Errorf("BinaryName = %q, want %q", tool.BinaryName, "test-tool")
	}
}

func TestFetchCatalogSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
test-tool:
  description: "A test tool"
  registry: "quay.io/test/tool"
  binary_name: "test-tool"
  tags:
    - latest
  architectures:
    - amd64
  os:
    - linux
`))
	}))
	defer srv.Close()

	catalog, err := FetchCatalog(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tool, ok := catalog["test-tool"]
	if !ok {
		t.Fatal("expected test-tool in catalog")
	}
	if tool.BinaryName != "test-tool" {
		t.Errorf("BinaryName = %q, want %q", tool.BinaryName, "test-tool")
	}
}

func TestFetchCatalogEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("---\n"))
	}))
	defer srv.Close()

	_, err := FetchCatalog(srv.URL, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for empty catalog")
	}
}

func TestLoadCatalogFileSuccess(t *testing.T) {
	catalog, err := LoadCatalogFile("tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := catalog["conformance-tester"]; !ok {
		t.Error("expected conformance-tester in catalog")
	}
}

func TestLoadCatalogFileErrors(t *testing.T) {
	dir := t.TempDir()

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("not: valid: yaml: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "missing file", path: filepath.Join(dir, "does-not-exist.yaml")},
		{name: "invalid YAML", path: invalid},
		{name: "empty file", path: empty},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadCatalogFile(tt.path); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestFetchCatalogNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchCatalog(srv.URL, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestFetchCatalogInvalidYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not: valid: yaml: ["))
	}))
	defer srv.Close()

	_, err := FetchCatalog(srv.URL, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestFetchCatalogTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// block forever
		<-r.Context().Done()
	}))
	defer srv.Close()

	_, err := FetchCatalog(srv.URL, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFetchCatalogUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := FetchCatalog(url, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}
