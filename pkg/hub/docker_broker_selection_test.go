//go:build !no_sqlite

package hub

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRuntimeBroker_RequireGPU(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gpu, required bool
		explicit      string
		accepted      bool
	}{
		{name: "explicit GPU broker", gpu: true, required: true, explicit: "gpu-worker", accepted: true},
		{name: "explicit CPU broker refused", required: true, explicit: "gpu-worker"},
		{name: "automatic GPU broker", gpu: true, required: true, accepted: true},
		{name: "automatic CPU broker refused", required: true},
		{name: "CPU broker unchanged without requirement", accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := testServer(t)
			ctx := context.Background()
			broker := &store.RuntimeBroker{ID: tid("docker-gpu-worker"), Name: "GPU Worker", Slug: "gpu-worker", Status: store.BrokerStatusOnline, AutoProvide: true, Capabilities: &store.BrokerCapabilities{NvidiaGPU: tc.gpu}}
			require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
			project := &store.Project{ID: tid("docker-gpu-project"), Name: "Docker Project", Slug: "docker-project"}
			require.NoError(t, st.CreateProject(ctx, project))
			require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))
			ctx = contextWithIdentity(ctx, NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Developer", Email: "dev@localhost"}))
			response := httptest.NewRecorder()
			id, err := srv.resolveRuntimeBroker(ctx, response, tc.explicit, project, tc.required)
			if tc.accepted {
				require.NoError(t, err)
				assert.Equal(t, broker.ID, id)
			} else {
				require.Error(t, err)
				assert.Empty(t, id)
				assert.GreaterOrEqual(t, response.Code, 400)
			}
		})
	}
}
