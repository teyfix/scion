package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

type recoveryFixture struct {
	opts               api.StartOptions
	state              *retainedRuntimeState
	rt                 *runtime.MockRuntime
	containers         []api.AgentInfo
	runConfig          runtime.RunConfig
	deletes, runs      int
	sentinels          map[string][]byte
	gitStatus, gitHead string
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", data)
	return string(data)
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_GROVE", "")
	project := filepath.Join(root, "project", ".scion")
	dir := filepath.Join(project, "agents", "retained")
	home, workspace := filepath.Join(dir, "home"), filepath.Join(dir, "workspace")
	privateDocker := filepath.Join(home, "docker")
	for _, path := range []string{workspace, privateDocker} {
		require.NoError(t, os.MkdirAll(path, 0755))
	}
	harnessDir := filepath.Join(root, ".scion", "harness-configs", "test-harness")
	require.NoError(t, os.MkdirAll(harnessDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(harnessDir, "config.yaml"), []byte(`harness: codex
user: root
image: old:latest
config_dir: .codex
provisioner:
  type: container-script
  interface_version: 1
  command: [python3, "$HOME/.scion/harness/provision.py"]
  lifecycle_events: [pre-start]
command:
  base: [codex]
  resume_flag: "resume --last"
capabilities:
  resume:
    support: "yes"
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(harnessDir, "provision.py"), []byte("# test fixture: provisioner execution occurs in the mocked container\n"), 0644))
	template := filepath.Join(root, ".scion", "templates", "current")
	require.NoError(t, os.MkdirAll(template, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(template, "scion-agent.json"), []byte(`{"harness":"codex","harness_config":"test-harness","env":{"CURRENT_TEMPLATE":"effective"}}`), 0644))
	old := &api.ScionConfig{Harness: "codex", HarnessConfig: "test-harness", User: "root", Image: "old:latest",
		Env:     map[string]string{"SCION_AGENT_ID": "existing-agent", "SCION_PROJECT_ID": "existing-project"},
		Volumes: []api.VolumeMount{{Source: privateDocker, Target: "/var/lib/docker"}},
		Info:    &api.AgentInfo{ID: "existing-agent", Name: "retained", ProjectID: "existing-project", RuntimeBrokerID: "existing-broker", Template: "previous", Phase: "suspended", Image: "old:latest"},
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
	state := &retainedRuntimeState{dir: dir, home: home, workspace: workspace, config: old}
	require.NoError(t, writeRuntimeRecoveryState(state))
	fixtureGit(t, workspace, "init", "-b", "retained-branch")
	fixtureGit(t, workspace, "config", "user.email", "test@example.com")
	fixtureGit(t, workspace, "config", "user.name", "Recovery test")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("original\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored-unique\n"), 0644))
	fixtureGit(t, workspace, "add", "tracked.txt", ".gitignore")
	fixtureGit(t, workspace, "commit", "-m", "fixture")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("staged\n"), 0644))
	fixtureGit(t, workspace, "add", "tracked.txt")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("unstaged\n"), 0644))
	f := &recoveryFixture{state: state, sentinels: map[string][]byte{}}
	for _, relative := range []string{"workspace/untracked", "workspace/ignored-unique", "home/.codex/sessions/retained.jsonl", "home/model-cache/sentinel", "home/docker/private-bytes", "home/unique-home", "home/.scion/hooks/pre-start.d/30-project-custom"} {
		path := filepath.Join(dir, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		data := []byte("retained:" + relative)
		require.NoError(t, os.WriteFile(path, data, 0644))
		f.sentinels[path] = data
	}
	f.gitStatus = fixtureGit(t, workspace, "status", "--porcelain=v1", "--untracked-files=all")
	f.gitHead = fixtureGit(t, workspace, "rev-parse", "HEAD")
	version := int64(4)
	f.opts = api.StartOptions{Name: "retained", Template: "current", TemplateName: "current", Image: "new:latest", ProjectPath: project, Resume: true, NoAuth: true, BrokerMode: true,
		Env: map[string]string{"SCION_AGENT_ID": "existing-agent", "SCION_PROJECT_ID": "existing-project"},
		RuntimeRecovery: &api.RuntimeRecovery{Update: api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "new:latest", Config: &api.ScionConfig{Env: map[string]string{"APP_DOMAIN": "preview.test"}, Docker: &api.DockerConfig{Networks: []string{"preview"}, Labels: map[string]string{"traefik.enable": "true", "traefik.http.routers.${SCION_AGENT_SLUG}.rule": "Host(`${APP_DOMAIN}`)"}}}},
			AgentID: "existing-agent", ProjectID: "existing-project", RuntimeBrokerID: "existing-broker", AdmissionVersion: 5},
	}
	f.containers = []api.AgentInfo{{Name: "retained", ContainerID: "old-container", Phase: "stopped", Image: "old:latest", Labels: map[string]string{"agent_id": "existing-agent", "scion.name": "retained", "scion.project_id": "existing-project"}}}
	f.rt = &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return f.containers, nil },
		DeleteFunc: func(_ context.Context, id string) error {
			require.Equal(t, "old-container", id)
			f.deletes++
			f.containers = nil
			return nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			f.runs++
			f.runConfig = cfg
			f.containers = []api.AgentInfo{{Name: "retained", ContainerID: "new-container", Phase: "running", Image: cfg.Image, Template: cfg.Template, Labels: cfg.Labels}}
			return "new-container", nil
		},
	}
	return f
}

func (f *recoveryFixture) assertPreserved(t *testing.T) {
	t.Helper()
	for path, expected := range f.sentinels {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, expected, data, path)
	}
	require.Equal(t, f.gitStatus, fixtureGit(t, f.state.workspace, "status", "--porcelain=v1", "--untracked-files=all"))
	require.Equal(t, f.gitHead, fixtureGit(t, f.state.workspace, "rev-parse", "HEAD"))
	require.Equal(t, "retained-branch\n", fixtureGit(t, f.state.workspace, "branch", "--show-current"))
}

func TestRetainedRuntimeRecoveryPreservesGitHomeDockerAndSession(t *testing.T) {
	f := newRecoveryFixture(t)
	info, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, "new-container", info.ContainerID)
	require.Equal(t, "new:latest", f.runConfig.Image)
	require.Equal(t, "current", f.runConfig.Template)
	require.Equal(t, f.state.home, f.runConfig.HomeDir)
	require.Equal(t, f.state.workspace, f.runConfig.Workspace)
	require.True(t, f.runConfig.Resume)
	require.Equal(t, []string{"codex", "resume", "--last"}, f.runConfig.Harness.GetCommand("", f.runConfig.Resume, f.runConfig.CommandArgs))
	require.Equal(t, []string{"preview"}, f.runConfig.Networks)
	require.Equal(t, "true", f.runConfig.DockerLabels["traefik.enable"])
	require.Equal(t, "Host(`preview.test`)", f.runConfig.DockerLabels["traefik.http.routers.retained.rule"])
	require.Contains(t, f.runConfig.Env, "CURRENT_TEMPLATE=effective")
	require.Contains(t, f.runConfig.Volumes, f.state.config.Volumes[0])
	require.Equal(t, 1, f.deletes)
	require.Equal(t, 1, f.runs)
	identityData, err := os.ReadFile(filepath.Join(f.state.home, "agent-info.json"))
	require.NoError(t, err)
	var retainedInfo api.AgentInfo
	require.NoError(t, json.Unmarshal(identityData, &retainedInfo))
	require.Equal(t, "existing-agent", retainedInfo.ID)
	require.Equal(t, "existing-project", retainedInfo.ProjectID)
	require.Equal(t, "existing-broker", retainedInfo.RuntimeBrokerID)
	require.Equal(t, "new:latest", retainedInfo.Image)
	require.Equal(t, "current", retainedInfo.Template)
	f.assertPreserved(t)
	// An acknowledged-loss retry verifies the actual matching running container.
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.deletes)
	require.Equal(t, 1, f.runs)
	f.assertPreserved(t)
}

func TestRetainedRuntimeRecoveryInterruptedAfterInstallRetries(t *testing.T) {
	f := newRecoveryFixture(t)
	originalDelete := f.rt.DeleteFunc
	f.rt.DeleteFunc = func(context.Context, string) error { return errors.New("interrupted after configuration install") }
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.ErrorContains(t, err, "interrupted")
	saved, err := (&config.Template{Path: f.state.dir}).LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "new:latest", saved.Image)
	require.Equal(t, 0, f.runs)
	f.assertPreserved(t)
	f.rt.DeleteFunc = originalDelete
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.runs)
	f.assertPreserved(t)
}

func TestRetainedRuntimeRecoveryPartialRunFailureRetainsRecoverableState(t *testing.T) {
	f := newRecoveryFixture(t)
	originalRun := f.rt.RunFunc
	f.rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		f.runs++
		return "", errors.New("container creation interrupted")
	}
	_, err := NewManager(f.rt).Start(context.Background(), f.opts)
	require.ErrorContains(t, err, "interrupted")
	require.Equal(t, 1, f.deletes)
	require.Equal(t, 1, f.runs)
	require.Empty(t, f.containers)
	f.assertPreserved(t)
	f.rt.RunFunc = originalRun
	_, err = NewManager(f.rt).Start(context.Background(), f.opts)
	require.NoError(t, err)
	require.Equal(t, 1, f.deletes)
	require.Equal(t, 2, f.runs)
	f.assertPreserved(t)
}

func TestRetainedRuntimeRecoveryRejectsBeforeContainerOrConfigMutation(t *testing.T) {
	for _, scenario := range []string{"missing-workspace", "missing-config", "identity", "private-docker", "private-docker-alias", "root-overmount", "privileges", "missing-template", "missing-image", "superseded", "running"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveryFixture(t)
			before, err := os.ReadFile(filepath.Join(f.state.dir, "scion-agent.json"))
			require.NoError(t, err)
			switch scenario {
			case "missing-workspace":
				require.NoError(t, os.Rename(f.state.workspace, f.state.workspace+"-preserved"))
			case "missing-config":
				require.NoError(t, os.Rename(filepath.Join(f.state.dir, "scion-agent.json"), filepath.Join(f.state.dir, "config-preserved")))
			case "identity":
				f.opts.RuntimeRecovery.AgentID = "another-agent"
			case "private-docker":
				f.opts.RuntimeRecovery.Update.Config.Volumes = []api.VolumeMount{{Source: f.state.home, Target: "/var/lib/docker"}}
			case "private-docker-alias":
				f.opts.RuntimeRecovery.Update.Config.Volumes = []api.VolumeMount{{Source: f.state.home, Target: "/var/lib/../lib/docker"}}
			case "root-overmount":
				f.opts.RuntimeRecovery.Update.Config.Volumes = []api.VolumeMount{{Source: f.state.home, Target: "/"}}
			case "privileges":
				enabled := true
				f.opts.RuntimeRecovery.Update.Config.Docker.Privileged = &enabled
			case "missing-template":
				f.opts.Template = "not-found"
			case "missing-image":
				f.rt.ImageExistsFunc = func(context.Context, string) (bool, error) { return false, nil }
				f.rt.PullImageFunc = func(context.Context, string) error { return errors.New("image unavailable") }
			case "superseded":
				f.state.config.RuntimeUpdateVersion = 99
				require.NoError(t, writeRuntimeRecoveryState(f.state))
				before, err = os.ReadFile(filepath.Join(f.state.dir, "scion-agent.json"))
				require.NoError(t, err)
			case "running":
				f.containers[0].Phase = "running"
			}
			_, err = NewManager(f.rt).Start(context.Background(), f.opts)
			require.Error(t, err)
			require.Equal(t, 0, f.deletes)
			require.Equal(t, 0, f.runs)
			if scenario != "missing-config" {
				after, err := os.ReadFile(filepath.Join(f.state.dir, "scion-agent.json"))
				require.NoError(t, err)
				require.True(t, reflect.DeepEqual(before, after))
			}
			if scenario != "missing-workspace" {
				f.assertPreserved(t)
			}
		})
	}
}
