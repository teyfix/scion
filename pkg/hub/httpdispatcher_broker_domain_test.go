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
	"context"
	"strings"
	"testing"

	agentpkg "github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker domains come from Hub storage, including when a template declares
// no environment value. Ambient host domains must not leak across brokers.
func TestResolveEnvFromStorage_BrokerDomainsExpandDockerLabels(t *testing.T) {
	t.Setenv("APP_DOMAIN", "ambient.invalid")
	ctx := context.Background()
	d, st := newEnvScopeDispatcher(t, "UNUSED", nil)
	for brokerID, domain := range map[string]string{
		"broker-dixie": "manga.tail.gg",
		"broker-other": "steinfor.gg",
	} {
		if _, err := st.UpsertEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "APP_DOMAIN", Value: domain,
			Scope: store.ScopeRuntimeBroker, ScopeID: brokerID,
			InjectionMode: store.InjectionModeAlways,
		}); err != nil {
			t.Fatalf("seed broker domain: %v", err)
		}
	}

	const labelKey = "traefik.http.routers.${SCION_AGENT_SLUG}.rule"
	labels := map[string]string{labelKey: "Host(`${SCION_AGENT_SLUG}.${APP_DOMAIN}`)"}
	for _, tc := range []struct {
		brokerID string
		domain   string
	}{
		{"broker-dixie", "manga.tail.gg"},
		{"broker-other", "steinfor.gg"},
		{"broker-missing", ""},
	} {
		t.Run(tc.brokerID, func(t *testing.T) {
			agent := envScopeTestAgent()
			agent.RuntimeBrokerID = tc.brokerID
			resolved, _, err := d.resolveEnvFromStorage(ctx, agent)
			if err != nil {
				t.Fatalf("resolve broker domain: %v", err)
			}
			vars := agentpkg.BuildScopedLabelVars("Role Worker", agent.ID, "animatrix", agent.ProjectID, resolved)
			got, err := agentpkg.ExpandAndValidateDockerLabels(labels, vars)
			if tc.domain == "" {
				if err == nil || !strings.Contains(err.Error(), "unresolved variable ${APP_DOMAIN}") {
					t.Fatalf("missing broker domain: labels=%v, error=%v; want unresolved APP_DOMAIN", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expand broker domain label: %v", err)
			}
			if value := got["traefik.http.routers.role-worker.rule"]; value != "Host(`role-worker."+tc.domain+"`)" {
				t.Errorf("router rule = %q, want broker domain %q", value, tc.domain)
			}
		})
	}
}
