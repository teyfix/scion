package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const runtimeEnvironmentSnapshotFile = "runtime-environment.json"
const runtimeEnvironmentSnapshotRequiredFile = "runtime-environment-required.json"

// This is effective configuration metadata for retained recovery, never an
// ownership claim. It lives in broker-side state beside image provenance,
// and deliberately contains no environment map, credentials or secrets.
type runtimeEnvironmentSnapshot struct {
	Version                 int               `json:"version"`
	AgentID                 string            `json:"agentId"`
	ProjectID               string            `json:"projectId"`
	RuntimeBrokerID         string            `json:"runtimeBrokerId"`
	RunID                   string            `json:"runId"`
	RuntimeUpdateVersion    int64             `json:"runtimeUpdateVersion,omitempty"`
	Docker                  *api.DockerConfig `json:"docker,omitempty"`
	RequesterDocker         *api.DockerConfig `json:"requesterDocker,omitempty"`
	RequesterDockerRecorded bool              `json:"requesterDockerRecorded,omitempty"`
	Volumes                 []api.VolumeMount `json:"volumes,omitempty"`
	User                    string            `json:"user,omitempty"`
	Model                   string            `json:"model,omitempty"`
	ThinkingLevel           *int              `json:"thinkingLevel,omitempty"`
}

func writeRuntimeEnvironmentSnapshot(dir string, cfg *api.ScionConfig, run runtime.RunConfig, agentID, projectID, brokerID string, requesterDocker *api.DockerConfig) error {
	// Local agents without a complete hosted identity have no admitted
	// retained-recovery operation. Do not manufacture a hosted identity.
	if cfg == nil || agentID == "" || projectID == "" || brokerID == "" {
		return nil
	}
	snapshot := runtimeEnvironmentSnapshot{
		Version: 1, AgentID: agentID, ProjectID: projectID, RuntimeBrokerID: brokerID, RunID: run.Labels[api.LabelRunID],
		RuntimeUpdateVersion: cfg.RuntimeUpdateVersion, Docker: config.CloneDockerConfig(cfg.Docker),
		RequesterDocker: config.CloneDockerConfig(requesterDocker), RequesterDockerRecorded: true,
		Volumes: append([]api.VolumeMount(nil), cfg.Volumes...), User: run.UnixUsername, Model: cfg.Model, ThinkingLevel: cfg.ThinkingLevel,
	}
	for _, entry := range run.Env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "SCION_MODEL" {
			snapshot.Model = value
		}
		if key == "SCION_THINKING_LEVEL" {
			level, err := strconv.Atoi(value)
			if err == nil {
				snapshot.ThinkingLevel = &level
			}
		}
	}
	if err := atomicRecoveryJSON(filepath.Join(dir, runtimeEnvironmentSnapshotFile), &snapshot); err != nil {
		return err
	}
	provenance, err := readImageProvenance(dir)
	if err != nil {
		return err
	}
	// Do not manufacture image provenance for legacy agents: an empty record
	// would suppress their existing ExplicitImage/Profile/Template fallback.
	if provenance != nil {
		provenance.RuntimeEnvironmentSnapshot = true
		if err := writeImageProvenance(dir, *provenance); err != nil {
			return err
		}
	}
	return atomicRecoveryJSON(filepath.Join(dir, runtimeEnvironmentSnapshotRequiredFile), struct {
		Version int `json:"version"`
	}{Version: 1})
}

func restoreRuntimeEnvironmentSnapshot(dir string, opts api.StartOptions, cfg *api.ScionConfig, info *api.AgentInfo, owner *OwnerScope) error {
	data, err := os.ReadFile(filepath.Join(dir, runtimeEnvironmentSnapshotFile))
	if errors.Is(err, fs.ErrNotExist) {
		provenance, pErr := readImageProvenance(dir)
		if pErr != nil {
			return pErr
		}
		_, markerErr := os.Stat(filepath.Join(dir, runtimeEnvironmentSnapshotRequiredFile))
		if markerErr != nil && !errors.Is(markerErr, fs.ErrNotExist) {
			return fmt.Errorf("read retained snapshot marker: %w", markerErr)
		}
		if markerErr == nil || (provenance != nil && (provenance.RuntimeEnvironmentSnapshot || cfg.RuntimeUpdateVersion == 0)) {
			return fmt.Errorf("retained effective runtime snapshot is missing")
		}
		return nil // validated legacy fork state predates the snapshot record
	}
	if err != nil {
		return fmt.Errorf("read retained effective runtime snapshot: %w", err)
	}
	var snapshot runtimeEnvironmentSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("read retained effective runtime snapshot: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("retained effective runtime snapshot has trailing data")
	}
	recovery := opts.RuntimeRecovery
	if snapshot.Version != 1 || snapshot.AgentID != recovery.AgentID || snapshot.ProjectID != recovery.ProjectID || snapshot.RuntimeBrokerID != recovery.RuntimeBrokerID || runtime.ValidateRunID(snapshot.RunID) != nil {
		return fmt.Errorf("retained effective runtime snapshot identity does not match")
	}
	if owner != nil {
		if owner.RuntimeBrokerID != snapshot.RuntimeBrokerID || owner.RetainedRunOwned == nil || !owner.RetainedRunOwned(snapshot.ProjectID, snapshot.AgentID, api.Slugify(opts.Name), snapshot.RunID) {
			return fmt.Errorf("retained effective runtime snapshot run is not owned")
		}
	} else if cfg.RuntimeUpdateVersion <= snapshot.RuntimeUpdateVersion && snapshot.RunID != info.RunID {
		return fmt.Errorf("retained effective runtime snapshot run does not match")
	}
	// An interrupted recovery may have atomically installed its complete
	// config before refreshing this older snapshot. That installed config
	// already contains the preserved effective mounts and permissions.
	if cfg.RuntimeUpdateVersion > snapshot.RuntimeUpdateVersion {
		return nil
	}
	if snapshot.RuntimeUpdateVersion > cfg.RuntimeUpdateVersion {
		return fmt.Errorf("retained effective runtime snapshot run or admission does not match")
	}
	cfg.Docker, cfg.Volumes = config.CloneDockerConfig(snapshot.Docker), append([]api.VolumeMount(nil), snapshot.Volumes...)
	cfg.User, cfg.Model, cfg.ThinkingLevel = snapshot.User, snapshot.Model, snapshot.ThinkingLevel
	// These selections were observed after all env tiers and alias resolution.
	// Replaying an older config expression must not replace the observed value.
	delete(cfg.Env, "SCION_MODEL")
	delete(cfg.Env, "SCION_THINKING_LEVEL")
	return nil
}

// normalizeRuntimeSelection records the same resolved model and thinking level
// the runtime receives so an acknowledged-loss retry hashes that selection.
func normalizeRuntimeSelection(cfg *api.ScionConfig, env []string) {
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "SCION_MODEL":
			cfg.Model = value
		case "SCION_THINKING_LEVEL":
			if level, err := strconv.Atoi(value); err == nil {
				cfg.ThinkingLevel = &level
			}
		}
	}
}

// requesterDockerFromSnapshot keeps the recovery's full installed effective
// config from ranking as requester intent on a later ordinary start.
func requesterDockerFromSnapshot(dir string, cfg *api.ScionConfig, allowOlder ...bool) (*api.DockerConfig, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, runtimeEnvironmentSnapshotFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var snapshot runtimeEnvironmentSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, false, fmt.Errorf("retained snapshot has trailing data")
	}
	agentID, projectID, brokerID := cfg.Env["SCION_AGENT_ID"], cfg.Env["SCION_PROJECT_ID"], cfg.Env["SCION_BROKER_ID"]
	if cfg.Info != nil {
		if cfg.Info.ID != "" {
			agentID = cfg.Info.ID
		}
		if cfg.Info.ProjectID != "" {
			projectID = cfg.Info.ProjectID
		}
		if cfg.Info.RuntimeBrokerID != "" {
			brokerID = cfg.Info.RuntimeBrokerID
		}
	}
	if snapshot.Version != 1 || snapshot.AgentID != agentID || snapshot.ProjectID != projectID || snapshot.RuntimeBrokerID != brokerID || runtime.ValidateRunID(snapshot.RunID) != nil {
		return nil, false, fmt.Errorf("retained requester Docker snapshot identity does not match")
	}
	if snapshot.RuntimeUpdateVersion < cfg.RuntimeUpdateVersion && !(len(allowOlder) > 0 && allowOlder[0]) {
		return nil, false, fmt.Errorf("complete interrupted runtime recovery before an ordinary start")
	}
	if snapshot.RuntimeUpdateVersion > cfg.RuntimeUpdateVersion {
		return nil, false, fmt.Errorf("retained requester Docker snapshot admission does not match")
	}
	return config.CloneDockerConfig(snapshot.RequesterDocker), snapshot.RequesterDockerRecorded, nil
}
