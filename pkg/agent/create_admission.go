package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"os"
	"path/filepath"
	"slices"
)

type createAdmissionContextKey struct{}

// ErrCreateAdmissionConflict identifies a refusal that must preserve retained files.
var ErrCreateAdmissionConflict = errors.New("CREATE admission conflict")

func completeCreateAdmission(admission *api.CreateAdmission) bool {
	return admission != nil && admission.AgentID != "" && admission.ProjectID != "" && admission.RuntimeBrokerID != ""
}

// ValidateCreateAdmission checks retained state without repairing or changing it.
// Only the CREATE handler supplies this identity; ordinary lifecycle calls are
// deliberately unaffected. Call it before any lifecycle or workspace effects.
func ValidateCreateAdmission(opts api.StartOptions) (retainedState bool, err error) {
	admission := opts.CreateAdmission
	if admission == nil {
		return retainedState, nil
	}
	conflict := func(message string, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrCreateAdmissionConflict, message, err)
		}
		return fmt.Errorf("%w: %s", ErrCreateAdmissionConflict, message)
	}
	if opts.RuntimeRecovery != nil || opts.Resume {
		return retainedState, conflict("conflicting lifecycle mode", nil)
	}
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return retainedState, conflict("resolve retained project", err)
	}
	agentDir, _, err := AgentStateDir(projectDir, opts.Name, opts.SharedWorkspace, opts.HubProjectID, opts.BrokerMode || opts.HubProjectID != "")
	if err != nil {
		return retainedState, conflict("resolve retained agent directory", err)
	}
	home := config.GetAgentHomePath(projectDir, opts.Name)
	if _, err := os.Lstat(agentDir); os.IsNotExist(err) {
		if _, homeErr := os.Lstat(home); os.IsNotExist(homeErr) {
			return retainedState, nil // No retained artifacts: normal fresh provisioning.
		}
		return retainedState, conflict("retained home exists without its agent directory", nil)
	} else if err != nil {
		return retainedState, conflict("inspect retained agent directory", err)
	}
	if !completeCreateAdmission(admission) {
		return retainedState, conflict("missing authoritative identity for retained state", nil)
	}
	retainedState = true
	data, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	if err != nil {
		return retainedState, conflict("read retained identity", err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return retainedState, conflict("parse retained identity", err)
	}
	if info.ID != admission.AgentID || info.ProjectID != admission.ProjectID || info.RuntimeBrokerID != admission.RuntimeBrokerID || api.Slugify(info.Name) != api.Slugify(opts.Name) {
		return retainedState, conflict("retained agent, project or broker identity does not match", nil)
	}
	if config.GetScionAgentConfigPath(agentDir) == "" {
		return retainedState, conflict("retained configuration is missing", nil)
	}
	retained, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		return retainedState, conflict("read retained configuration", err)
	}
	for key, expected := range map[string]string{"SCION_AGENT_ID": admission.AgentID, "SCION_PROJECT_ID": admission.ProjectID, "SCION_BROKER_ID": admission.RuntimeBrokerID} {
		if value, present := retained.Env[key]; present && value != expected {
			return retainedState, conflict("retained configuration identity is inconsistent", nil)
		}
	}
	if projectID := retained.Env["SCION_PROJECT_ID"]; projectID != "" && projectID != admission.ProjectID {
		return retainedState, conflict("retained configuration project identity is inconsistent", nil)
	}
	if opts.InlineConfig == nil || opts.InlineConfig.Docker == nil || opts.InlineConfig.Docker.NvidiaGPU == nil {
		return retainedState, nil
	}
	// Reconstruct exactly the existing agent's native template/config layers.
	// Current CREATE config contributes only its explicit optional GPU intent.
	effective := &api.ScionConfig{}
	templateName := info.Template
	if templateName == "" {
		templateName = "default"
		settings, _, err := config.LoadEffectiveSettings(projectDir)
		if err != nil {
			return retainedState, conflict("read retained template defaults", err)
		}
		if settings != nil && settings.DefaultTemplate != "" {
			templateName = settings.DefaultTemplate
		}
	}
	// The normal chain resolver can hydrate/download missing templates. This
	// preflight must only read already-present layers; unresolved layers reject.
	templateNames := []string{"default"}
	if templateName != "default" {
		templateNames = append(templateNames, templateName)
	}
	for _, name := range templateNames {
		if config.IsRemoteURI(name) {
			return retainedState, conflict("retained template requires download", nil)
		}
		template, err := config.FindTemplateInProjectPath(name, opts.ProjectPath)
		if err != nil {
			return retainedState, conflict("resolve retained template", err)
		}
		cfg, err := template.LoadConfig()
		if err != nil {
			return retainedState, conflict("read retained template", err)
		}
		effective = config.MergeScionConfig(effective, cfg)
	}
	effective = config.MergeScionConfig(effective, retained)
	candidate := config.MergeScionConfig(effective, &api.ScionConfig{Docker: &api.DockerConfig{NvidiaGPU: opts.InlineConfig.Docker.NvidiaGPU}})
	if !slices.Equal(config.ResolveDockerDevices(effective.Docker), config.ResolveDockerDevices(candidate.Docker)) {
		return retainedState, conflict("explicit GPU intent changes retained device grants", nil)
	}
	return retainedState, nil
}
