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
	"fmt"

	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func (s *Server) configureInstanceAuthority(rt scionrt.Runtime) {
	if configurable, ok := rt.(scionrt.BrokerInstanceAuthorityConfigurer); ok {
		configurable.ConfigureBrokerInstanceAuthority(s.config.InstanceAuthorityPath, s.authenticatedInstanceBrokerID)
	}
}

// Use the same loaded HMAC credentials as the serving broker. Merely setting
// BrokerID in configuration, or in an agent request/environment, grants nothing.
func (s *Server) authenticatedInstanceBrokerID() (string, error) {
	if !s.config.BrokerAuthEnabled || !s.config.BrokerAuthStrictMode {
		return "", fmt.Errorf("strict authenticated broker ownership unavailable")
	}
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	id := ""
	for _, conn := range s.hubConnections {
		conn.mu.RLock()
		if len(conn.SecretKey) > 0 && conn.Credentials != nil && conn.BrokerID != "" && conn.BrokerID == conn.Credentials.BrokerID {
			if id != "" && id != conn.BrokerID {
				conn.mu.RUnlock()
				return "", fmt.Errorf("authenticated broker ownership is ambiguous")
			}
			id = conn.BrokerID
		}
		conn.mu.RUnlock()
	}
	if id == "" || s.config.BrokerID != id {
		return "", fmt.Errorf("configured broker does not match authenticated ownership")
	}
	return id, nil
}
