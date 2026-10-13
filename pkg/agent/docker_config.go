package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// resolveDockerConfig gives runtime and profile policy the lowest precedence.
// Retained recovery uses its recorded permissions instead of current defaults.
func resolveDockerConfig(settings *config.VersionedSettings, profile string, own *api.DockerConfig) *api.DockerConfig {
	var defaults *api.DockerConfig
	if settings != nil {
		if profile == "" {
			profile = settings.ActiveProfile
		}
		if p, ok := settings.Profiles[profile]; ok {
			if rt, ok := settings.Runtimes[p.Runtime]; ok {
				defaults = config.MergeDockerConfig(defaults, rt.Docker)
			}
			defaults = config.MergeDockerConfig(defaults, p.Docker)
		}
	}
	return config.MergeDockerConfig(defaults, own)
}

func validateDockerDeviceRuntime(runtimeName string, devices []string) error {
	if len(devices) > 0 && runtimeName != "docker" && runtimeName != "podman" && runtimeName != "mock" {
		return fmt.Errorf("Docker device requests require a Docker-compatible runtime, not %q", runtimeName)
	}
	return nil
}

// liveTemplateDockerConfig reads only the live template chain. Docker is the
// deliberate template environment exception; it never joins requester config.
func liveTemplateDockerConfig(opts api.StartOptions, cfg *api.ScionConfig, projectDir string) (*api.DockerConfig, error) {
	template := opts.Template
	if template == "" {
		template = opts.TemplateEnvSource
	}
	if template == "" && cfg != nil && cfg.Info != nil {
		template = cfg.Info.Template
	}
	if template == "" {
		template = "default"
	}
	chain, err := config.GetTemplateChainInProject(template, projectDir)
	if err != nil {
		// Existing agents may have an unresolvable provenance-pinned template;
		// upstream keeps their recorded job config, with no live template tier.
		if opts.Template == "" && cfg != nil {
			return nil, nil
		}
		return nil, err
	}
	var docker *api.DockerConfig
	for _, layer := range chain {
		cfg, err := layer.LoadConfig()
		if err != nil {
			return nil, err
		}
		docker = config.MergeDockerConfig(docker, cfg.Docker)
	}
	return docker, nil
}

// EffectiveStartDockerConfig resolves the network policy before the broker
// chooses container Hub URLs. Start validates and applies the same tiers.
func EffectiveStartDockerConfig(ctx context.Context, opts api.StartOptions, owner *OwnerScope) (*api.DockerConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return nil, err
	}
	if opts.RuntimeRecovery != nil {
		retained, err := readRetainedRuntimeState(opts, projectDir, nil, owner)
		if err != nil {
			return nil, err
		}
		requested := opts.RuntimeRecovery.Update.Config
		if requested != nil {
			return config.MergeDockerConfig(retained.config.Docker, requested.Docker), nil
		}
		return config.CloneDockerConfig(retained.config.Docker), nil
	}
	var own *api.DockerConfig
	var stored *api.ScionConfig
	if !opts.FreshProvision {
		dir, _, err := AgentStateDir(projectDir, opts.Name, opts.SharedWorkspace, opts.HubProjectID, opts.BrokerMode || opts.HubProjectID != "")
		if err != nil {
			return nil, err
		}
		cfg, err := (&config.Template{Path: dir}).LoadConfig()
		if err == nil {
			own = cfg.Docker
			stored = cfg
			if data, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(projectDir, opts.Name), "agent-info.json")); err == nil {
				var info api.AgentInfo
				if json.Unmarshal(data, &info) == nil {
					stored.Info = &info
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) && config.GetScionAgentConfigPath(dir) != "" {
			return nil, err
		}
	}
	if stored != nil && stored.RuntimeUpdateVersion > 0 {
		dir, _, err := AgentStateDir(projectDir, opts.Name, opts.SharedWorkspace, opts.HubProjectID, opts.BrokerMode || opts.HubProjectID != "")
		if err != nil {
			return nil, err
		}
		if requester, recorded, err := requesterDockerFromSnapshot(dir, stored); err != nil {
			return nil, err
		} else if recorded {
			own = requester
		}
	}
	if opts.InlineConfig != nil {
		own = config.MergeDockerConfig(own, opts.InlineConfig.Docker)
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		return nil, err
	}
	template, err := liveTemplateDockerConfig(opts, stored, projectDir)
	if err != nil {
		// Create without a named template resolves its default during Provision.
		if opts.FreshProvision && opts.Template == "" {
			template = nil
		} else {
			return nil, err
		}
	}
	return resolveDockerConfig(settings, opts.Profile, config.MergeDockerConfig(template, own)), nil
}
