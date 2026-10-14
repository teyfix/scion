//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecoveryImagePersistsForOrdinaryResume(t *testing.T) {
	for _, shape := range []string{"explicit", "already-recovered-stale-inputs", "empty-inputs", "legacy"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Image           string               `json:"image"`
					Resume          bool                 `json:"resume"`
					Task            string               `json:"task"`
					ResolvedEnv     map[string]string    `json:"resolvedEnv"`
					InlineConfig    *api.ScionConfig     `json:"inlineConfig"`
					RuntimeRecovery *api.RuntimeRecovery `json:"runtimeRecovery"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.True(t, request.Resume)
				if calls.Add(1) == 1 {
					require.NotNil(t, request.RuntimeRecovery)
				} else {
					require.Nil(t, request.RuntimeRecovery, "the next start must use ordinary resume")
					require.Equal(t, "new:latest", request.Image)
					require.Equal(t, "new:latest", request.InlineConfig.Image)
					require.Empty(t, request.Task, "ordinary resume must not replay the original task")
					require.Equal(t, "updated-model", request.InlineConfig.Model)
					require.Equal(t, "preview.test", request.ResolvedEnv["APP_DOMAIN"])
					require.Equal(t, "true", request.InlineConfig.Env["RETAINED"])
					require.Equal(t, []string{"preview"}, request.InlineConfig.Docker.Networks)
					require.Equal(t, "true", request.InlineConfig.Docker.Labels["traefik.enable"])
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: "recovered-container", Name: "retained", Phase: "running", Image: "new:latest", Template: "current", ContainerStatus: "Up"}}))
			})
			agent.Image = "old:latest"
			agent.AppliedConfig.Task = "original useful task"
			update.Config.Model = "updated-model"
			agent.AppliedConfig.InlineConfig.Image = "old:latest"
			if shape != "legacy" {
				agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{Profile: "retained-profile", Branch: "retained-branch", HarnessAuth: "auth-file"}
				if shape != "empty-inputs" {
					agent.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "old:latest", Model: "explicit-model", Env: map[string]string{"EXPLICIT": "retained"}}
				}
			}
			if shape == "already-recovered-stale-inputs" {
				// The released bug already recorded the current image in these
				// fields, while display and explicit dispatch inputs stayed old.
				agent.AppliedConfig.Image = "new:latest"
				agent.AppliedConfig.InlineConfig.Image = "new:latest"
			}
			require.NoError(t, db.UpdateAgent(ctx, agent))
			version := agent.StateVersion
			update.StateVersion = &version
			// Image is explicit in runtimeUpdate, even when config omits it.
			require.Empty(t, update.Config.Image)
			recovery := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
			require.Equal(t, http.StatusOK, recovery.Code, recovery.Body.String())
			var response store.Agent
			require.NoError(t, json.Unmarshal(recovery.Body.Bytes(), &response))
			require.Equal(t, "new:latest", response.Image, "API/UI runtime image must reflect recovery")
			saved, err := db.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			require.Equal(t, agent.ID, saved.ID)
			require.Equal(t, agent.ProjectID, saved.ProjectID)
			require.Equal(t, agent.RuntimeBrokerID, saved.RuntimeBrokerID)
			require.Equal(t, "original useful task", saved.AppliedConfig.Task)
			require.Equal(t, "updated-model", saved.AppliedConfig.Model)
			require.Equal(t, "preview.test", saved.AppliedConfig.Env["APP_DOMAIN"])
			require.Equal(t, update.Config.Docker, saved.AppliedConfig.Docker)
			require.Equal(t, "new:latest", saved.Image)
			require.Equal(t, "new:latest", saved.AppliedConfig.Image)
			require.Equal(t, "new:latest", saved.AppliedConfig.InlineConfig.Image)
			require.Equal(t, "new:latest", explicitDispatchImage(saved.AppliedConfig))
			if shape == "legacy" {
				require.Nil(t, saved.AppliedConfig.CreateInputs, "do not invent a legacy creation snapshot")
			} else {
				require.Equal(t, "retained-profile", saved.AppliedConfig.CreateInputs.Profile)
				require.Equal(t, "retained-branch", saved.AppliedConfig.CreateInputs.Branch)
				require.Equal(t, "auth-file", saved.AppliedConfig.CreateInputs.HarnessAuth)
				if shape != "empty-inputs" {
					require.Equal(t, "explicit-model", saved.AppliedConfig.CreateInputs.InlineConfig.Model)
					require.Equal(t, map[string]string{"EXPLICIT": "retained"}, saved.AppliedConfig.CreateInputs.InlineConfig.Env)
				}
			}
			saved.Phase = "suspended"
			require.NoError(t, db.UpdateAgent(ctx, saved))
			resume := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", map[string]any{})
			require.Equal(t, http.StatusOK, resume.Code, resume.Body.String())
			require.Equal(t, int32(2), calls.Load())
			afterResume, err := db.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			require.Equal(t, agent.ID, afterResume.ID)
			require.Equal(t, agent.ProjectID, afterResume.ProjectID)
			require.Equal(t, agent.RuntimeBrokerID, afterResume.RuntimeBrokerID)
			require.Equal(t, "original useful task", afterResume.AppliedConfig.Task)
		})
	}
}

func TestRuntimeRecoveryImageFailureDoesNotMutateRetainedInputs(t *testing.T) {
	ctx := context.Background()
	_, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "recreation failed", http.StatusInternalServerError)
	})
	agent.Image = "old:latest"
	agent.AppliedConfig.InlineConfig.Image = "old:latest"
	agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Image: "old:latest", Env: map[string]string{"RETAINED": "true"}}}
	require.NoError(t, db.UpdateAgent(ctx, agent))
	version := agent.StateVersion
	update.StateVersion = &version
	admitted, err := db.AdmitAgentRuntimeRecovery(ctx, agent.ID, version, version+1)
	require.NoError(t, err)
	originalInputs := admitted.AppliedConfig.CreateInputs
	recovery := &api.RuntimeRecovery{Update: *update, AgentID: agent.ID, ProjectID: agent.ProjectID, RuntimeBrokerID: agent.RuntimeBrokerID, AdmissionVersion: admitted.StateVersion}
	err = NewHTTPAgentDispatcher(db, false, slog.Default()).DispatchAgentRecover(ctx, admitted, recovery)
	require.Error(t, err)
	require.Equal(t, "old:latest", admitted.Image)
	require.Same(t, originalInputs, admitted.AppliedConfig.CreateInputs)
	require.Equal(t, "old:latest", originalInputs.InlineConfig.Image, "failed dispatch must not mutate a shared creation-input pointer")
	require.Equal(t, "old:latest", admitted.AppliedConfig.InlineConfig.Image)
	saved, err := db.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, "old:latest", saved.Image)
	require.Equal(t, "old:latest", explicitDispatchImage(saved.AppliedConfig))
}
