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
	"sort"
	"testing"
)

func TestNamesAreSorted(t *testing.T) {
	names := Names()
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() = %v, want sorted", names)
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
	}
}
