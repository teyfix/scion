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
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestBrokerInstanceAuthority_ConcurrentCredentialReinitialize(t *testing.T) {
	const brokerID = "broker-current"
	creds := func(id, key string) *brokercredentials.BrokerCredentials {
		return &brokercredentials.BrokerCredentials{BrokerID: id, SecretKey: base64.StdEncoding.EncodeToString([]byte(key)), HubEndpoint: "http://127.0.0.1:1"}
	}
	initial := creds(brokerID, "initial-fixture-key")
	conn := &HubConnection{Name: "credential-race-fixture", Credentials: initial, BrokerID: brokerID, SecretKey: []byte("initial-fixture-key")}
	s := &Server{config: ServerConfig{BrokerID: brokerID, BrokerAuthEnabled: true, BrokerAuthStrictMode: true}, hubConnections: map[string]*HubConnection{"current": conn}}
	// Heartbeat/control channel remain disabled: this exercises the ACTUAL
	// tuple writer and authority reader without any network or live services.
	start := make(chan struct{})
	errors := make(chan error, 3)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 128; i++ {
			id := brokerID
			if i%2 != 0 {
				id = "other-broker"
			}
			if err := conn.Reinitialize(context.Background(), s, creds(id, fmt.Sprintf("fixture-key-%d", i))); err != nil {
				errors <- err
				return
			}
		}
	}()
	for i := 0; i < 2; i++ {
		go func() {
			defer workers.Done()
			<-start
			for j := 0; j < 512; j++ {
				id, err := s.authenticatedInstanceBrokerID()
				if err == nil && id != brokerID {
					errors <- fmt.Errorf("authority accepted mismatched broker")
					return
				}
				conn.mu.RLock()
				expected, decodeErr := base64.StdEncoding.DecodeString(conn.Credentials.SecretKey)
				coherent := decodeErr == nil && conn.BrokerID == conn.Credentials.BrokerID && bytes.Equal(expected, conn.SecretKey)
				conn.mu.RUnlock()
				if !coherent {
					errors <- fmt.Errorf("authority credential tuple was partial")
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestBrokerInstanceAuthority_InvalidRotatedKeyRefuses(t *testing.T) {
	conn := &HubConnection{Name: "invalid-key-fixture", BrokerID: "broker-current", SecretKey: []byte("previous-key"), Credentials: &brokercredentials.BrokerCredentials{BrokerID: "broker-current"}}
	s := &Server{config: ServerConfig{BrokerID: "broker-current", BrokerAuthEnabled: true, BrokerAuthStrictMode: true}, hubConnections: map[string]*HubConnection{"current": conn}}
	// A valid prefix followed by bad base64 must not retain partial/old key bytes.
	err := conn.Reinitialize(context.Background(), s, &brokercredentials.BrokerCredentials{BrokerID: "broker-current", SecretKey: "Zm9v!!!!", HubEndpoint: "http://127.0.0.1:1"})
	if err == nil {
		t.Fatal("invalid rotated key unexpectedly accepted")
	}
	if _, err := s.authenticatedInstanceBrokerID(); err == nil {
		t.Fatal("invalid rotated key retained owning authority")
	}
	if conn.GetStatus() != ConnectionStatusError {
		t.Fatal("failed rotation did not report error")
	}
}

func TestBrokerInstanceAuthority_AuthenticatedCredentialBinding(t *testing.T) {
	for _, name := range []string{"authenticated", "configured-only", "no-key", "mismatch", "ambiguous", "non-strict", "auth-disabled"} {
		t.Run(name, func(t *testing.T) {
			s := &Server{config: ServerConfig{BrokerID: "broker-current", BrokerAuthEnabled: true, BrokerAuthStrictMode: true}, hubConnections: make(map[string]*HubConnection)}
			s.hubConnections["current"] = &HubConnection{BrokerID: "broker-current", SecretKey: []byte("test-key-not-logged"), Credentials: &brokercredentials.BrokerCredentials{BrokerID: "broker-current"}}
			switch name {
			case "configured-only":
				delete(s.hubConnections, "current")
			case "no-key":
				s.hubConnections["current"].SecretKey = nil
			case "mismatch":
				s.hubConnections["current"].Credentials.BrokerID = "agent-claimed"
			case "ambiguous":
				s.hubConnections["other"] = &HubConnection{BrokerID: "other", SecretKey: []byte("test"), Credentials: &brokercredentials.BrokerCredentials{BrokerID: "other"}}
			case "non-strict":
				s.config.BrokerAuthStrictMode = false
			case "auth-disabled":
				s.config.BrokerAuthEnabled = false
			}
			t.Setenv("SCION_BROKER_CONTAINER_ID", "arbitrary-agent-claim")
			id, err := s.authenticatedInstanceBrokerID()
			if (err != nil) != (name != "authenticated") {
				t.Fatalf("unexpected authenticated ownership: %v", err)
			}
			if err == nil && id != "broker-current" {
				t.Fatal("wrong credential identity")
			}
		})
	}
}

type authorityCaptureRuntime struct {
	*runtime.MockRuntime
	path     string
	identity func() (string, error)
}

func (r *authorityCaptureRuntime) ConfigureBrokerInstanceAuthority(path string, identity func() (string, error)) {
	r.path, r.identity = path, identity
}

func TestBrokerInstanceAuthority_RuntimeWiring(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultServerConfig()
	cfg.HubEnabled = false
	cfg.InstanceAuthorityPath = "/run/scion-broker-authority/instance.json"
	rt := &authorityCaptureRuntime{MockRuntime: &runtime.MockRuntime{}}
	s := New(cfg, &mockManager{}, rt)
	if rt.path != cfg.InstanceAuthorityPath || rt.identity == nil {
		t.Fatal("broker did not inject supported authority")
	}
	if _, err := rt.identity(); err == nil {
		t.Fatal("unregistered broker was granted ownership")
	}
	newRuntime := &authorityCaptureRuntime{MockRuntime: &runtime.MockRuntime{}}
	s.SwapRuntime(newRuntime)
	if newRuntime.path != cfg.InstanceAuthorityPath || newRuntime.identity == nil {
		t.Fatal("runtime reload lost broker-only authority")
	}
}
