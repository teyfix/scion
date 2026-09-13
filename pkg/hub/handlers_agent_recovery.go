package hub

import (
	"context"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type runtimeRecoveryDispatcher interface {
	DispatchAgentRecover(context.Context, *store.Agent, *api.RuntimeRecovery) error
}

func (s *Server) recoverAgentRuntime(w http.ResponseWriter, r *http.Request, agent *store.Agent, update *api.RuntimeUpdateRequest) {
	ctx := r.Context()
	if err := update.Validate(); err != nil {
		BadRequest(w, err.Error())
		return
	}
	if agent.StateVersion != *update.StateVersion {
		Conflict(w, "Agent stateVersion is stale; refresh the agent before retrying runtime recovery")
		return
	}
	if agent.Phase != string(state.PhaseSuspended) && agent.Phase != string(state.PhaseStopped) && agent.Phase != string(state.PhaseError) {
		Conflict(w, "Runtime recovery requires a suspended, stopped, or error agent")
		return
	}
	if agent.AppliedConfig == nil || agent.RuntimeBrokerID == "" || isManagedAgentRuntime(agent.Runtime) {
		BadRequest(w, "Runtime recovery requires retained configuration and a container runtime broker")
		return
	}
	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}
	if ok, reason := s.harnessSupportsResume(agent); !ok {
		BadRequest(w, "Cannot recover retained session: "+reason)
		return
	}
	dispatcher, ok := s.GetDispatcher().(runtimeRecoveryDispatcher)
	if !ok {
		BadRequest(w, "Runtime dispatcher does not support explicit retained recovery")
		return
	}
	template, err := s.resolveTemplate(ctx, update.Template, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	recovery := &api.RuntimeRecovery{
		Update: *update, AgentID: agent.ID, ProjectID: agent.ProjectID,
		RuntimeBrokerID: agent.RuntimeBrokerID, AdmissionVersion: agent.StateVersion + 1,
	}
	if template != nil {
		if template.Scope == "project" && template.ScopeID != agent.ProjectID {
			BadRequest(w, "Recovery template belongs to another project")
			return
		}
		recovery.Update.Template = template.Slug
		recovery.TemplateID, recovery.TemplateHash = template.ID, template.ContentHash
	}
	// Admit through the existing optimistic lock before any broker mutation.
	// Keep effective configuration unchanged until the broker reports success.
	applied := *agent.AppliedConfig
	applied.RuntimeUpdateVersion = recovery.AdmissionVersion
	agent.AppliedConfig = &applied
	agent.Phase = string(state.PhaseStarting)
	agent.Message = ""
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.events.PublishAgentStatus(ctx, agent)
	if err := dispatcher.DispatchAgentRecover(ctx, agent, recovery); err != nil {
		agent.Phase = string(state.PhaseError)
		agent.Message = "Runtime recovery failed: " + err.Error()
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchRollingTimeout)
		defer cancel()
		if persistErr := s.completeRuntimeRecovery(persistCtx, agent, recovery.AdmissionVersion); persistErr != nil {
			s.agentLifecycleLog.Error("Failed to record runtime recovery error", "agent_id", agent.ID, "error", persistErr)
		}
		RuntimeError(w, agent.Message)
		return
	}
	if err := s.completeRuntimeRecovery(ctx, agent, recovery.AdmissionVersion); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// completeRuntimeRecovery fences late results against a newer admitted retry.
func (s *Server) completeRuntimeRecovery(ctx context.Context, agent *store.Agent, admissionVersion int64) error {
	latest, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		return err
	}
	if latest.AppliedConfig == nil || latest.AppliedConfig.RuntimeUpdateVersion != admissionVersion {
		return store.ErrVersionConflict
	}
	agent.StateVersion = latest.StateVersion
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		return err
	}
	s.events.PublishAgentStatus(ctx, agent)
	return nil
}

func (d *HTTPAgentDispatcher) DispatchAgentRecover(ctx context.Context, agent *store.Agent, recovery *api.RuntimeRecovery) error {
	if agent.ID != recovery.AgentID || agent.ProjectID != recovery.ProjectID || agent.RuntimeBrokerID != recovery.RuntimeBrokerID || agent.AppliedConfig == nil || agent.AppliedConfig.RuntimeUpdateVersion != recovery.AdmissionVersion {
		return fmt.Errorf("runtime recovery admission is stale or identity does not match")
	}
	working := *agent
	applied := *agent.AppliedConfig
	working.AppliedConfig = &applied
	applied.InlineConfig = config.MergeScionConfig(applied.InlineConfig, recovery.Update.Config)
	applied.Image = recovery.Update.Image
	if recovery.Update.Config != nil {
		c := recovery.Update.Config
		applied.Env = config.MergeScionConfig(&api.ScionConfig{Env: applied.Env}, &api.ScionConfig{Env: c.Env}).Env
		if c.Model != "" {
			applied.Model = c.Model
		}
		if c.ThinkingLevel != nil {
			applied.ThinkingLevel = c.ThinkingLevel
		}
	}
	working.Template = recovery.Update.Template
	applied.TemplateID, applied.TemplateHash = recovery.TemplateID, recovery.TemplateHash
	if err := d.DispatchAgentStart(api.ContextWithRuntimeRecovery(ctx, recovery), &working, "", true); err != nil {
		return err
	}
	if recovery.Update.Labels != nil {
		working.Labels = recovery.Update.Labels
	}
	*agent = working
	return nil
}
