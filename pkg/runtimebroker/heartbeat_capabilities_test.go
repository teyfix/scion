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
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestHeartbeatCapabilities_StaticSetPlusTemplateTiers: a heartbeat reports
// exactly the static capability set plus TemplateTiers, while the static set
// itself (also written at in-process registration) and the registration
// capability names carry no TemplateTiers and are unchanged.
func TestHeartbeatCapabilities_StaticSetPlusTemplateTiers(t *testing.T) {
	t.Setenv("SCION_NVIDIA_GPU", "")
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 || calls[0].Heartbeat.Capabilities == nil {
		t.Fatalf("expected 1 heartbeat with capabilities, got %d calls", len(calls))
	}
	want := StaticCapabilities(nil)
	want.TemplateTiers = true
	if got := calls[0].Heartbeat.Capabilities; !reflect.DeepEqual(got, want) {
		t.Errorf("heartbeat capabilities = %+v, want the static set plus TemplateTiers %+v", got, want)
	}

	if StaticCapabilities(nil).TemplateTiers {
		t.Error("the static set (sent at registration) must not carry TemplateTiers")
	}
	wantNames := []string{"sync", "attach", "reprovision", "asyncLaunch", "emptyPerAgentWorkspace", "agentMove", "reprovisionEmptyPerAgent", "runtimeRecovery"}
	if got := StaticCapabilityNames(nil); !reflect.DeepEqual(got, wantNames) {
		t.Errorf("registration capability names = %v, want %v (unchanged)", got, wantNames)
	}
}

func TestStaticCapabilities_ExplicitNvidiaGPU(t *testing.T) {
	for _, value := range []string{"", "false", "TRUE", "true"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SCION_NVIDIA_GPU", value)
			want := value == "true"
			if got := StaticCapabilities(nil).NvidiaGPU; got != want {
				t.Fatalf("NvidiaGPU=%v want=%v", got, want)
			}
			if got := slices.Contains(StaticCapabilityNames(nil), "nvidiaGpu"); got != want {
				t.Fatalf("registration nvidiaGpu=%v want=%v", got, want)
			}
		})
	}
}

func TestHeartbeat_PreservesObservedPrivilegesAndGPU(t *testing.T) {
	privileged, gpu := false, true
	mgr := &heartbeatMockManager{agents: []api.AgentInfo{{Name: "agent-a", ProjectID: "project-a", Phase: "running", Privileged: &privileged, NvidiaGPU: &gpu}}}
	svc := NewHeartbeatService(&mockRuntimeBrokerService{}, "broker-a", time.Hour, mgr, nil, slog.Default())
	projects, _ := svc.gatherProjectAgents(context.Background(), nil)
	if len(projects) != 1 || len(projects[0].Agents) != 1 {
		t.Fatalf("unexpected inventory: %+v", projects)
	}
	got := projects[0].Agents[0]
	if got.Privileged == nil || *got.Privileged || got.NvidiaGPU == nil || !*got.NvidiaGPU {
		t.Fatalf("observed runtime flags not preserved: %+v", got)
	}
}
