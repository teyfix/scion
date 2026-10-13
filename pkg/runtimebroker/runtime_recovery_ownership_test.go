package runtimebroker

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRuntimeRecoveryOwnershipAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "wrong-slug", "foreign-instance", "deleting", "deleted", "superseded", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			owned := NewOwnershipStore(dir, "broker")
			if scenario != "missing" {
				require.NoError(t, owned.BeginRun("project", "agent", "slug", "run"))
			}
			target, slug, version := owned, "slug", int64(2)
			switch scenario {
			case "wrong-slug":
				slug = "other"
			case "foreign-instance":
				target = NewOwnershipStore(dir, "foreign")
			case "deleting":
				require.NoError(t, owned.SetRecordState("project", "agent", OwnershipStateDeleting))
			case "deleted":
				require.NoError(t, owned.SetRecordState("project", "agent", OwnershipStateDeleting))
				require.NoError(t, owned.MarkFilesRemoved("project", "agent"))
				require.NoError(t, owned.SetRunState("project", "agent", "run", OwnershipStateDeleted))
				require.NoError(t, owned.SetRecordState("project", "agent", OwnershipStateDeleted))
			case "superseded":
				require.NoError(t, owned.AdmitRuntimeRecovery("project", "agent", slug, 3))
			case "retry":
				require.NoError(t, owned.AdmitRuntimeRecovery("project", "agent", slug, version))
				target = NewOwnershipStore(dir, "broker") // durable across broker restart
			}
			err := target.AdmitRuntimeRecovery("project", "agent", slug, version)
			if scenario == "retry" {
				require.NoError(t, err)
				record, ok, err := target.Get("project", "agent")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, version, record.RuntimeRecoveryVersion)
			} else {
				require.Error(t, err)
			}
		})
	}
}
