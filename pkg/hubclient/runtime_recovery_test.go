package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestRecoverRuntimeUsesExistingIdentityAndNativeStartContract(t *testing.T) {
	version := int64(7)
	update := &api.RuntimeUpdateRequest{StateVersion: &version, Template: "current", Image: "registry.test/scion:current", Config: &api.ScionConfig{Env: map[string]string{"APP_DOMAIN": "preview.test"}, Docker: &api.DockerConfig{Networks: []string{"preview"}, Labels: map[string]string{"traefik.enable": "true"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/agents/existing-id/start", r.URL.Path)
		var request struct {
			RuntimeUpdate *api.RuntimeUpdateRequest `json:"runtimeUpdate"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, update, request.RuntimeUpdate)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(Agent{ID: "existing-id", Phase: "running"}))
	}))
	defer server.Close()
	client, err := New(server.URL)
	require.NoError(t, err)
	agent, err := client.Agents().RecoverRuntime(context.Background(), "existing-id", update)
	require.NoError(t, err)
	require.Equal(t, "existing-id", agent.ID)
	require.Equal(t, "running", agent.Phase)
}
