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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestCreateDispatchAdmission_IncompleteSignedIdentityHasNoEffects(t *testing.T) {
	for _, retained := range []bool{false, true} {
		for _, missing := range []string{"agent", "project", "both"} {
			t.Run(missing+map[bool]string{false: "/fresh", true: "/retained"}[retained], func(t *testing.T) {
				mgr := &mockManager{}
				srv := newTestServerWithManager(t, mgr)
				srv.config.BrokerAuthEnabled, srv.config.BrokerAuthStrictMode = true, true
				key := []byte("complete-create-admission-test")
				configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
				projectDir := filepath.Join(t.TempDir(), ".scion")
				agentDir := filepath.Join(projectDir, "agents", "guarded-agent")
				marker := filepath.Join(agentDir, "retained-marker")
				if retained {
					if err := os.MkdirAll(agentDir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				body := map[string]any{"name": "guarded-agent", "id": "agent-a", "projectId": "project-a", "projectPath": projectDir, "requestId": "incomplete-create-attempt"}
				if missing == "agent" || missing == "both" {
					delete(body, "id")
				}
				if missing == "project" || missing == "both" {
					delete(body, "projectId")
				}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(data)))
				req.Header.Set("Content-Type", "application/json")
				signRequest(req, "broker-a", key)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if mgr.startCalls != 0 || len(srv.dispatchAttempts) != 0 {
					t.Fatal("incomplete hosted identity reached launch or dispatch-attempt effects")
				}
				if retained {
					data, err := os.ReadFile(marker)
					if err != nil || string(data) != "keep" {
						t.Fatalf("retained marker changed: %q %v", data, err)
					}
				} else if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
					t.Fatalf("fresh agent directory created: %v", err)
				}
			})
		}
	}
}

func TestCreateDispatchAdmission_RegisteredOmittedIdentityRequiresAuthority(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "verification-enabled", false: "verification-disabled"}[enabled], func(t *testing.T) {
			mgr := &mockManager{}
			srv := newTestServerWithManager(t, mgr)
			// Permissive middleware is intentional: the handler independently refuses
			// a configured Hub create rather than downgrading an omitted-ID request.
			srv.config.BrokerAuthEnabled, srv.config.BrokerAuthStrictMode = enabled, false
			configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", []byte("configured-create-test"))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name":"guarded-agent","requestId":"unsigned-create-attempt"}`))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if mgr.startCalls != 0 || len(srv.dispatchAttempts) != 0 {
				t.Fatal("unsigned hosted create reached side effects")
			}
		})
	}
}

func TestCreateDispatchAdmission_BuildContextBackstopBeforeOwnership(t *testing.T) {
	fixture := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	reached := false
	handler := fixture.srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, err := fixture.srv.buildStartContext(r.Context(), startContextInputs{
			Name: "guarded-agent", ProjectID: flatTestProjectID, RunID: "missing-agent-run", Operation: opCreate, HTTPRequest: r,
		})
		var refusal *startContextError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusBadRequest {
			t.Fatalf("incomplete signed create backstop error = %v", err)
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	signRequest(req, fixture.identity.RuntimeBrokerID, []byte("secret"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("signed request never reached the build-context backstop: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if records, err := fixture.srv.ownership.List(); err != nil || len(records) != 0 {
		t.Fatalf("incomplete identity changed ownership: %v %v", records, err)
	}
}

func TestCreateDispatchAdmission_LocalIDOnlyPreservesFreshProvision(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	projectDir, err := config.GetResolvedProjectDir("")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(projectDir, "agents", "local-agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "local-marker"), []byte("in-flight"), 0600); err != nil {
		t.Fatal(err)
	}
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{Name: "local-agent", AgentID: "local-attempt-id", Operation: opCreate, HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.CreateAdmission != nil || !sc.Opts.FreshProvision {
		t.Fatal("local transient create was treated as retained Hub admission")
	}
}
