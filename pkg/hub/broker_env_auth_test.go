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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestBrokerEnvAuth_GenericSuperAdminLifecycle(t *testing.T) {
	srv, st := testServer(t)
	broker := &store.RuntimeBroker{ID: tid("canonical-env-broker"), Name: "Canonical Env Broker", Slug: "canonical-env-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, st.CreateRuntimeBroker(context.Background(), broker))
	query := "?scope=" + store.ScopeRuntimeBroker + "&scopeId=" + broker.ID
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/CANONICAL_KEY", SetEnvVarRequest{Value: "initial", Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/env/CANONICAL_KEY"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/env"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/env/CANONICAL_KEY"+query, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := st.GetEnvVar(context.Background(), "CANONICAL_KEY", store.ScopeRuntimeBroker, broker.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestBrokerEnvAuth_MemberAndBrokerBoundaries(t *testing.T) {
	for _, generic := range []bool{false, true} {
		name := "nested"
		if generic {
			name = "generic"
		}
		t.Run(name, func(t *testing.T) {
			srv, st := testServer(t)
			ctx := context.Background()
			own := &store.RuntimeBroker{ID: tid("env-boundary-own"), Name: "Own Env Broker", Slug: "own-env-broker", Status: store.BrokerStatusOnline}
			other := &store.RuntimeBroker{ID: tid("env-boundary-other"), Name: "Other Env Broker", Slug: "other-env-broker", Status: store.BrokerStatusOnline}
			require.NoError(t, st.CreateRuntimeBroker(ctx, own))
			require.NoError(t, st.CreateRuntimeBroker(ctx, other))
			member := &store.User{ID: tid("env-boundary-member"), Email: "env-boundary-member@test.com", DisplayName: "Member", Role: store.UserRoleMember, Status: store.UserStatusActive}
			require.NoError(t, st.CreateUser(ctx, member))
			for _, target := range []*store.RuntimeBroker{own, other} {
				for _, op := range []string{"list", "get", "put", "delete"} {
					t.Run(target.Slug+"/"+op, func(t *testing.T) {
						_, err := st.UpsertEnvVar(ctx, &store.EnvVar{ID: tid("env-boundary-" + target.Slug), Key: "BOUNDARY_KEY", Value: "before", Scope: store.ScopeRuntimeBroker, ScopeID: target.ID})
						require.NoError(t, err)
						method := http.MethodGet
						if op == "put" {
							method = http.MethodPut
						}
						if op == "delete" {
							method = http.MethodDelete
						}
						path := "/api/v1/runtime-brokers/" + target.ID + "/env"
						if op != "list" {
							path += "/BOUNDARY_KEY"
						}
						if generic {
							path = "/api/v1/env"
							if op != "list" {
								path += "/BOUNDARY_KEY"
							}
							path += "?scope=" + store.ScopeRuntimeBroker + "&scopeId=" + target.ID
						}
						var body interface{}
						if op == "put" {
							body = SetEnvVarRequest{Value: "after", Scope: store.ScopeRuntimeBroker, ScopeID: target.ID}
						}
						denied := doRequestAsUser(t, srv, member, method, path, body)
						require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
						saved, err := st.GetEnvVar(ctx, "BOUNDARY_KEY", store.ScopeRuntimeBroker, target.ID)
						require.NoError(t, err)
						require.Equal(t, "before", saved.Value, "a denied member write cannot change broker environment")
						var payload []byte
						if body != nil {
							payload, err = json.Marshal(body)
							require.NoError(t, err)
						}
						req := httptest.NewRequest(method, path, bytes.NewReader(payload)).WithContext(contextWithIdentity(ctx, NewBrokerIdentity(own.ID)))
						if body != nil {
							req.Header.Set("Content-Type", "application/json")
						}
						rec := httptest.NewRecorder()
						if generic {
							if op == "list" {
								srv.handleEnvVars(rec, req)
							} else {
								srv.handleEnvVarByKey(rec, req)
							}
						} else {
							if op == "list" {
								srv.handleBrokerEnvVars(rec, req, target.ID)
							} else {
								srv.handleBrokerEnvVarByKey(rec, req, target.ID, "BOUNDARY_KEY")
							}
						}
						want := http.StatusForbidden
						if target.ID == own.ID {
							want = http.StatusOK
							if op == "delete" {
								want = http.StatusNoContent
							}
						}
						require.Equal(t, want, rec.Code, rec.Body.String())
						if target.ID != own.ID {
							saved, err := st.GetEnvVar(ctx, "BOUNDARY_KEY", store.ScopeRuntimeBroker, target.ID)
							require.NoError(t, err)
							require.Equal(t, "before", saved.Value)
						}
					})
				}
			}
		})
	}
}
