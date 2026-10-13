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

package runtimebroker

import (
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// createDispatchAdmission distinguishes immutable Hub dispatch from the legacy
// unregistered local API. Request endpoint/env hints are never authority.
func (s *Server) createDispatchAdmission(r *http.Request, agentID, projectID string) (*api.CreateAdmission, error) {
	authority, authenticated := authenticatedBrokerAuthority(r)
	configured := s.hasConfiguredHubAuthority()
	if configured && !authenticated {
		return nil, &startContextError{Status: http.StatusUnauthorized, Message: "CREATE dispatch requires its registered Hub authority"}
	}
	flat := s.flatInstance()
	hosted := authenticated || configured || flat != nil
	if !hosted && projectID == "" {
		// ID-only creates are used by the local API's in-flight supersede path.
		// Their transient files are not immutable retained Hub identity.
		return nil, nil
	}
	brokerID := s.config.BrokerID
	if authenticated {
		brokerID = authority.brokerID
	} else if flat != nil {
		brokerID = flat.Identity.RuntimeBrokerID
	}
	if hosted && (agentID == "" || projectID == "" || brokerID == "") {
		return nil, &startContextError{Status: http.StatusBadRequest, Message: "Hub CREATE dispatch requires agent, project and runtime broker identity"}
	}
	return &api.CreateAdmission{AgentID: agentID, ProjectID: projectID, RuntimeBrokerID: brokerID}, nil
}
