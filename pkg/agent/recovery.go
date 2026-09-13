package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

const runtimeConfigHashLabel = "scion.runtime_config_hash"

type retainedRuntimeState struct {
	dir, home, workspace string
	config               *api.ScionConfig
}

// loadRuntimeRecovery reads retained state without GetAgent's fresh provisioning
// and repair paths. Missing state is an error, never permission to reconstruct it.
func loadRuntimeRecovery(opts api.StartOptions, projectDir string, containers []api.AgentInfo) (*retainedRuntimeState, error) {
	recovery := opts.RuntimeRecovery
	if err := recovery.Update.Validate(); err != nil {
		return nil, err
	}
	if opts.GitClone != nil || opts.Branch != "" || opts.Workspace != "" {
		return nil, fmt.Errorf("retained recovery cannot clone or change workspace/branch")
	}
	state, err := readRetainedRuntimeState(opts, projectDir, containers)
	if err != nil {
		return nil, err
	}
	old, info := state.config, state.config.Info
	requested, err := requestedRuntimeConfig(opts)
	if err != nil {
		return nil, err
	}
	if err := resolveRecoveryLabels(opts, projectDir, old, requested); err != nil {
		return nil, err
	}
	effective := config.MergeScionConfig(old, requested)
	if err := validateRetainedRuntimeConfig(old, effective); err != nil {
		return nil, err
	}
	effective.Volumes, err = preservedVolumes(old.Volumes, requested.Volumes)
	if err != nil {
		return nil, err
	}
	effective.Env = config.MergeScionConfig(&api.ScionConfig{Env: effective.Env}, &api.ScionConfig{Env: map[string]string{
		"SCION_AGENT_ID": recovery.AgentID, "SCION_PROJECT_ID": recovery.ProjectID, "SCION_BROKER_ID": recovery.RuntimeBrokerID,
	}}).Env
	effective.RuntimeUpdateVersion = recovery.AdmissionVersion
	info.ID, info.ProjectID, info.RuntimeBrokerID = recovery.AgentID, recovery.ProjectID, recovery.RuntimeBrokerID
	info.Template = recovery.Update.Template
	info.Image = recovery.Update.Image
	effective.Info = info
	state.config = effective
	return state, nil
}
func readRetainedRuntimeState(opts api.StartOptions, projectDir string, containers []api.AgentInfo) (*retainedRuntimeState, error) {
	recovery := opts.RuntimeRecovery
	dir := config.ResolveAgentDir(projectDir, opts.Name)
	home := config.GetAgentHomePath(projectDir, opts.Name)
	for _, path := range []string{dir, home} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("retained directory is missing or invalid: %s", path)
		}
	}
	old, err := (&config.Template{Path: dir}).LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("read retained configuration: %w", err)
	}
	if old.RuntimeUpdateVersion > recovery.AdmissionVersion {
		return nil, fmt.Errorf("runtime recovery admission has been superseded")
	}
	data, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	if err != nil {
		return nil, fmt.Errorf("read retained agent identity: %w", err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("read retained agent identity: %w", err)
	}
	if err := verifyRetainedRuntimeIdentity(opts, old, &info, containers); err != nil {
		return nil, err
	}
	workspace := filepath.Join(dir, "workspace")
	if opts.SharedWorkspace || old.ExplicitWorkspace {
		workspace = extractWorkspaceFromVolumes(old.Volumes)
	}
	if workspace == "" {
		return nil, fmt.Errorf("retained workspace binding is missing")
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("retained workspace is missing: %s", workspace)
	}
	old.Info = &info
	return &retainedRuntimeState{dir: dir, home: home, workspace: workspace, config: old}, nil
}

func verifyRetainedRuntimeIdentity(opts api.StartOptions, old *api.ScionConfig, info *api.AgentInfo, containers []api.AgentInfo) error {
	recovery := opts.RuntimeRecovery
	if api.Slugify(info.Name) != api.Slugify(opts.Name) || (info.RuntimeBrokerID != "" && info.RuntimeBrokerID != recovery.RuntimeBrokerID) {
		return fmt.Errorf("retained agent name or broker does not match")
	}
	agentID, projectID := info.ID, info.ProjectID
	if agentID == "" {
		agentID = old.Env["SCION_AGENT_ID"]
	}
	if projectID == "" {
		projectID = projectcompat.ProjectIDFromEnv(old.Env)
	}
	for _, c := range containers {
		if api.Slugify(c.Name) != api.Slugify(opts.Name) {
			continue
		}
		if c.Labels["agent_id"] == recovery.AgentID && projectcompat.ProjectIDFromLabels(c.Labels) == recovery.ProjectID {
			if agentID == "" {
				agentID = recovery.AgentID
			}
			if projectID == "" {
				projectID = recovery.ProjectID
			}
		}
	}
	if agentID != recovery.AgentID || projectID != recovery.ProjectID {
		return fmt.Errorf("retained agent or project identity does not match")
	}
	return nil
}

func requestedRuntimeConfig(opts api.StartOptions) (*api.ScionConfig, error) {
	recovery := opts.RuntimeRecovery
	chain, err := config.GetTemplateChainInProject(opts.Template, opts.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("resolve requested recovery template: %w", err)
	}
	requested := &api.ScionConfig{}
	for _, template := range chain {
		cfg, err := template.LoadConfig()
		if err != nil {
			return nil, fmt.Errorf("read requested recovery template: %w", err)
		}
		requested = config.MergeScionConfig(requested, cfg)
	}
	requested = config.MergeScionConfig(requested, recovery.Update.Config)
	requested.Image = recovery.Update.Image
	return requested, nil
}

func resolveRecoveryLabels(opts api.StartOptions, projectDir string, old, requested *api.ScionConfig) error {
	recovery := opts.RuntimeRecovery
	// Expand layers before merging so a retry overrides an expanded key.
	labelEnv := config.MergeScionConfig(old, requested).Env
	vars := BuildScopedLabelVars(opts.Name, recovery.AgentID, config.GetProjectName(projectDir), recovery.ProjectID, labelEnv)
	for _, layer := range []*api.ScionConfig{old, requested} {
		if layer.Docker == nil {
			continue
		}
		labels, err := ExpandAndValidateDockerLabels(layer.Docker.Labels, vars)
		if err != nil {
			return fmt.Errorf("resolve recovery Docker labels: %w", err)
		}
		layer.Docker.Labels = labels
	}
	return nil
}

func validateRetainedRuntimeConfig(old, effective *api.ScionConfig) error {
	if old.User != effective.User || old.Harness != effective.Harness || old.HarnessConfig != effective.HarnessConfig || old.DefaultHarnessConfig != effective.DefaultHarnessConfig || old.ConfigDir != effective.ConfigDir {
		return fmt.Errorf("runtime recovery cannot change container user, harness, or provider configuration identity")
	}
	if !reflect.DeepEqual(old.Services, effective.Services) || !reflect.DeepEqual(old.Skills, effective.Skills) || old.AgentInstructions != effective.AgentInstructions || old.SystemPrompt != effective.SystemPrompt || effective.Kubernetes != nil {
		return fmt.Errorf("runtime recovery cannot change services, skills, instructions, or runtime backend")
	}
	oldPrivileged, newPrivileged := false, false
	if old.Docker != nil && old.Docker.Privileged != nil {
		oldPrivileged = *old.Docker.Privileged
	}
	if effective.Docker != nil && effective.Docker.Privileged != nil {
		newPrivileged = *effective.Docker.Privileged
	}
	if oldPrivileged != newPrivileged {
		return fmt.Errorf("runtime recovery cannot change daemon privileges")
	}
	if !reflect.DeepEqual(config.ResolveDockerDevices(old.Docker), config.ResolveDockerDevices(effective.Docker)) {
		return fmt.Errorf("runtime recovery cannot change daemon device permissions")
	}
	return nil
}

func preservedVolumes(old, requested []api.VolumeMount) ([]api.VolumeMount, error) {
	result := append([]api.VolumeMount(nil), old...)
	byTarget := make(map[string]api.VolumeMount)
	for _, volume := range old {
		if err := volume.Validate(); err != nil {
			return nil, err
		}
		if previous, ok := byTarget[volume.Target]; ok && previous != volume {
			return nil, fmt.Errorf("retained volume binding is ambiguous: %s", volume.Target)
		}
		byTarget[volume.Target] = volume
		if volume.Type == "" || volume.Type == "local" {
			if _, err := os.Stat(volume.Source); err != nil {
				return nil, fmt.Errorf("retained volume source is missing: %s", volume.Source)
			}
		}
	}
	for _, volume := range requested {
		if err := volume.Validate(); err != nil {
			return nil, err
		}
		target := filepath.Clean(volume.Target)
		if target != volume.Target {
			return nil, fmt.Errorf("runtime recovery requires canonical volume targets: %s", volume.Target)
		}
		if previous, exists := byTarget[target]; exists {
			if previous != volume {
				return nil, fmt.Errorf("runtime recovery cannot change retained volume binding: %s", volume.Target)
			}
			continue
		}
		if recoveryMountOverlapsIdentity(target) {
			return nil, fmt.Errorf("runtime recovery cannot introduce a replacement home, workspace, or Docker identity")
		}
		if volume.Type != "" && volume.Type != "local" {
			return nil, fmt.Errorf("runtime recovery only supports existing storage and added local binds")
		}
		sourceInfo, err := os.Stat(volume.Source)
		if err != nil {
			return nil, fmt.Errorf("requested volume source is missing: %s", volume.Source)
		}
		if sourceInfo.Mode()&os.ModeSocket != 0 {
			return nil, fmt.Errorf("runtime recovery cannot introduce a daemon socket bind")
		}
		byTarget[volume.Target] = volume
		result = append(result, volume)
	}
	return result, nil
}

func recoveryMountOverlapsIdentity(target string) bool {
	for _, retained := range []string{"/workspace", "/home", "/var/lib/docker", "/var/run/docker.sock"} {
		if target == "/" || target == retained || strings.HasPrefix(target, retained+"/") || strings.HasPrefix(retained, target+"/") {
			return true
		}
	}
	return false
}

func runtimeConfigHash(cfg *api.ScionConfig) (string, error) {
	copy := *cfg
	// Admission changes on retry; effective configuration identity does not.
	copy.RuntimeUpdateVersion = 0
	data, err := json.Marshal(&copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func writeRuntimeRecoveryState(state *retainedRuntimeState) error {
	if err := atomicRecoveryJSON(filepath.Join(state.dir, "scion-agent.json"), state.config); err != nil {
		return err
	}
	return atomicRecoveryJSON(filepath.Join(state.home, "agent-info.json"), state.config.Info)
}

func atomicRecoveryJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".runtime-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Recovery installs its complete configuration atomically after all start
// validation; ordinary starts keep their existing write behavior.
func writeStartConfig(path string, data []byte, recovery bool) error {
	if recovery {
		return nil
	}
	return os.WriteFile(path, data, 0644)
}

func updateStartStatus(opts api.StartOptions, retained *retainedRuntimeState, status, runtime, profile string) error {
	if retained == nil {
		return UpdateAgentConfig(opts.Name, opts.ProjectPath, status, runtime, profile)
	}
	info := *retained.config.Info
	info.Phase, info.Runtime = status, runtime
	if profile != "" {
		info.Profile = profile
	}
	return atomicRecoveryJSON(filepath.Join(retained.home, "agent-info.json"), &info)
}
