//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func setupRuntimeRecoveryHub(t *testing.T, handler http.HandlerFunc) (*Server, store.Store, *store.Agent, *api.RuntimeUpdateRequest) {
	t.Helper()
	srv, db := testServer(t)
	_, broker, agent := setupOnlineBrokerAgent(t, db, "runtime-recovery")
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	broker.Endpoint = backend.URL
	require.NoError(t, db.UpdateRuntimeBroker(context.Background(), broker))
	agent.Phase = "suspended"
	agent.Template = "previous"
	agent.AppliedConfig = &store.AgentAppliedConfig{Image: "old:latest", HarnessConfig: "claude", Branch: "retained-branch", Workspace: "/retained/workspace", CreatorName: "original-creator", AgentRole: "baseline", InlineConfig: &api.ScionConfig{Env: map[string]string{"RETAINED": "true"}}}
	require.NoError(t, db.UpdateAgent(context.Background(), agent))
	srv.SetDispatcher(NewHTTPAgentDispatcher(db, false, slog.Default()))
	version := agent.StateVersion
	update := &api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "new:latest", Config: &api.ScionConfig{Env: map[string]string{"APP_DOMAIN": "preview.test"}, Docker: &api.DockerConfig{Networks: []string{"preview"}, Labels: map[string]string{"traefik.enable": "true"}}}}
	return srv, db, agent, update
}

func recoveryStartBody(update *api.RuntimeUpdateRequest) any {
	return map[string]any{"runtimeUpdate": update}
}

func writeSuccessfulRecovery(t *testing.T, w http.ResponseWriter, r *http.Request) *api.RuntimeRecovery {
	t.Helper()
	var req struct {
		RuntimeRecovery *api.RuntimeRecovery `json:"runtimeRecovery"`
		Resume          bool                 `json:"resume"`
		ResolvedEnv     map[string]string    `json:"resolvedEnv"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
	require.NotNil(t, req.RuntimeRecovery)
	require.True(t, req.Resume)
	require.Equal(t, "preview.test", req.ResolvedEnv["APP_DOMAIN"])
	require.Equal(t, "new:latest", req.RuntimeRecovery.Update.Image)
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: "new-container", Name: "retained", Phase: "running", Image: "new:latest", Template: "current", ContainerStatus: "Up"}}))
	return req.RuntimeRecovery
}

func TestRetainedRuntimeRecoveryHubAdmitsCASAndRetainsIdentity(t *testing.T) {
	var db store.Store
	var original *store.Agent
	srv, storage, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		// Ordinary status reports can advance StateVersion after admission.
		require.NoError(t, db.UpdateAgentStatus(context.Background(), original.ID, store.AgentStatusUpdate{Activity: "thinking"}))
		recovery := writeSuccessfulRecovery(t, w, r)
		require.Equal(t, original.ID, recovery.AgentID)
		require.Equal(t, original.ProjectID, recovery.ProjectID)
		require.Equal(t, original.RuntimeBrokerID, recovery.RuntimeBrokerID)
	})
	db, original = storage, agent
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, agent.ID, saved.ID)
	require.Equal(t, agent.ProjectID, saved.ProjectID)
	require.Equal(t, agent.RuntimeBrokerID, saved.RuntimeBrokerID)
	require.Equal(t, "running", saved.Phase)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "current", saved.Template)
	require.Equal(t, "retained-branch", saved.AppliedConfig.Branch)
	require.Equal(t, "/retained/workspace", saved.AppliedConfig.Workspace)
	require.Equal(t, "original-creator", saved.AppliedConfig.CreatorName)
	require.Equal(t, "baseline", saved.AppliedConfig.AgentRole)
	require.Equal(t, *update.StateVersion+1, saved.AppliedConfig.RuntimeUpdateVersion)
}

func TestRetainedRuntimeRecoveryConcurrentDifferentTargetsConflictBeforeDispatch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		starts.Add(1)
		close(entered)
		<-release
		writeSuccessfulRecovery(t, w, r)
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	}()
	select {
	case <-entered:
	case first := <-firstDone:
		t.Fatalf("recovery never reached broker: status=%d body=%s", first.Code, first.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not reach broker within test timeout")
	}
	other := *update
	other.Image = "another:latest"
	second := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(&other))
	require.Equal(t, http.StatusConflict, second.Code, second.Body.String())
	close(release)
	first := <-firstDone
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, int32(1), starts.Load())
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
}

func TestRetainedRuntimeRecoveryHubFailureVisibleAndFreshVersionRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "image unavailable", http.StatusBadRequest)
			return
		}
		writeSuccessfulRecovery(t, w, r)
	})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.GreaterOrEqual(t, rec.Code, 500)
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "error", saved.Phase)
	require.Contains(t, saved.Message, "Runtime recovery failed")
	require.Equal(t, "old:latest", saved.AppliedConfig.Image)
	require.Equal(t, "previous", saved.Template)
	stale := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusConflict, stale.Code)
	version := saved.StateVersion
	update.StateVersion = &version
	fail.Store(false)
	retry := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
}

func TestRetainedRuntimeRecoveryLateCompletionAndIntentFenced(t *testing.T) {
	srv, db, agent, _ := setupRuntimeRecoveryHub(t, func(http.ResponseWriter, *http.Request) { t.Fatal("superseded intent must never reach broker") })
	agent.AppliedConfig.RuntimeUpdateVersion = 20
	require.NoError(t, db.UpdateAgent(context.Background(), agent))
	old := *agent
	old.AppliedConfig = &store.AgentAppliedConfig{RuntimeUpdateVersion: 10, Image: "obsolete:latest"}
	require.ErrorIs(t, srv.completeRuntimeRecovery(context.Background(), &old, 10), store.ErrVersionConflict)
	version := int64(9)
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Resume: true, RuntimeRecovery: &api.RuntimeRecovery{Update: api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "obsolete:latest"}, AdmissionVersion: 10, AgentID: agent.ID, ProjectID: agent.ProjectID, RuntimeBrokerID: agent.RuntimeBrokerID}})
	require.NoError(t, err)
	_, err = srv.executeDispatch(context.Background(), store.BrokerDispatch{AgentID: agent.ID, ProjectID: agent.ProjectID, BrokerID: agent.RuntimeBrokerID, Op: "start", Args: args})
	require.ErrorContains(t, err, "stale")
}
