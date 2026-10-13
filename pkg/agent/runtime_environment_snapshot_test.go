package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

const snapshotFixtureRun = "b0000000-0000-4000-8000-000000000001"

func saveSnapshotFixture(t *testing.T, f *recoveryFixture) {
	t.Helper()
	f.state.config.Info.RunID = snapshotFixtureRun
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	require.NoError(t, writeRuntimeEnvironmentSnapshot(f.state.dir, f.state.config, runtime.RunConfig{
		Labels: map[string]string{api.LabelRunID: snapshotFixtureRun}, UnixUsername: "root",
		Env: []string{"SCION_MODEL=retained-effective-model", "SCION_THINKING_LEVEL=50", "SCION_AUTH_TOKEN=dummy-do-not-snapshot"},
	}, "existing-agent", "existing-project", "existing-broker", nil))
}

func TestRuntimeRecoverySnapshotPreservesEffectiveDefaults(t *testing.T) {
	f := newRecoveryFixture(t)
	initialBind := t.TempDir()
	f.state.config.Volumes = append(f.state.config.Volumes, api.VolumeMount{Source: initialBind, Target: "/original-default"})
	f.state.config.Docker = &api.DockerConfig{Networks: []string{"original-profile"}, Devices: []string{"/dev/kvm"}}
	saveSnapshotFixture(t, f)
	// scion-agent.json now represents only the requester's own environment.
	f.state.config.Docker = nil
	f.state.config.Volumes = f.state.config.Volumes[:1]
	f.state.config.Model = "old-alias"
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    docker:
      networks: [changed-profile]
      devices: [/dev/fuse]
`), 0644))
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, []string{"original-profile", "preview"}, f.runConfig.Networks)
	require.Equal(t, []string{"/dev/kvm"}, f.runConfig.Devices)
	require.Contains(t, f.runConfig.Volumes, api.VolumeMount{Source: initialBind, Target: "/original-default"})
	require.Contains(t, f.runConfig.Env, "SCION_MODEL=retained-effective-model")
	require.Contains(t, f.runConfig.Env, "SCION_THINKING_LEVEL=50")
	data, err := os.ReadFile(filepath.Join(f.state.dir, runtimeEnvironmentSnapshotFile))
	require.NoError(t, err)
	require.NotContains(t, string(data), "SCION_AUTH_TOKEN")
	require.NotContains(t, string(data), "dummy-do-not-snapshot")
	require.NotContains(t, string(data), `"env"`)
	f.assertPreserved(t)
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.runs)
}

func TestRuntimeRecoverySnapshotRunDriftRequiresOwnerEvidence(t *testing.T) {
	f := newRecoveryFixture(t)
	saveSnapshotFixture(t, f)
	// Ordinary Start records this attempt early, then can fail before Run.
	f.state.config.Info.RunID = "b0000000-0000-4000-8000-000000000002"
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	_, err := loadRuntimeRecovery(context.Background(), f.opts, f.opts.ProjectPath, nil, nil)
	require.ErrorContains(t, err, "run does not match")
	owner := &OwnerScope{RuntimeBrokerID: "existing-broker", RetainedRunOwned: func(p, a, slug, run string) bool {
		return p == "existing-project" && a == "existing-agent" && slug == "retained" && run == snapshotFixtureRun
	}}
	retained, err := loadRuntimeRecovery(context.Background(), f.opts, f.opts.ProjectPath, nil, owner)
	require.NoError(t, err)
	require.Equal(t, "retained-effective-model", retained.config.Model)
	owner.RetainedRunOwned = func(string, string, string, string) bool { return false }
	_, err = loadRuntimeRecovery(context.Background(), f.opts, f.opts.ProjectPath, nil, owner)
	require.ErrorContains(t, err, "run is not owned")
}

func TestRuntimeRecoverySnapshotRefusesForeignAndMalformed(t *testing.T) {
	for _, mutation := range []string{"agent", "project", "broker", "run", "unknown", "trailing", "version", "admission", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			f := newRecoveryFixture(t)
			saveSnapshotFixture(t, f)
			path := filepath.Join(f.state.dir, runtimeEnvironmentSnapshotFile)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var snapshot map[string]any
			require.NoError(t, json.Unmarshal(data, &snapshot))
			switch mutation {
			case "agent":
				snapshot["agentId"] = "foreign"
			case "project":
				snapshot["projectId"] = "foreign"
			case "broker":
				snapshot["runtimeBrokerId"] = "foreign"
			case "run":
				snapshot["runId"] = "foreign-invalid-run"
			case "unknown":
				snapshot["env"] = map[string]string{"SCION_AUTH_TOKEN": "forbidden"}
			case "version":
				snapshot["version"] = 2
			case "admission":
				snapshot["runtimeUpdateVersion"] = 9
			}
			data, err = json.Marshal(snapshot)
			require.NoError(t, err)
			if mutation == "trailing" {
				data = append(data, []byte(` {}`)...)
			}
			require.NoError(t, os.WriteFile(path, data, 0644))
			if mutation == "missing" {
				require.NoError(t, os.Remove(path))
			}
			before, err := os.ReadFile(filepath.Join(f.state.dir, "scion-agent.json"))
			require.NoError(t, err)
			_, err = NewManager(f.rt).Start(context.Background(), f.opts)
			require.Error(t, err)
			require.Zero(t, f.deletes)
			require.Zero(t, f.runs)
			after, err := os.ReadFile(filepath.Join(f.state.dir, "scion-agent.json"))
			require.NoError(t, err)
			require.Equal(t, before, after)
			f.assertPreserved(t)
		})
	}
}

func TestRuntimeRecoveryUpstreamConfigWithoutSnapshotFailsSafely(t *testing.T) {
	f := newRecoveryFixture(t)
	require.NoError(t, writeImageProvenance(f.state.dir, imageProvenance{RequestImage: "old:latest", Template: "previous"}))
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.ErrorContains(t, err, "snapshot is missing")
	require.Zero(t, f.runs)
	require.Zero(t, f.deletes)
}

func TestRuntimeRecoveryExplicitEnvironmentSelectionWins(t *testing.T) {
	for _, scalarCollision := range []bool{false, true} {
		t.Run(map[bool]string{false: "environment only", true: "scalar collision"}[scalarCollision], func(t *testing.T) {
			f := newRecoveryFixture(t)
			saveSnapshotFixture(t, f)
			requested := f.opts.RuntimeRecovery.Update.Config
			requested.Env["SCION_MODEL"] = "explicit-env-model"
			requested.Env["SCION_THINKING_LEVEL"] = "90"
			if scalarCollision {
				level := 20
				requested.Model = "explicit-scalar-model"
				requested.ThinkingLevel = &level
			}
			_, err := NewManager(f.rt).Start(context.Background(), f.opts)
			require.NoError(t, err)
			require.Contains(t, f.runConfig.Env, "SCION_MODEL=explicit-env-model")
			require.Contains(t, f.runConfig.Env, "SCION_THINKING_LEVEL=90")
			saved, err := (&config.Template{Path: f.state.dir}).LoadConfig()
			require.NoError(t, err)
			require.Equal(t, "explicit-env-model", saved.Model)
			require.Equal(t, 90, *saved.ThinkingLevel)
			_, err = NewManager(f.rt).Start(context.Background(), f.opts)
			require.NoError(t, err)
			require.Equal(t, 1, f.runs)
		})
	}
}

func TestRuntimeRecoveryExplicitBridgeKeepsMetadataInterception(t *testing.T) {
	for _, metadata := range []string{"block", "assign"} {
		t.Run(metadata, func(t *testing.T) {
			f := newRecoveryFixture(t)
			t.Setenv(runtime.ForceHostNetworkEnvVar, "1")
			f.opts.Env["SCION_HUB_ENDPOINT"] = "http://host.docker.internal:9810"
			f.opts.Env["SCION_NETWORK_MODE"] = "host"
			f.opts.Env["SCION_METADATA_MODE"] = metadata
			_, err := NewManager(f.rt).Start(context.Background(), f.opts)
			require.NoError(t, err)
			require.Equal(t, "preview", f.runConfig.NetworkMode)
			require.Contains(t, f.runConfig.Env, "SCION_NETWORK_MODE=preview")
			require.Contains(t, f.runConfig.Env, "SCION_HUB_ENDPOINT=http://host.docker.internal:9810")
			require.True(t, f.runConfig.MetadataInterception)
		})
	}
}

func TestOrdinaryStartUsesLiveDockerDefaultsWithoutPersistingThem(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	templatePath := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "default", "scion-agent.json")
	require.NoError(t, os.WriteFile(templatePath, []byte(`{"default_harness_config":"test-harness","docker":{"networks":["template-old"],"devices":["/dev/kvm"],"privileged":true}}`), 0644))
	settings := envSettingsV1.yaml()
	settings = strings.Replace(settings, "    runtime: docker", "    runtime: docker\n    docker:\n      networks: [profile-old]\n      devices: [/dev/fuse]", 1)
	require.NoError(t, os.WriteFile(settingsPath, []byte(settings), 0644))
	no := false
	own := &api.ScionConfig{Docker: &api.DockerConfig{Networks: []string{"requester"}, Privileged: &no}}
	mutate := func(opts *api.StartOptions) {
		opts.InlineConfig = own
		opts.Env = map[string]string{"SCION_AGENT_ID": "existing-agent", "SCION_PROJECT_ID": "existing-project", "SCION_BROKER_ID": "existing-broker"}
	}
	first := startEnv(t, project, mutate)
	require.Equal(t, []string{"profile-old", "template-old", "requester"}, first.Networks)
	require.Equal(t, []string{"/dev/fuse", "/dev/kvm"}, first.Devices)
	require.False(t, first.Privileged)
	saved, _ := persistedEnvAgentConfig(t, project)
	require.Equal(t, own.Docker, saved.Docker)
	require.NoError(t, os.WriteFile(templatePath, []byte(`{"default_harness_config":"test-harness","docker":{"networks":["template-new"],"devices":["/dev/dri"],"privileged":true}}`), 0644))
	settings = strings.Replace(settings, "profile-old", "profile-new", 1)
	settings = strings.Replace(settings, "      devices: [/dev/fuse]\n", "", 1)
	require.NoError(t, os.WriteFile(settingsPath, []byte(settings), 0644))
	second := startEnv(t, project, mutate)
	require.Equal(t, []string{"profile-new", "template-new", "requester"}, second.Networks)
	require.Equal(t, []string{"/dev/dri"}, second.Devices)
	require.False(t, second.Privileged)
	saved, _ = persistedEnvAgentConfig(t, project)
	require.Equal(t, own.Docker, saved.Docker)
	data, err := os.ReadFile(filepath.Join(project, "agents", "env-agent", runtimeEnvironmentSnapshotFile))
	require.NoError(t, err)
	var snapshot runtimeEnvironmentSnapshot
	require.NoError(t, json.Unmarshal(data, &snapshot))
	require.Equal(t, second.Networks, snapshot.Docker.Networks)
	require.Equal(t, second.Devices, snapshot.Docker.Devices)
}

func TestOrdinaryStartLiveTemplateImageFallback(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	template := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "default", "scion-agent.json")
	writeImage := func(image string) {
		require.NoError(t, os.WriteFile(template, []byte(`{"default_harness_config":"test-harness","image":"`+image+`"}`), 0644))
	}
	writeImage("project-template:v1")
	first := startEnv(t, project, nil)
	require.Equal(t, "project-template:v1", first.Image)
	saved, _ := persistedEnvAgentConfig(t, project)
	require.Empty(t, saved.Image)
	provenance, err := readImageProvenance(filepath.Join(project, "agents", "env-agent"))
	require.NoError(t, err)
	require.Empty(t, provenance.RequestImage)
	require.Empty(t, provenance.InlineImage)
	writeImage("project-template:v2")
	second := startEnv(t, project, nil)
	require.Equal(t, "project-template:v2", second.Image)
	settings := strings.Replace(envSettingsV1.yaml(), "      test-harness:", "      test-harness:\n        image: profile-pin:v1", 1)
	require.NoError(t, os.WriteFile(settingsPath, []byte(settings), 0644))
	profile := startEnv(t, project, nil)
	require.Equal(t, "profile-pin:v1", profile.Image)
	requested := startEnv(t, project, func(opts *api.StartOptions) { opts.Image = "explicit-request:v1" })
	require.Equal(t, "explicit-request:v1", requested.Image)
}

func TestRuntimeRecoverySnapshotRetryAfterInstalledConfigAndFailedRun(t *testing.T) {
	f := newRecoveryFixture(t)
	saveSnapshotFixture(t, f)
	original := f.rt.RunFunc
	f.rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		return "", fmt.Errorf("interrupted after install")
	}
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.ErrorContains(t, err, "interrupted after install")
	require.Equal(t, 1, f.deletes)
	f.rt.RunFunc = original
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.deletes)
	require.Equal(t, 1, f.runs)
	f.assertPreserved(t)
}

func TestLegacyHostedOrdinaryStartsDoNotManufactureImageProvenance(t *testing.T) {
	f := newRecoveryFixture(t)
	f.state.config.Info.ExplicitImage = "legacy-explicit:latest"
	f.state.config.Info.Template = "previous"
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	f.rt.DeleteFunc = func(context.Context, runtime.RunRef) error { f.containers = nil; return nil }
	opts := f.opts
	opts.RuntimeRecovery, opts.Template, opts.Image = nil, "", ""
	for attempt := 0; attempt < 2; attempt++ {
		_, err := NewManager(f.rt).Start(context.Background(), opts)
		require.NoError(t, err)
		require.Equal(t, "legacy-explicit:latest", f.runConfig.Image)
		provenance, err := readImageProvenance(f.state.dir)
		require.NoError(t, err)
		require.Nil(t, provenance, "legacy fallback must remain authoritative across ordinary restarts")
		f.containers[0].Phase = "stopped"
	}
	f.assertPreserved(t)
}

func TestOrdinaryRestartUsesHydratedTemplateEnvironmentSourceImageAndDocker(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	startEnv(t, project, nil)
	home := config.GetAgentHomePath(project, "env-agent")
	infoData, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	require.NoError(t, err)
	var info api.AgentInfo
	require.NoError(t, json.Unmarshal(infoData, &info))
	info.Template = "unresolvable-hub-display"
	info.TemplateHash = strings.Repeat("a", 64)
	require.NoError(t, atomicRecoveryJSON(filepath.Join(home, "agent-info.json"), &info))
	dir := filepath.Join(project, "agents", "env-agent")
	provenance, err := readImageProvenance(dir)
	require.NoError(t, err)
	provenance.Template, provenance.TemplateHash = info.Template, info.TemplateHash
	require.NoError(t, writeImageProvenance(dir, *provenance))
	hydrated := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hydrated, "scion-agent.json"), []byte(`{"image":"live-hydrated:v2","docker":{"networks":["hydrated-preview"]}}`), 0644))
	run := startEnv(t, project, func(opts *api.StartOptions) { opts.TemplateEnvSource = hydrated })
	require.Equal(t, "live-hydrated:v2", run.Image)
	require.Equal(t, []string{"hydrated-preview"}, run.Networks)
	saved, _ := persistedEnvAgentConfig(t, project)
	require.Empty(t, saved.Image)
	require.Nil(t, saved.Docker)
}

func TestOrdinaryStartAfterRecoveryRefreshesInheritedDockerDefaults(t *testing.T) {
	f := newRecoveryFixture(t)
	privileged := true
	f.state.config.Docker = &api.DockerConfig{Privileged: &privileged, Networks: []string{"original-profile"}, Devices: []string{"/dev/kvm"}}
	saveSnapshotFixture(t, f)
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, []string{"original-profile", "preview"}, f.runConfig.Networks)
	require.Equal(t, []string{"/dev/kvm"}, f.runConfig.Devices)
	// A normal start reads current defaults, retaining only explicit requester
	// additions from recovery rather than freezing its full effective grants.
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    docker:
      networks: [changed-profile]
      devices: [/dev/fuse]
      privileged: true
`), 0644))
	f.containers[0].Phase = "stopped"
	f.rt.DeleteFunc = func(context.Context, runtime.RunRef) error { f.containers = nil; return nil }
	ordinary := f.opts
	ordinary.RuntimeRecovery, ordinary.Template, ordinary.Image = nil, "", ""
	disabled := false
	ordinary.InlineConfig = &api.ScionConfig{Docker: &api.DockerConfig{Privileged: &disabled, Networks: []string{"patched"}, Labels: map[string]string{"patched": "explicit"}}}
	persisted, err := (&config.Template{Path: f.state.dir}).LoadConfig()
	require.NoError(t, err)
	persisted = config.MergeScionConfig(persisted, ordinary.InlineConfig)
	require.NoError(t, atomicRecoveryJSON(filepath.Join(f.state.dir, "scion-agent.json"), persisted))
	_, err = NewManager(f.rt).Start(context.Background(), ordinary)
	require.NoError(t, err)
	require.Equal(t, []string{"changed-profile", "preview", "patched"}, f.runConfig.Networks)
	require.Equal(t, []string{"/dev/fuse"}, f.runConfig.Devices)
	saved, err := (&config.Template{Path: f.state.dir}).LoadConfig()
	require.NoError(t, err)
	require.Equal(t, []string{"preview", "patched"}, saved.Docker.Networks)
	require.False(t, f.runConfig.Privileged)
	require.Equal(t, "explicit", f.runConfig.DockerLabels["patched"])
	require.Empty(t, saved.Docker.Devices)
	f.assertPreserved(t)
}

func TestRuntimeRecoveryRetryHashesActualHarnessUser(t *testing.T) {
	f := newRecoveryFixture(t)
	f.state.config.User = ""
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, "root", f.runConfig.UnixUsername)
	saved, err := (&config.Template{Path: f.state.dir}).LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "root", saved.User)
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.runs)
}
