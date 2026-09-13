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

package config

import "testing"

func TestBrokerInstanceAuthority_ConfigBoundary(t *testing.T) {
	const path = "/run/scion-broker-authority/instance.json"
	v1 := &V1ServerConfig{Broker: &V1BrokerConfig{InstanceAuthorityPath: path}}
	global := ConvertV1ServerToGlobalConfig(v1)
	if global.RuntimeBroker.InstanceAuthorityPath != path {
		t.Fatal("broker-only path lost during conversion")
	}
	if ConvertGlobalToV1ServerConfig(global).Broker.InstanceAuthorityPath != path {
		t.Fatal("broker-only path lost on round trip")
	}
	t.Setenv("SCION_SERVER_BROKER_INSTANCEAUTHORITYPATH", path)
	global.RuntimeBroker.InstanceAuthorityPath = ""
	if err := applyEnvOverrides(global); err != nil {
		t.Fatal(err)
	}
	if global.RuntimeBroker.InstanceAuthorityPath != path {
		t.Fatal("supported broker env input not applied")
	}
	if serverEnvToOpsettingsKey("BROKER_INSTANCEAUTHORITYPATH") != "server.broker.instance_authority_path" {
		t.Fatal("versioned settings env key mismatch")
	}
}
