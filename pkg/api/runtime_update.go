package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
)

// RuntimeUpdateRequest explicitly updates creation-time configuration on a
// retained agent. StateVersion admits the transition through the Hub's existing
// optimistic lock; callers refresh it before retrying a rejected request.
type RuntimeUpdateRequest struct {
	StateVersion *int64            `json:"stateVersion"`
	Template     string            `json:"template"`
	Image        string            `json:"image"`
	Config       *ScionConfig      `json:"config,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func (r *RuntimeUpdateRequest) Validate() error {
	if r == nil || r.StateVersion == nil || *r.StateVersion < 0 {
		return fmt.Errorf("runtime update requires a nonnegative stateVersion")
	}
	if strings.TrimSpace(r.Template) == "" || strings.TrimSpace(r.Template) != r.Template {
		return fmt.Errorf("runtime update requires an explicit template")
	}
	if err := imagecheck.ValidateReference(r.Image); err != nil {
		return fmt.Errorf("invalid runtime update image: %w", err)
	}
	if r.Config != nil {
		c := r.Config
		if c.Branch != "" || c.ExplicitWorkspace || c.Hub != nil || c.Kubernetes != nil || c.Info != nil || c.RuntimeUpdateVersion != 0 {
			return fmt.Errorf("runtime update cannot change repository, workspace, Hub identity, or runtime")
		}
		if c.Image != "" && c.Image != r.Image {
			return fmt.Errorf("runtime update image conflicts with config.image")
		}
		for key := range c.Env {
			if strings.HasPrefix(key, "SCION_") && key != "SCION_MODEL" && key != "SCION_THINKING_LEVEL" {
				return fmt.Errorf("runtime update cannot override reserved environment key %q", key)
			}
		}
	}
	return nil
}

// RuntimeRecovery binds an admitted update to its existing Hub identity and
// template revision. It travels through the existing authenticated start
// transport and durable broker-dispatch arguments, rather than creating intake.
type RuntimeRecovery struct {
	Update           RuntimeUpdateRequest `json:"update"`
	AgentID          string               `json:"agentId"`
	ProjectID        string               `json:"projectId"`
	RuntimeBrokerID  string               `json:"runtimeBrokerId"`
	AdmissionVersion int64                `json:"admissionVersion"`
	TemplateID       string               `json:"templateId,omitempty"`
	TemplateHash     string               `json:"templateHash,omitempty"`
}

type runtimeRecoveryKey struct{}

// ContextWithRuntimeRecovery carries invocation metadata through the existing
// start client interface. Transports serialize it explicitly across processes.
func ContextWithRuntimeRecovery(ctx context.Context, recovery *RuntimeRecovery) context.Context {
	return context.WithValue(ctx, runtimeRecoveryKey{}, recovery)
}

func RuntimeRecoveryFromContext(ctx context.Context) *RuntimeRecovery {
	recovery, _ := ctx.Value(runtimeRecoveryKey{}).(*RuntimeRecovery)
	return recovery
}
