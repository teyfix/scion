package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestEffectiveStartDockerConfigPreservesLiveNetworkTiers(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    docker:
      networks: [profile-preview]
runtimes:
  docker:
    type: docker
    docker:
      networks: [runtime-preview]
`), 0644))
	template := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "default", "scion-agent.json")
	require.NoError(t, os.WriteFile(template, []byte(`{"docker":{"networks":["template-preview"]}}`), 0644))
	opts := api.StartOptions{
		Name: "policy-agent", ProjectPath: project, FreshProvision: true,
		InlineConfig: &api.ScionConfig{Docker: &api.DockerConfig{Networks: []string{"request-preview"}}},
	}
	docker, err := EffectiveStartDockerConfig(context.Background(), opts, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"runtime-preview", "profile-preview", "template-preview", "request-preview"}, docker.Networks)
	require.NoError(t, os.WriteFile(template, []byte(`{"docker":{"networks":["template-new"]}}`), 0644))
	docker, err = EffectiveStartDockerConfig(context.Background(), opts, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"runtime-preview", "profile-preview", "template-new", "request-preview"}, docker.Networks)
}

func TestEffectiveStartDockerConfigRetainedRecoveryRequiresOwner(t *testing.T) {
	f := newRecoveryFixture(t)
	f.state.config.Docker = &api.DockerConfig{Networks: []string{"retained-preview"}}
	saveSnapshotFixture(t, f)
	f.state.config.Info.RunID = "b0000000-0000-4000-8000-000000000002"
	require.NoError(t, writeRuntimeRecoveryState(f.state))
	_, err := EffectiveStartDockerConfig(context.Background(), f.opts, nil)
	require.ErrorContains(t, err, "run does not match")
	owner := &OwnerScope{RuntimeBrokerID: "existing-broker", RetainedRunOwned: func(project, agent, slug, run string) bool {
		return project == "existing-project" && agent == "existing-agent" && slug == "retained" && run == snapshotFixtureRun
	}}
	docker, err := EffectiveStartDockerConfig(context.Background(), f.opts, owner)
	require.NoError(t, err)
	require.Equal(t, []string{"retained-preview", "preview"}, docker.Networks)
	owner.RetainedRunOwned = func(string, string, string, string) bool { return false }
	_, err = EffectiveStartDockerConfig(context.Background(), f.opts, owner)
	require.ErrorContains(t, err, "run is not owned")
}
