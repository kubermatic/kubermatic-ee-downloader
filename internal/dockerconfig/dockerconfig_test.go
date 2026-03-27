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
package dockerconfig

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func encode(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func TestGetCredentials_DockerHub(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "`+encode("alice", "secret")+`"}
		}
	}`)

	creds, err := getCredentialsFromPath(path, "docker.io/library/nginx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds == nil {
		t.Fatal("expected credentials, got nil")
	}
	if creds.Username != "alice" || creds.Password != "secret" {
		t.Errorf("got %q/%q, want alice/secret", creds.Username, creds.Password)
	}
}

func TestGetCredentials_ExactHost(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{
		"auths": {
			"quay.io": {"auth": "`+encode("bob", "pass123")+`"}
		}
	}`)

	creds, err := getCredentialsFromPath(path, "quay.io/kubermatic/conformance-ee")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds == nil {
		t.Fatal("expected credentials, got nil")
	}
	if creds.Username != "bob" || creds.Password != "pass123" {
		t.Errorf("got %q/%q, want bob/pass123", creds.Username, creds.Password)
	}
}

func TestGetCredentials_NoMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{
		"auths": {
			"ghcr.io": {"auth": "`+encode("x", "y")+`"}
		}
	}`)

	creds, err := getCredentialsFromPath(path, "quay.io/something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil credentials, got %+v", creds)
	}
}

func TestGetCredentials_FileNotExist(t *testing.T) {
	creds, err := getCredentialsFromPath("/nonexistent/path/config.json", "docker.io/foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil, got %+v", creds)
	}
}

func TestGetCredentials_EmptyPath(t *testing.T) {
	creds, err := getCredentialsFromPath("", "docker.io/foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil, got %+v", creds)
	}
}

func TestGetCredentials_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `not json`)

	_, err := getCredentialsFromPath(path, "docker.io/foo")
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestGetCredentials_InvalidBase64(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{
		"auths": {
			"docker.io": {"auth": "!!!not-base64!!!"}
		}
	}`)

	_, err := getCredentialsFromPath(path, "docker.io/foo")
	if err == nil {
		t.Error("expected error for bad base64, got nil")
	}
}

func TestGetCredentials_EmptyAuth(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `{
		"auths": {
			"docker.io": {"auth": ""}
		}
	}`)

	creds, err := getCredentialsFromPath(path, "docker.io/foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil for empty auth, got %+v", creds)
	}
}

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"docker.io/library/nginx", "docker.io"},
		{"quay.io/kubermatic/tool", "quay.io"},
		{"ghcr.io", "ghcr.io"},
		{"https://index.docker.io/v1/", "index.docker.io"},
		{"localhost:5000/myimage", "localhost:5000"},
	}
	for _, tt := range tests {
		got := registryHost(tt.input)
		if got != tt.want {
			t.Errorf("registryHost(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
