package entadapter

import (
	"context"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"time"
)

func (s *AgentStore) UpdateAgentRuntimeRecovery(ctx context.Context, a *store.Agent, admissionVersion int64, replaceLabels bool) error {
	var result *store.Agent
	err := s.withLockedAgent(ctx, a.ID, func(ctx context.Context, c *ent.Client, current *ent.Agent, now time.Time) (bool, error) {
		latest := entAgentToStore(current)
		if !claimEligible(current) || store.DeletionHoldsRow(current.DeletionState, current.DeletionLeaseAt, now) || latest.StateVersion != a.StateVersion || latest.AppliedConfig == nil || latest.AppliedConfig.RuntimeUpdateVersion != admissionVersion || (a.RunID != "" && current.RunID != a.RunID) {
			return false, store.ErrVersionConflict
		}
		upd := c.Agent.UpdateOneID(current.ID).SetUpdated(now).SetStateVersion(a.StateVersion + 1)
		if current.Phase == "starting" {
			upd.SetPhase(a.Phase).SetMessage(a.Message)
			if a.Phase != "error" {
				upd.SetContainerStatus(a.ContainerStatus).SetRuntimeState(a.RuntimeState).ClearExitCode().SetExitReason("").SetStalledFromActivity("")
			}
		}
		if a.Phase != "error" {
			upd.SetAppliedConfig(marshalAppliedConfig(a.AppliedConfig)).SetTemplate(a.Template).SetImage(a.Image).SetDetached(a.Detached).SetHarnessConfig(harnessConfigOf(a.AppliedConfig))
			if replaceLabels {
				upd.SetLabels(a.Labels)
			}
		}
		updated, err := upd.Save(ctx)
		if err != nil {
			return false, mapError(err)
		}
		result = entAgentToStore(updated)
		return true, nil
	})
	if err == nil {
		*a = *result
	}
	return err
}

// AdmitAgentRuntimeRecovery changes only the recovery admission document and
// starting phase under the same row lock used by start claims and deletion.
func (s *AgentStore) AdmitAgentRuntimeRecovery(ctx context.Context, id string, version, admission int64) (*store.Agent, error) {
	var result *store.Agent
	err := s.withLockedAgent(ctx, id, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if row.StateVersion != version || admission != version+1 {
			return false, store.ErrVersionConflict
		}
		if !claimEligible(row) || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now) {
			return false, store.ErrDeleteInProgress
		}
		current := entAgentToStore(row)
		if current.AppliedConfig == nil || (row.Phase != "starting" && row.Phase != "stopped" && row.Phase != "suspended" && row.Phase != "error") {
			return false, store.ErrVersionConflict
		}
		current.AppliedConfig.RuntimeUpdateVersion = admission
		data := marshalAppliedConfig(current.AppliedConfig)
		updated, err := c.Agent.UpdateOneID(row.ID).SetAppliedConfig(data).SetPhase("starting").SetMessage("").SetUpdated(now).SetStateVersion(admission).Save(ctx)
		if err != nil {
			return false, mapError(err)
		}
		result = entAgentToStore(updated)
		return true, nil
	})
	return result, err
}
