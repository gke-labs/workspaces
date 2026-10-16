// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The ConfigMap in manifests/fleet-helper.yaml, as rendered by the deploy script.
const renderedPolicy = `rules:
- tlsPassthrough:
    hostnames: ["generativelanguage.googleapis.com", "*.googleapis.com"]
    ports:
      numbers: [443]
`

func TestLoadRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress-policy.yaml")
	if err := os.WriteFile(path, []byte(renderedPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadRules(path)
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	tp := rules[0].GetTlsPassthrough()
	if tp == nil {
		t.Fatalf("rule is not tls_passthrough: %v", rules[0])
	}
	if got := tp.GetHostnames(); len(got) != 2 || got[0] != "generativelanguage.googleapis.com" {
		t.Errorf("hostnames = %v", got)
	}
	if got := tp.GetPorts().GetNumbers(); len(got) != 1 || got[0] != 443 {
		t.Errorf("ports = %v", got)
	}
}

func TestLoadRulesEmptyPathDisablesDefaulting(t *testing.T) {
	rules, err := loadRules("")
	if err != nil || rules != nil {
		t.Fatalf("loadRules(\"\") = %v, %v; want nil, nil", rules, err)
	}
}

func TestLoadRulesRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("rules:\n- tls: {hostnames: [x]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRules(path); err == nil {
		t.Fatal("expected an error for an unknown rule type")
	}
}
