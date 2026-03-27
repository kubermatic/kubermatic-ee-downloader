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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials holds a username/password pair extracted from Docker config.
type Credentials struct {
	Username string
	Password string
}

// configFile represents the relevant parts of ~/.docker/config.json.
type configFile struct {
	Auths map[string]authEntry `json:"auths"`
}

type authEntry struct {
	Auth string `json:"auth"`
}

// GetCredentials attempts to read credentials for the given OCI registry
// reference from the Docker config file at ~/.docker/config.json.
// Returns nil if the file does not exist or contains no matching entry.
func GetCredentials(registry string) (*Credentials, error) {
	return getCredentialsFromPath(defaultConfigPath(), registry)
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

func getCredentialsFromPath(path, registry string) (*Credentials, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read docker config: %w", err)
	}

	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse docker config: %w", err)
	}

	return matchCredentials(cfg.Auths, registry)
}

// matchCredentials finds the auth entry that best matches the given OCI
// registry reference (e.g. "docker.io/library/nginx") and decodes it.
func matchCredentials(auths map[string]authEntry, registry string) (*Credentials, error) {
	host := registryHost(registry)

	// Try exact host match first, then common Docker Hub variants.
	candidates := []string{host}
	if isDockerHub(host) {
		candidates = append(candidates,
			"https://index.docker.io/v1/",
			"https://index.docker.io/v2/",
			"index.docker.io",
			"docker.io",
		)
	}

	for _, key := range candidates {
		entry, ok := auths[key]
		if !ok {
			continue
		}
		return decodeAuth(entry.Auth)
	}

	return nil, nil
}

// registryHost extracts the hostname from an OCI reference like
// "docker.io/library/nginx" → "docker.io".
func registryHost(ref string) string {
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return ref[:i]
	}
	return ref
}

func isDockerHub(host string) bool {
	return host == "docker.io" || host == "index.docker.io" || host == "registry-1.docker.io"
}

// decodeAuth decodes a base64-encoded "username:password" string.
func decodeAuth(encoded string) (*Credentials, error) {
	if encoded == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode auth field: %w", err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid auth format: expected username:password")
	}
	if parts[0] == "" || parts[1] == "" {
		return nil, nil
	}
	return &Credentials{Username: parts[0], Password: parts[1]}, nil
}
