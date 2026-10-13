//go:build !no_sqlite

package entadapter

import (
	"context"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRuntimeRecoveryAdmissionAndCompletion(t *testing.T) {
	ctx := context.Background()
	s, project := newTestAgentStore(t)
	original := makeAgent(project, "retained")
	original.Phase = "error"
	exit := 137
	original.ExitCode = &exit
	original.ExitReason = "old error"
	original.Image = "old:tag"
	original.AppliedConfig = &store.AgentAppliedConfig{Image: "old:tag", Model: "retained-model", Env: map[string]string{"OPENAI_API_KEY": "ephemeral"}}
	require.NoError(t, s.CreateAgent(ctx, original))
	version := original.StateVersion
	_, err := s.AdmitAgentRuntimeRecovery(ctx, original.ID, version-1, version)
	require.ErrorIs(t, err, store.ErrVersionConflict)
	after, err := s.GetAgent(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, original.AppliedConfig, after.AppliedConfig)
	require.Equal(t, "error", after.Phase)
	admitted, err := s.AdmitAgentRuntimeRecovery(ctx, original.ID, version, version+1)
	require.NoError(t, err)
	require.Equal(t, version+1, admitted.StateVersion)
	require.Equal(t, version+1, admitted.AppliedConfig.RuntimeUpdateVersion)
	require.Equal(t, "old:tag", admitted.AppliedConfig.Image)
	_, err = s.AdmitAgentRuntimeRecovery(ctx, original.ID, version, version+1)
	require.ErrorIs(t, err, store.ErrVersionConflict)
	// Status reports deliberately do not change StateVersion. Completion
	// installs only recovery fields while preserving their newer activity.
	require.NoError(t, s.UpdateAgentStatus(ctx, original.ID, store.AgentStatusUpdate{Phase: "running", Activity: "waiting_for_input", Message: "new report"}))
	admitted.Template, admitted.Image = "current", "new:tag"
	admitted.AppliedConfig.Image = "new:tag"
	admitted.Phase, admitted.Message = "running", ""
	require.NoError(t, s.UpdateAgentRuntimeRecovery(ctx, admitted, version+1, false, true))
	require.Nil(t, admitted.ExitCode)
	require.Empty(t, admitted.ExitReason)
	require.Equal(t, "waiting_for_input", admitted.Activity)
	require.Equal(t, "new report", admitted.Message)
	require.Equal(t, "new:tag", admitted.AppliedConfig.Image)
	require.Equal(t, "ephemeral", admitted.AppliedConfig.Env["OPENAI_API_KEY"])
	// A later admission owns the result; a delayed completion cannot undo it.
	stale := *admitted
	stale.AppliedConfig = &store.AgentAppliedConfig{RuntimeUpdateVersion: version + 1, Image: "stale:tag"}
	require.NoError(t, s.UpdateAgentStatus(ctx, original.ID, store.AgentStatusUpdate{Phase: "stopped"}))
	newer, err := s.AdmitAgentRuntimeRecovery(ctx, original.ID, admitted.StateVersion, admitted.StateVersion+1)
	require.NoError(t, err)
	stale.StateVersion = newer.StateVersion
	require.ErrorIs(t, s.UpdateAgentRuntimeRecovery(ctx, &stale, version+1, false, true), store.ErrVersionConflict)
	latest, err := s.GetAgent(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, newer.AppliedConfig, latest.AppliedConfig)
}

func TestRuntimeRecoveryCompletionCannotOverwriteConcurrentPatch(t *testing.T) {
	ctx := context.Background()
	s, project := newTestAgentStore(t)
	original := makeAgent(project, "concurrent-patch")
	original.Phase = "suspended"
	original.AppliedConfig = &store.AgentAppliedConfig{Image: "old:tag"}
	require.NoError(t, s.CreateAgent(ctx, original))
	admitted, err := s.AdmitAgentRuntimeRecovery(ctx, original.ID, original.StateVersion, original.StateVersion+1)
	require.NoError(t, err)
	patch, err := s.GetAgent(ctx, original.ID)
	require.NoError(t, err)
	patch.Labels = map[string]string{"newer": "patch"}
	patch.AppliedConfig.Model = "newer-model"
	require.NoError(t, s.UpdateAgent(ctx, patch))
	admitted.Template, admitted.Image = "current", "new:tag"
	admitted.AppliedConfig.Image = "new:tag"
	admitted.Labels = map[string]string{"stale": "recovery"}
	admitted.Phase = "running"
	require.ErrorIs(t, s.UpdateAgentRuntimeRecovery(ctx, admitted, admitted.AppliedConfig.RuntimeUpdateVersion, true, true), store.ErrVersionConflict)
	latest, err := s.GetAgent(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, patch.Labels, latest.Labels)
	require.Equal(t, patch.AppliedConfig, latest.AppliedConfig)
}
