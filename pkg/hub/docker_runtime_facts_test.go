package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"testing"
)

func TestApplyAgentRuntimeFacts(t *testing.T) {
	requested := true
	agent := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Docker: &api.DockerConfig{Privileged: &requested}}}}
	observed := false
	if !applyAgentRuntimeFacts(agent, &observed, &observed) {
		t.Fatal("expected observed facts to change")
	}
	if agent.AppliedConfig.Docker.Privileged == nil || *agent.AppliedConfig.Docker.Privileged || agent.AppliedConfig.NvidiaGPU == nil || *agent.AppliedConfig.NvidiaGPU {
		t.Fatal("observed false must be stored")
	}
	if !*agent.AppliedConfig.InlineConfig.Docker.Privileged {
		t.Fatal("observations rewrote explicit config")
	}
	if applyAgentRuntimeFacts(agent, &observed, &observed) {
		t.Fatal("same facts should not cause writeback")
	}
	if applyAgentRuntimeFacts(agent, nil, nil) {
		t.Fatal("unknown facts should not clear observed state")
	}
}
