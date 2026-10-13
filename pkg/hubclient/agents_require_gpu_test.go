// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentService_CreateRequireGPUTransport(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted default", true: "required"}[required], func(t *testing.T) {
			var body map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/projects/project-gpu/agents", r.URL.Path)
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"agent":{"id":"agent-gpu","slug":"gpu-agent","phase":"created"}}`))
			}))
			defer server.Close()
			client, err := New(server.URL)
			require.NoError(t, err)
			_, err = client.ProjectAgents("project-gpu").Create(context.Background(), &CreateAgentRequest{
				Name: "gpu-agent", ProjectID: "project-gpu", RequireGPU: required,
			})
			require.NoError(t, err)
			if required {
				assert.Equal(t, true, body["requireGpu"])
			} else {
				assert.NotContains(t, body, "requireGpu")
			}
			assert.NotContains(t, body, "require_gpu", "request must use the Hub's existing camelCase field")
		})
	}
}
