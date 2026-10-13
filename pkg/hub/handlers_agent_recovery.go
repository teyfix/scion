package hub

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"time"

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
	// A runtime update changes configuration. As for reincarnate patches,
	// user/session and UAT callers need agent.update as well as lifecycle authority.
	if GetUserIdentityFromContext(ctx) != nil && !s.authorizeMsg(w, r, agentResource(agent), ActionUpdate, "Runtime recovery with configuration changes requires agent.update permission") {
		return
	}
	if err := update.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		return
	}
	if agent.StateVersion != *update.StateVersion {
		writeError(w, http.StatusConflict, ErrCodeConflict, "Agent stateVersion is stale; refresh the agent before retrying runtime recovery", nil)
		return
	}
	if agent.Phase != string(state.PhaseSuspended) && agent.Phase != string(state.PhaseStopped) && agent.Phase != string(state.PhaseError) {
		writeError(w, http.StatusConflict, ErrCodeConflict, "Runtime recovery requires a suspended, stopped, or error agent", nil)
		return
	}
	if agent.AppliedConfig == nil || agent.RuntimeBrokerID == "" || isManagedAgentRuntime(agent.Runtime) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Runtime recovery requires retained configuration and a container runtime broker", nil)
		return
	}
	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if broker.Capabilities == nil || !broker.Capabilities.RuntimeRecovery {
		writeError(w, http.StatusPreconditionFailed, ErrCodeInvalidRequest, "Runtime broker does not support retained runtime recovery", nil)
		return
	}
	if ok, reason := s.harnessSupportsResume(agent); !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Cannot recover retained session: "+reason, nil)
		return
	}
	dispatcher, ok := s.GetDispatcher().(runtimeRecoveryDispatcher)
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Runtime dispatcher does not support explicit retained recovery", nil)
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
		if !s.authorizeResolvedTemplate(ctx, GetIdentityFromContext(ctx), template) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Not authorized to use the requested recovery template", nil)
			return
		}
		if template.Scope == "project" && template.ScopeID != agent.ProjectID {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Recovery template belongs to another project", nil)
			return
		}
		recovery.Update.Template = template.Slug
		recovery.TemplateID, recovery.TemplateHash = template.ID, template.ContentHash
	}
	// The same start claim, lease, capacity reservation and self-fence as
	// ordinary starts wrap admission, dispatch and the committed result.
	ctx = detachLaunchFromClient(ctx)
	extendWriteDeadlineForSyncDispatch(ctx, w, s.config.WriteTimeout)
	admitted := false
	err = s.startAgentCore(ctx, agent, StartOpts{
		Kind: store.StartClaimUser, Resume: true, NewGeneration: true, SyncDispatchBound: true,
		DispatchStart: func(dispatchCtx context.Context) error {
			current, err := s.store.AdmitAgentRuntimeRecovery(dispatchCtx, agent.ID, *update.StateVersion, recovery.AdmissionVersion)
			if err != nil {
				return err
			}
			*agent = *current
			admitted = true
			s.events.PublishAgentStatus(dispatchCtx, agent)
			if err := dispatcher.DispatchAgentRecover(dispatchCtx, agent, recovery); err != nil {
				agent.Phase = string(state.PhaseError)
				agent.Message = "Runtime recovery failed: " + err.Error()
				persistCtx, cancel := context.WithTimeout(context.WithoutCancel(dispatchCtx), 2*time.Minute)
				defer cancel()
				if persistErr := s.completeRuntimeRecovery(persistCtx, agent, recovery); persistErr != nil {
					s.agentLifecycleLog.Error("Failed to record runtime recovery error", "agent_id", agent.ID, "error", persistErr)
				}
				return err
			}
			return nil
		},
		AfterStart: func(finalCtx context.Context, st startedState) error {
			if err := s.completeRuntimeRecovery(finalCtx, agent, recovery, st.ClearRemnants); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		if writeStartQuotaError(w, err) {
			return
		}
		if !admitted {
			writeErrorFromErr(w, err, "")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "Runtime recovery failed: "+err.Error(), nil)
		return
	}
	response := *agent
	response.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	writeJSON(w, http.StatusOK, &response)
}

// completeRuntimeRecovery fences late results against a newer admitted retry.
func (s *Server) completeRuntimeRecovery(ctx context.Context, agent *store.Agent, recovery *api.RuntimeRecovery, clearRemnants ...bool) error {
	clear := len(clearRemnants) > 0 && clearRemnants[0]
	for attempt := 0; attempt < 3; attempt++ {
		err := s.completeRuntimeRecoveryAttempt(ctx, agent, recovery, clear)
		if !errors.Is(err, store.ErrVersionConflict) {
			return err
		}
	}
	return store.ErrVersionConflict
}

func (s *Server) completeRuntimeRecoveryAttempt(ctx context.Context, agent *store.Agent, recovery *api.RuntimeRecovery, clearRemnants bool) error {
	latest, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		return err
	}
	if latest.AppliedConfig == nil || latest.AppliedConfig.RuntimeUpdateVersion != recovery.AdmissionVersion {
		return store.ErrVersionConflict
	}
	if agent.Phase == string(state.PhaseError) && (latest.Template != agent.Template || latest.Image != agent.Image || !reflect.DeepEqual(latest.AppliedConfig, agent.AppliedConfig)) {
		// Cancellation can race the owner's committed result. A failure from
		// the unchanged admission snapshot cannot undo that effective config.
		*agent = *latest
		return nil
	}
	// The owner may already have committed this result while the requester
	// waited for the durable intent. Do not replay its snapshot over newer
	// status reports or lifecycle actions.
	labelsMatch := recovery.Update.Labels == nil || reflect.DeepEqual(latest.Labels, recovery.Update.Labels)
	if agent.Phase != string(state.PhaseError) && latest.Template == agent.Template && latest.Image == agent.Image && reflect.DeepEqual(latest.AppliedConfig, agent.AppliedConfig) && labelsMatch && latest.Phase != string(state.PhaseStarting) {
		*agent = *latest
		return nil
	}
	// Status reports do not increment StateVersion. Configuration/metadata
	// edits do: keep the dispatch snapshot's expected version so a concurrent
	// PATCH cannot be overwritten by a late recovery result.
	result := *agent
	if recovery.Update.Labels != nil {
		result.Labels = recovery.Update.Labels
	}
	if err := s.store.UpdateAgentRuntimeRecovery(ctx, &result, recovery.AdmissionVersion, recovery.Update.Labels != nil, clearRemnants); err != nil {
		return err
	}
	*agent = result
	s.events.PublishAgentStatus(ctx, agent)
	return nil
}

func (d *HTTPAgentDispatcher) DispatchAgentRecover(ctx context.Context, agent *store.Agent, recovery *api.RuntimeRecovery) error {
	if agent.StateVersion != recovery.AdmissionVersion || agent.ID != recovery.AgentID || agent.ProjectID != recovery.ProjectID || agent.RuntimeBrokerID != recovery.RuntimeBrokerID || agent.AppliedConfig == nil || agent.AppliedConfig.RuntimeUpdateVersion != recovery.AdmissionVersion {
		return fmt.Errorf("runtime recovery admission is stale or identity does not match")
	}
	latest, err := d.store.GetAgent(ctx, agent.ID)
	if err != nil {
		return err
	}
	if latest.StateVersion != recovery.AdmissionVersion || latest.AppliedConfig == nil || latest.AppliedConfig.RuntimeUpdateVersion != recovery.AdmissionVersion || latest.RuntimeBrokerID != recovery.RuntimeBrokerID || latest.ProjectID != recovery.ProjectID {
		return fmt.Errorf("runtime recovery admission is stale or identity does not match")
	}
	working := *agent
	applied := *agent.AppliedConfig
	working.AppliedConfig = &applied
	// Templates contribute job scalars; their tool env stays the broker's
	// literal lowest-precedence FallbackEnv and never reaches credential gathering.
	requested := &api.ScionConfig{}
	if recovery.TemplateID != "" {
		template, err := d.store.GetTemplate(ctx, recovery.TemplateID)
		if err != nil {
			return err
		}
		if template.ContentHash != recovery.TemplateHash {
			return fmt.Errorf("requested recovery template changed after admission")
		}
		if template.AgentConfig != nil {
			requested.Model, requested.ThinkingLevel = template.AgentConfig.Model, template.AgentConfig.ThinkingLevel
		} else if template.Config != nil {
			// Older templates have only the reduced configuration index.
			requested.Model = template.Config.Model
		}
	}
	requested = config.MergeScionConfig(requested, recovery.Update.Config)
	applied.Env = maps.Clone(applied.Env)
	if applied.InlineConfig != nil {
		inline := *applied.InlineConfig
		inline.Env = maps.Clone(inline.Env)
		applied.InlineConfig = &inline
	}
	if requested.Model != "" {
		applied.Model = requested.Model
		if requested.Env["SCION_MODEL"] == "" {
			delete(applied.Env, "SCION_MODEL")
			if applied.InlineConfig != nil {
				delete(applied.InlineConfig.Env, "SCION_MODEL")
			}
		}
	}
	if requested.ThinkingLevel != nil {
		applied.ThinkingLevel = requested.ThinkingLevel
		if requested.Env["SCION_THINKING_LEVEL"] == "" {
			delete(applied.Env, "SCION_THINKING_LEVEL")
			if applied.InlineConfig != nil {
				delete(applied.InlineConfig.Env, "SCION_THINKING_LEVEL")
			}
		}
	}
	applied.InlineConfig = config.MergeScionConfig(applied.InlineConfig, requested)
	applied.Env = config.MergeScionConfig(&api.ScionConfig{Env: applied.Env}, &api.ScionConfig{Env: requested.Env}).Env
	applied.Docker = config.MergeDockerConfig(applied.Docker, requested.Docker)
	applied.Image = recovery.Update.Image
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
