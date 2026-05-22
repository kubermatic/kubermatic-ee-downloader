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
