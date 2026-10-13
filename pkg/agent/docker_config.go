package agent

import (
	"fmt"
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
