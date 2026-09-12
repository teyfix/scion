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

package agent

import (
	"os"
	"strings"
	"testing"
)

func TestBuildScopedLabelVars(t *testing.T) {
	// Set an ambient host environment variable to verify it is NOT leaked
	origAmbient := os.Getenv("SECRET_HOST_VAR")
	_ = os.Setenv("SECRET_HOST_VAR", "leaked-secret")
	defer func() {
		if origAmbient != "" {
			_ = os.Setenv("SECRET_HOST_VAR", origAmbient)
		} else {
			_ = os.Unsetenv("SECRET_HOST_VAR")
		}
	}()

	env := map[string]string{
		"APP_DOMAIN":       "example.com",
		"TIER":             "dev",
		"SCION_AGENT_NAME": "should-be-overridden",
	}

	vars := BuildScopedLabelVars("Feature Worker (Issue #123)", "ag-12345", "My Project", "proj-9999", env)

	// Check authoritativeness of SCION identity variables
	if got := vars["SCION_AGENT_NAME"]; got != "Feature Worker (Issue #123)" {
		t.Errorf("SCION_AGENT_NAME = %q, want \"Feature Worker (Issue #123)\"", got)
	}
	if got := vars["SCION_AGENT_SLUG"]; got != "feature-worker-issue-123" {
		t.Errorf("SCION_AGENT_SLUG = %q, want \"feature-worker-issue-123\"", got)
	}
	if got := vars["SCION_AGENT_ID"]; got != "ag-12345" {
		t.Errorf("SCION_AGENT_ID = %q, want \"ag-12345\"", got)
	}
	if got := vars["SCION_PROJECT"]; got != "My Project" {
		t.Errorf("SCION_PROJECT = %q, want \"My Project\"", got)
	}
	if got := vars["SCION_PROJECT_SLUG"]; got != "my-project" {
		t.Errorf("SCION_PROJECT_SLUG = %q, want \"my-project\"", got)
	}
	if got := vars["SCION_PROJECT_ID"]; got != "proj-9999" {
		t.Errorf("SCION_PROJECT_ID = %q, want \"proj-9999\"", got)
	}

	// Check ingested environment variables
	if got := vars["APP_DOMAIN"]; got != "example.com" {
		t.Errorf("APP_DOMAIN = %q, want \"example.com\"", got)
	}
	if got := vars["TIER"]; got != "dev" {
		t.Errorf("TIER = %q, want \"dev\"", got)
	}

	// Check ambient env exclusion
	if _, exists := vars["SECRET_HOST_VAR"]; exists {
		t.Errorf("SECRET_HOST_VAR leaked from ambient host environment into scoped vars")
	}
}

func TestExpandAndValidateDockerLabels(t *testing.T) {
	vars := map[string]string{
		"SCION_AGENT_SLUG":   "issue-42",
		"SCION_AGENT_ID":     "ag-001",
		"SCION_PROJECT_SLUG": "my-project",
		"APP_DOMAIN":         "internal.net",
		"PORT":               "8080",
		"RESERVED_KEY":       "agent_id",
	}

	tests := []struct {
		name        string
		labels      map[string]string
		want        map[string]string
		wantErr     bool
		errContains string
	}{
		{
			name:   "nil and empty labels",
			labels: nil,
			want:   nil,
		},
		{
			name:   "empty map",
			labels: map[string]string{},
			want:   nil,
		},
		{
			name: "static key and value",
			labels: map[string]string{
				"traefik.enable": "true",
			},
			want: map[string]string{
				"traefik.enable": "true",
			},
		},
		{
			name: "expansion in keys and values",
			labels: map[string]string{
				"traefik.http.routers.${SCION_AGENT_SLUG}.rule":                      "Host(`${SCION_AGENT_SLUG}.${APP_DOMAIN}`)",
				"traefik.http.services.${SCION_AGENT_SLUG}.loadbalancer.server.port": "${PORT}",
				"custom.env.${SCION_PROJECT_SLUG}.${SCION_AGENT_ID}":                 "active",
			},
			want: map[string]string{
				"traefik.http.routers.issue-42.rule":                      "Host(`issue-42.internal.net`)",
				"traefik.http.services.issue-42.loadbalancer.server.port": "8080",
				"custom.env.my-project.ag-001":                            "active",
			},
		},
		{
			name: "unresolved variable in value",
			labels: map[string]string{
				"router.rule": "Host(`${UNKNOWN_DOMAIN}`)",
			},
			wantErr:     true,
			errContains: "unresolved variable ${UNKNOWN_DOMAIN}",
		},
		{
			name: "unresolved variable in key",
			labels: map[string]string{
				"service.${UNKNOWN_NAME}.port": "80",
			},
			wantErr:     true,
			errContains: "unresolved variable ${UNKNOWN_NAME}",
		},
		{
			name: "unclosed variable reference",
			labels: map[string]string{
				"test": "${UNCLOSED",
			},
			wantErr:     true,
			errContains: "unclosed variable reference",
		},
		{
			name: "empty variable reference",
			labels: map[string]string{
				"test": "${}",
			},
			wantErr:     true,
			errContains: "empty variable reference",
		},
		{
			name: "key collision after expansion",
			labels: map[string]string{
				"prefix.issue-42":            "first",
				"prefix.${SCION_AGENT_SLUG}": "second", // also expands to prefix.issue-42
			},
			wantErr:     true,
			errContains: "docker label key collision",
		},
		{
			name: "reserved label collision with scion prefix",
			labels: map[string]string{
				"scion.custom": "val",
			},
			wantErr:     true,
			errContains: "collides with reserved internal label",
		},
		{
			name: "reserved label collision with agent_id",
			labels: map[string]string{
				"agent_id": "my-agent",
			},
			wantErr:     true,
			errContains: "collides with reserved internal label",
		},
		{
			name: "non-reserved label via expansion succeeds",
			labels: map[string]string{
				"${SCION_AGENT_SLUG}": "val",
			},
			want: map[string]string{
				"issue-42": "val",
			},
			wantErr: false,
		},
		{
			name: "reserved label collision via expansion",
			labels: map[string]string{
				"${RESERVED_KEY}": "val",
			},
			wantErr:     true,
			errContains: "collides with reserved internal label",
		},
		{
			name: "reserved label collision project_id",
			labels: map[string]string{
				"project_id": "123",
			},
			wantErr:     true,
			errContains: "collides with reserved internal label",
		},
		{
			name: "reserved label collision agent_id",
			labels: map[string]string{
				"agent_id": "123",
			},
			wantErr:     true,
			errContains: "collides with reserved internal label",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandAndValidateDockerLabels(tt.labels, vars)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExpandAndValidateDockerLabels() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errContains)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("len(got) = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for k, wantV := range tt.want {
				if gotV, ok := got[k]; !ok || gotV != wantV {
					t.Errorf("got[%q] = %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}

func TestBuildScopedLabelVars_HostEnvPassthrough(t *testing.T) {
	t.Setenv("APP_DOMAIN", "tail.gg")
	t.Setenv("EXPLICIT_VAR", "host-value")
	t.Setenv("EMPTY_HOST_VAR", "")
	t.Setenv("UNSET_IN_CONFIG", "secret-key")

	env := map[string]string{
		"APP_DOMAIN":     "",             // Empty value = passthrough from host
		"EXPLICIT_VAR":   "config-value", // Explicit value takes precedence over host
		"EMPTY_HOST_VAR": "",             // Empty on host as well -> left absent
		"UNSET_ON_HOST":  "",             // Not set on host -> left absent
	}

	vars := BuildScopedLabelVars("test-agent", "ag-001", "my-project", "proj-001", env)

	// Passthrough succeeds
	if got := vars["APP_DOMAIN"]; got != "tail.gg" {
		t.Errorf("APP_DOMAIN = %q, want %q", got, "tail.gg")
	}

	// Explicit value in env map wins over host env
	if got := vars["EXPLICIT_VAR"]; got != "config-value" {
		t.Errorf("EXPLICIT_VAR = %q, want %q", got, "config-value")
	}

	// Empty on host should be absent
	if val, ok := vars["EMPTY_HOST_VAR"]; ok {
		t.Errorf("EMPTY_HOST_VAR present in vars with value %q, want absent", val)
	}

	// Unset on host should be absent
	if val, ok := vars["UNSET_ON_HOST"]; ok {
		t.Errorf("UNSET_ON_HOST present in vars with value %q, want absent", val)
	}

	// Ambient host env not in config env map should NOT be present
	if val, ok := vars["UNSET_IN_CONFIG"]; ok {
		t.Errorf("UNSET_IN_CONFIG leaked into vars with value %q", val)
	}
}

func TestExpandAndValidateDockerLabels_HostEnvPassthrough(t *testing.T) {
	t.Run("env with empty value and host env set resolves in label", func(t *testing.T) {
		t.Setenv("APP_DOMAIN", "tail.gg")
		env := map[string]string{
			"APP_DOMAIN": "",
		}
		vars := BuildScopedLabelVars("my-agent", "ag-1", "proj", "p-1", env)
		labels := map[string]string{
			"traefik.http.routers.${SCION_AGENT_SLUG}.rule": "Host(`${SCION_AGENT_SLUG}.${APP_DOMAIN}`)",
		}
		resolved, err := ExpandAndValidateDockerLabels(labels, vars)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantRule := "Host(`my-agent.tail.gg`)"
		if got := resolved["traefik.http.routers.my-agent.rule"]; got != wantRule {
			t.Errorf("got %q, want %q", got, wantRule)
		}
	})

	t.Run("env with empty value and host env NOT set fails with unresolved variable error", func(t *testing.T) {
		orig, exists := os.LookupEnv("APP_DOMAIN_UNSET_TEST")
		if exists {
			_ = os.Unsetenv("APP_DOMAIN_UNSET_TEST")
			defer func() {
				_ = os.Setenv("APP_DOMAIN_UNSET_TEST", orig)
			}()
		}
		env := map[string]string{
			"APP_DOMAIN_UNSET_TEST": "",
		}
		vars := BuildScopedLabelVars("my-agent", "ag-1", "proj", "p-1", env)
		labels := map[string]string{
			"router.rule": "Host(`${APP_DOMAIN_UNSET_TEST}`)",
		}
		_, err := ExpandAndValidateDockerLabels(labels, vars)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "unresolved variable ${APP_DOMAIN_UNSET_TEST}") {
			t.Errorf("error %q does not contain expected substring", err.Error())
		}
	})

	t.Run("env with non-empty value does not call host env", func(t *testing.T) {
		t.Setenv("APP_DOMAIN", "host-ignored.com")
		env := map[string]string{
			"APP_DOMAIN": "explicit.domain.com",
		}
		vars := BuildScopedLabelVars("my-agent", "ag-1", "proj", "p-1", env)
		labels := map[string]string{
			"router.rule": "Host(`${APP_DOMAIN}`)",
		}
		resolved, err := ExpandAndValidateDockerLabels(labels, vars)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantRule := "Host(`explicit.domain.com`)"
		if got := resolved["router.rule"]; got != wantRule {
			t.Errorf("got %q, want %q", got, wantRule)
		}
	})
}
