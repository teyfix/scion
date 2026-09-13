//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
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

// The thinking fixture executes real broker/manager configuration handling;
// synthetic container creation is the only external runtime effect. Auth has
// its separate auth-enabled retained-secret regression in pkg/agent.
type recoveryThinkingManager struct{ agent.Manager }

func (m recoveryThinkingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	opts.NoAuth = true
	return m.Manager.Start(ctx, opts)
}

func TestRetainedRuntimeRecoveryThinkingAndModelSurviveOrdinaryHubResume(t *testing.T) {
	for _, mode := range []string{"template", "explicit", "zero", "env", "retained"} {
		t.Run(mode, func(t *testing.T) {
			srv, db := testServer(t)
			_, broker, ag := setupOnlineBrokerAgent(t, db, "thinking-recovery")
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("SCION_PROJECT", "")
			t.Setenv("SCION_GROVE", "")
			project := filepath.Join(root, "project", ".scion")
			dir := config.ResolveAgentDir(project, ag.Slug)
			home, workspace := filepath.Join(dir, "home"), filepath.Join(root, "workspace")
			harnessDir := filepath.Join(root, ".scion", "harness-configs", "codex")
			templateDir := filepath.Join(root, ".scion", "templates", "current")
			for _, path := range []string{home, workspace, harnessDir, templateDir} {
				require.NoError(t, os.MkdirAll(path, 0755))
			}
			require.NoError(t, os.WriteFile(filepath.Join(harnessDir, "config.yaml"), []byte("harness: codex\nuser: root\nimage: old:latest\nconfig_dir: .codex\nprovisioner:\n  type: container-script\n  interface_version: 1\n  command: [python3, provision.py]\n  lifecycle_events: [pre-start]\ncommand:\n  base: [codex]\n  resume_flag: resume --last\ncapabilities:\n  resume:\n    support: yes\n"), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(harnessDir, "provision.py"), []byte("# synthetic runtime fixture\n"), 0644))
			templateConfig := `{"harness":"codex","harness_config":"codex","thinking_level":6,"model":"template-model"}`
			if mode == "retained" {
				templateConfig = `{"harness":"codex","harness_config":"codex","thinking_level":6}`
			}
			require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.json"), []byte(templateConfig), 0644))
			one := 1
			old := &api.ScionConfig{Harness: "codex", HarnessConfig: "codex", User: "root", Image: "old:latest", ExplicitWorkspace: true, ThinkingLevel: &one, Model: "different-old-scalar",
				Env:     map[string]string{"SCION_AGENT_ID": ag.ID, "SCION_PROJECT_ID": ag.ProjectID, "SCION_THINKING_LEVEL": "1", "SCION_MODEL": "old-model"},
				Volumes: []api.VolumeMount{{Source: workspace, Target: "/workspace"}},
				Info:    &api.AgentInfo{ID: ag.ID, Name: ag.Slug, ProjectID: ag.ProjectID, RuntimeBrokerID: broker.ID, Template: "previous", Phase: "suspended", Image: "old:latest"},
			}
			if mode == "retained" {
				t.Setenv("RECOVERY_RETAINED_MODEL", "old-model")
				old.Env["SCION_MODEL"] = "${RECOVERY_RETAINED_MODEL}"
			}
			chain, err := config.GetTemplateChainInProject("current", project)
			require.NoError(t, err)
			defaults := &api.ScionConfig{}
			for _, template := range chain {
				cfg, err := template.LoadConfig()
				require.NoError(t, err)
				defaults = config.MergeScionConfig(defaults, cfg)
			}
			old = config.MergeScionConfig(defaults, old)
			data, err := json.Marshal(old)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.json"), data, 0644))
			data, err = json.Marshal(old.Info)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(home, "agent-info.json"), data, 0644))
			var containers []api.AgentInfo
			var runs []runtime.RunConfig
			rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return containers, nil }, RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
				runs = append(runs, cfg)
				containers = []api.AgentInfo{{Name: ag.Slug, ContainerID: "synthetic-container", Phase: "running", Image: cfg.Image, Template: cfg.Template, Labels: cfg.Labels}}
				return "synthetic-container", nil
			}}
			bs := runtimebroker.New(runtimebroker.ServerConfig{BrokerID: broker.ID, StateDir: t.TempDir(), ForceRuntime: rt.Name()}, recoveryThinkingManager{agent.NewManager(rt)}, rt)
			backend := httptest.NewServer(bs.Handler())
			t.Cleanup(backend.Close)
			t.Cleanup(func() { _ = bs.Shutdown(context.Background()) })
			broker.Endpoint = backend.URL
			require.NoError(t, db.UpdateRuntimeBroker(context.Background(), broker))
			require.NoError(t, db.AddProjectProvider(context.Background(), &store.ProjectProvider{ProjectID: ag.ProjectID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: project, Status: "online"}))
			ag.Phase, ag.Template = "suspended", "previous"
			ag.AppliedConfig = &store.AgentAppliedConfig{Image: "old:latest", HarnessConfig: "codex", ThinkingLevel: &one, Model: "old-model",
				Env: map[string]string{"SCION_THINKING_LEVEL": "1", "SCION_MODEL": "old-model"}, InlineConfig: &api.ScionConfig{ThinkingLevel: &one, Model: "old-model", Env: map[string]string{"SCION_THINKING_LEVEL": "1", "SCION_MODEL": "old-model"}}}
			require.NoError(t, db.UpdateAgent(context.Background(), ag))
			srv.SetDispatcher(NewHTTPAgentDispatcher(db, false, slog.Default()))
			version := ag.StateVersion
			update := &api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "new:latest"}
			want := "6"
			wantModel := "template-model"
			if mode == "retained" {
				wantModel = "old-model"
			}
			switch mode {
			case "explicit", "zero":
				level := 8
				if mode == "zero" {
					level = 0
				}
				update.Config = &api.ScionConfig{ThinkingLevel: &level, Model: "current-model"}
				want = fmt.Sprint(level)
				wantModel = "current-model"
			case "env":
				update.Config = &api.ScionConfig{Env: map[string]string{"SCION_THINKING_LEVEL": "4", "SCION_MODEL": "current-env-model"}}
				want = "4"
				wantModel = "current-env-model"
			}
			response := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+ag.ID+"/start", recoveryStartBody(update))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, runs, 1)
			require.Contains(t, runs[0].Env, "SCION_THINKING_LEVEL="+want)
			require.Contains(t, runs[0].Env, "SCION_MODEL="+wantModel)
			saved, err := db.GetAgent(context.Background(), ag.ID)
			require.NoError(t, err)
			if mode == "template" {
				require.Nil(t, saved.AppliedConfig.ThinkingLevel)
				require.Nil(t, saved.AppliedConfig.InlineConfig.ThinkingLevel)
				require.NotContains(t, saved.AppliedConfig.Env, "SCION_THINKING_LEVEL")
				require.NotContains(t, saved.AppliedConfig.InlineConfig.Env, "SCION_THINKING_LEVEL")
				require.Empty(t, saved.AppliedConfig.Model)
				require.Empty(t, saved.AppliedConfig.InlineConfig.Model)
				require.NotContains(t, saved.AppliedConfig.Env, "SCION_MODEL")
				require.NotContains(t, saved.AppliedConfig.InlineConfig.Env, "SCION_MODEL")
			}
			saved.Phase = "suspended"
			require.NoError(t, db.UpdateAgent(context.Background(), saved))
			containers[0].Phase = "suspended"
			response = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+ag.ID+"/start", map[string]any{})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, runs, 2)
			require.True(t, runs[1].Resume)
			require.Contains(t, runs[1].Env, "SCION_THINKING_LEVEL="+want)
			require.Contains(t, runs[1].Env, "SCION_MODEL="+wantModel)
		})
	}
}

func TestRetainedRuntimeRecoveryHubRequestedTemplateDefaultsAndExplicitOverrides(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			domain, model := "current.test", "current-model"
			if explicit {
				domain, model = "explicit.test", "explicit-model"
			}
			var original *store.Agent
			srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ResolvedEnv  map[string]string `json:"resolvedEnv"`
					InlineConfig *api.ScionConfig  `json:"inlineConfig"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				require.Equal(t, domain, req.ResolvedEnv["APP_DOMAIN"])
				require.Equal(t, model, req.ResolvedEnv["SCION_MODEL"])
				require.Equal(t, original.ID, req.ResolvedEnv["SCION_AGENT_ID"])
				require.Equal(t, domain, req.InlineConfig.Env["APP_DOMAIN"])
				require.Equal(t, model, req.InlineConfig.Model)
				require.NoError(t, json.NewEncoder(w).Encode(RemoteAgentResponse{Agent: &RemoteAgentInfo{Phase: "running", Image: "new:latest", Template: "current", ContainerStatus: "Up"}}))
			})
			original = agent
			require.NoError(t, db.CreateTemplate(context.Background(), &store.Template{
				ID: tid("retained-current-template"), Name: "current", Slug: "current", Scope: store.TemplateScopeGlobal,
				Harness: "claude", Status: store.TemplateStatusActive, ContentHash: "current-hash",
				Config: &store.TemplateConfig{Env: map[string]string{"APP_DOMAIN": "current.test"}, Model: "current-model"},
			}))
			agent.AppliedConfig.Env = map[string]string{"APP_DOMAIN": "old.test"}
			agent.AppliedConfig.Model = "old-model"
			agent.AppliedConfig.InlineConfig.Env["APP_DOMAIN"] = "old.test"
			agent.AppliedConfig.InlineConfig.Model = "old-model"
			require.NoError(t, db.UpdateAgent(context.Background(), agent))
			version := agent.StateVersion
			update.StateVersion = &version
			update.Config.Env = nil
			if explicit {
				update.Config.Env = map[string]string{"APP_DOMAIN": domain}
				update.Config.Model = model
			}
			response := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			saved, err := db.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.Equal(t, domain, saved.AppliedConfig.Env["APP_DOMAIN"])
			require.Equal(t, model, saved.AppliedConfig.Model)
			require.Equal(t, domain, saved.AppliedConfig.InlineConfig.Env["APP_DOMAIN"])
		})
	}
}

func TestRetainedRuntimeRecoveryHubAdmitsCASAndRetainsIdentity(t *testing.T) {
	var db store.Store
	var original *store.Agent
	srv, storage, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		// Ordinary status reports can advance StateVersion after admission.
		turns := 7
		require.NoError(t, db.UpdateAgentStatus(context.Background(), original.ID, store.AgentStatusUpdate{Activity: "thinking", ToolName: "retained-tool", CurrentTurns: &turns}))
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
	require.Equal(t, "thinking", saved.Activity)
	require.Equal(t, "retained-tool", saved.ToolName)
	require.Equal(t, 7, saved.CurrentTurns)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "current", saved.Template)
	require.Equal(t, "retained-branch", saved.AppliedConfig.Branch)
	require.Equal(t, "/retained/workspace", saved.AppliedConfig.Workspace)
	require.Equal(t, "original-creator", saved.AppliedConfig.CreatorName)
	require.Equal(t, "baseline", saved.AppliedConfig.AgentRole)
	require.Equal(t, *update.StateVersion+1, saved.AppliedConfig.RuntimeUpdateVersion)
}

type recoverySignalBus struct {
	NoopCommandBus
	signal func(context.Context, string) error
}

func (b recoverySignalBus) SignalBrokerCmd(ctx context.Context, brokerID string) error {
	return b.signal(ctx, brokerID)
}

func TestRetainedRuntimeRecoveryDeferredOwnerResultAndConcurrentStatusPreserved(t *testing.T) {
	srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeSuccessfulRecovery(t, w, r)
	})
	owner := srv.GetDispatcher()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.events = events
	requester := NewHTTPAgentDispatcherWithClient(db, &deferredTestClient{localBroker: "elsewhere"}, false, slog.Default())
	var completedVersion int64
	requester.SetCrossNodeDeps(events, recoverySignalBus{signal: func(ctx context.Context, brokerID string) error {
		pending, err := db.ListPendingDispatch(ctx, brokerID)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		// Exercise actual owner CAS claim, execution, result commit and done
		// notification, rather than manually dispatching a phase event.
		srv.SetDispatcher(owner)
		srv.ReconcileBroker(ctx, brokerID)
		srv.SetDispatcher(requester)
		intent, err := db.GetBrokerDispatch(ctx, pending[0].ID)
		require.NoError(t, err)
		require.Equal(t, store.DispatchStateDone, intent.State, intent.Error)
		native, err := db.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.Equal(t, "running", native.Phase)
		turns := 9
		require.NoError(t, db.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Activity: "executing", ToolName: "after-owner", CurrentTurns: &turns, Heartbeat: true}))
		native, err = db.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		completedVersion = native.StateVersion
		return nil
	}})
	srv.SetDispatcher(requester)
	response := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "running", saved.Phase)
	require.Equal(t, "Up", saved.ContainerStatus)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "current", saved.Template)
	require.Equal(t, "executing", saved.Activity)
	require.Equal(t, "after-owner", saved.ToolName)
	require.Equal(t, 9, saved.CurrentTurns)
	require.False(t, saved.LastSeen.IsZero())
	require.Equal(t, completedVersion, saved.StateVersion, "requester must not rewrite the owner's result")
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
	require.ErrorIs(t, srv.completeRuntimeRecovery(context.Background(), &old, &api.RuntimeRecovery{AdmissionVersion: 10}), store.ErrVersionConflict)
	version := int64(9)
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Resume: true, RuntimeRecovery: &api.RuntimeRecovery{Update: api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "obsolete:latest"}, AdmissionVersion: 10, AgentID: agent.ID, ProjectID: agent.ProjectID, RuntimeBrokerID: agent.RuntimeBrokerID}})
	require.NoError(t, err)
	_, err = srv.executeDispatch(context.Background(), store.BrokerDispatch{AgentID: agent.ID, ProjectID: agent.ProjectID, BrokerID: agent.RuntimeBrokerID, Op: "start", Args: args})
	require.ErrorContains(t, err, "stale")
}

func TestRetainedRuntimeRecoveryRequesterFailureCannotUndoOwnerResult(t *testing.T) {
	srv, db, agent, update := setupRuntimeRecoveryHub(t, func(http.ResponseWriter, *http.Request) { t.Fatal("no dispatch needed") })
	admission := agent.StateVersion + 1
	agent.AppliedConfig.RuntimeUpdateVersion = admission
	agent.Phase = "starting"
	require.NoError(t, db.UpdateAgent(context.Background(), agent))
	requester := *agent
	requesterConfig := *agent.AppliedConfig
	requester.AppliedConfig = &requesterConfig
	// The owner commits the successful configuration before a cancelled
	// requester tries to persist its unchanged admission snapshot as an error.
	agent.Template, agent.Image, agent.Phase = "current", "new:latest", "running"
	agent.AppliedConfig.Image = "new:latest"
	agent.Activity, agent.ToolName = "executing", "owner-tool"
	require.NoError(t, db.UpdateAgent(context.Background(), agent))
	version := agent.StateVersion
	requester.Phase, requester.Message = "error", "request cancelled"
	require.NoError(t, srv.completeRuntimeRecovery(context.Background(), &requester, &api.RuntimeRecovery{Update: *update, AdmissionVersion: admission}))
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "running", saved.Phase)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "executing", saved.Activity)
	require.Equal(t, "owner-tool", saved.ToolName)
	require.Equal(t, version, saved.StateVersion)
}

type recoveryStatusInterleavingStore struct {
	store.Store
	interleave func(context.Context, *store.Agent) error
}

func (s *recoveryStatusInterleavingStore) UpdateAgentRuntimeRecovery(ctx context.Context, agent *store.Agent, admissionVersion int64, replaceLabels bool) error {
	if s.interleave != nil && agent.AppliedConfig != nil && agent.AppliedConfig.Image == "new:latest" {
		interleave := s.interleave
		s.interleave = nil
		if err := interleave(ctx, agent); err != nil {
			return err
		}
	}
	return s.Store.UpdateAgentRuntimeRecovery(ctx, agent, admissionVersion, replaceLabels)
}

func TestRetainedRuntimeRecoveryCommitPreservesInterleavedStatusWithoutRepeatingDispatch(t *testing.T) {
	var starts atomic.Int32
	srv, db, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		starts.Add(1)
		writeSuccessfulRecovery(t, w, r)
	})
	srv.store = &recoveryStatusInterleavingStore{Store: db, interleave: func(ctx context.Context, result *store.Agent) error {
		turns := 13
		return db.UpdateAgentStatus(ctx, result.ID, store.AgentStatusUpdate{Activity: "thinking", ToolName: "during-cas", CurrentTurns: &turns})
	}}
	response := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "running", saved.Phase)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "thinking", saved.Activity)
	require.Equal(t, "during-cas", saved.ToolName)
	require.Equal(t, 13, saved.CurrentTurns)
	require.Equal(t, int32(1), starts.Load())
}

func TestRetainedRuntimeRecoveryConfigCommitPreservesNewerStopStatus(t *testing.T) {
	var db store.Store
	var id string
	srv, storage, agent, update := setupRuntimeRecoveryHub(t, func(w http.ResponseWriter, r *http.Request) {
		// Native creation succeeded; a later stop reports its state before the
		// earlier requester commits the recovered effective configuration.
		writeSuccessfulRecovery(t, w, r)
		require.NoError(t, db.UpdateAgentStatus(r.Context(), id, store.AgentStatusUpdate{Phase: "stopped", ContainerStatus: "Exited", RuntimeState: "stopped", Message: "Stopped by user"}))
	})
	db, id = storage, agent.ID
	response := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", recoveryStartBody(update))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	saved, err := db.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", saved.Phase)
	require.Equal(t, "Exited", saved.ContainerStatus)
	require.Equal(t, "stopped", saved.RuntimeState)
	require.Equal(t, "Stopped by user", saved.Message)
	require.Equal(t, "new:latest", saved.AppliedConfig.Image)
	require.Equal(t, "current", saved.Template)
}
