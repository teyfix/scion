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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// makeTestCreds creates BrokerCredentials with a base64-encoded secret key.
func makeTestCreds(name, brokerID, hubEndpoint string) *brokercredentials.BrokerCredentials {
	secretKey := base64.StdEncoding.EncodeToString([]byte("test-secret-key-32bytes!" + name + "12"))
	return &brokercredentials.BrokerCredentials{
		Name:         name,
		BrokerID:     brokerID,
		SecretKey:    secretKey,
		HubEndpoint:  hubEndpoint,
		AuthMode:     brokercredentials.AuthModeHMAC,
		RegisteredAt: time.Now(),
	}
}

// newTestServerWithInMemoryCreds creates a server with in-memory credentials for a "local" connection.
func newTestServerWithInMemoryCreds(creds *brokercredentials.BrokerCredentials) *Server {
	cfg := DefaultServerConfig()
	cfg.BrokerID = creds.BrokerID
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = creds.HubEndpoint
	cfg.InMemoryCredentials = creds
	// Most tests in this file focus on hub connection behavior, not auth gates.
	cfg.BrokerAuthEnabled = false
	cfg.ForceRuntime = "mock"

	tempDir, err := os.MkdirTemp("", "scion-broker-test-*")
	if err == nil {
		cfg.TemplateCacheDir = filepath.Join(tempDir, "cache", "templates")
		cfg.StateDir = filepath.Join(tempDir, "state")
	}

	mgr := &mockManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt)
}

func TestHubConnection_SingleConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	defer srv.hubMu.RUnlock()

	if len(srv.hubConnections) != 1 {
		t.Fatalf("expected 1 hub connection, got %d", len(srv.hubConnections))
	}

	conn, ok := srv.hubConnections["local"]
	if !ok {
		t.Fatal("expected 'local' connection to exist")
	}

	if conn.BrokerID != "broker-1" {
		t.Errorf("expected BrokerID 'broker-1', got %q", conn.BrokerID)
	}

	if conn.HubEndpoint != "http://localhost:8080" {
		t.Errorf("expected HubEndpoint 'http://localhost:8080', got %q", conn.HubEndpoint)
	}

	if conn.HubClient == nil {
		t.Error("expected HubClient to be set")
	}

	if len(conn.SecretKey) == 0 {
		t.Error("expected SecretKey to be decoded")
	}
}

func TestHubConnection_MultipleConnections(t *testing.T) {
	// Create a server with InMemory ("local") + MultiStore credentials
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write a second credential file
	prodCreds := makeTestCreds("hub-prod", "broker-prod", "https://hub.prod.example.com")
	prodData, _ := json.MarshalIndent(prodCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), prodData, 0600); err != nil {
		t.Fatal(err)
	}

	localCreds := makeTestCreds("local", "broker-local", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-local"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds
	cfg.TemplateCacheDir = filepath.Join(tmpDir, "cache", "templates")
	cfg.StateDir = filepath.Join(tmpDir, "state")

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// The multi-store was initialized but pointed to default dir.
	// Manually set up the multi-store and add the prod connection.
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	defer srv.hubMu.RUnlock()

	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 hub connections, got %d", len(srv.hubConnections))
	}

	if _, ok := srv.hubConnections["local"]; !ok {
		t.Error("expected 'local' connection")
	}
	if _, ok := srv.hubConnections["hub-prod"]; !ok {
		t.Error("expected 'hub-prod' connection")
	}
}

func TestMultiKeyBrokerAuth_MatchesAnyKey(t *testing.T) {
	secret1 := []byte("secret-key-for-hub-1-32bytes!!!!")
	secret2 := []byte("secret-key-for-hub-2-32bytes!!!!")

	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", brokerID: "broker-1", secretKey: secret1},
		{hubName: "hub-2", brokerID: "broker-1", secretKey: secret2},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request signed with secret1 should pass
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req1, "broker-1", secret1)
	rr1 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Errorf("Expected 200 for request signed with hub-1 key, got %d", rr1.Code)
	}

	// Request signed with secret2 should also pass
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req2, "broker-1", secret2)
	rr2 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("Expected 200 for request signed with hub-2 key, got %d", rr2.Code)
	}

	// Request signed with unknown key should fail
	wrongSecret := []byte("wrong-secret-key-32bytes!!!!!!!!")
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req3, "broker-1", wrongSecret)
	rr3 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for request signed with wrong key, got %d", rr3.Code)
	}
}

func TestMultiKeyBrokerAuth_Disabled(t *testing.T) {
	middleware := NewMultiKeyBrokerAuthMiddleware(false, 5*time.Minute, false)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 when disabled, got %d", rr.Code)
	}
}

func TestMultiKeyBrokerAuth_AllowUnauthenticated(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, true)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", brokerID: "broker-1", secretKey: secret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers should pass
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 for unauthenticated request, got %d", rr.Code)
	}
}

func TestMultiKeyBrokerAuth_UpdateKeys(t *testing.T) {
	oldSecret := []byte("old-secret-key-32bytes!123456789")
	newSecret := []byte("new-secret-key-32bytes!987654321")

	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", brokerID: "broker-1", secretKey: oldSecret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request with old key should work
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req1, "broker-1", oldSecret)
	rr1 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Errorf("Expected 200 with old key, got %d", rr1.Code)
	}

	// Update keys to new secret only
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", brokerID: "broker-1", secretKey: newSecret},
	})

	// Request with old key should now fail
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req2, "broker-1", oldSecret)
	rr2 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 with old key after update, got %d", rr2.Code)
	}

	// Request with new key should work
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req3, "broker-1", newSecret)
	rr3 := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Errorf("Expected 200 with new key, got %d", rr3.Code)
	}
}

func TestMultiKeyBrokerAuth_ExpiredTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	middleware.UpdateKeys([]secretKeyEntry{
		{hubName: "hub-1", brokerID: "broker-1", secretKey: secret},
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with old timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	oldTimestamp := time.Now().Add(-10 * time.Minute).Unix()
	ts := fmt.Sprintf("%d", oldTimestamp)
	nonce := "test-nonce"
	req.Header.Set(apiclient.HeaderBrokerID, "broker-1")
	req.Header.Set(apiclient.HeaderTimestamp, ts)
	req.Header.Set(apiclient.HeaderNonce, nonce)
	canonical := apiclient.BuildCanonicalString(req, ts, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for expired timestamp, got %d", rr.Code)
	}
}

func TestHubConnectionReinitializePublishesAuthorityAtomically(t *testing.T) {
	oldKey := []byte("old-connection-secret")
	newKey := []byte("new-connection-secret")
	oldCreds := &brokercredentials.BrokerCredentials{
		Name:        "hub-a",
		BrokerID:    "broker-a",
		HubEndpoint: "https://hub-a.example",
		SecretKey:   base64.StdEncoding.EncodeToString(oldKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	newCreds := &brokercredentials.BrokerCredentials{
		Name:        "hub-a",
		BrokerID:    "broker-b",
		HubEndpoint: "https://hub-b.example",
		SecretKey:   base64.StdEncoding.EncodeToString(newKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	conn := &HubConnection{
		Name:        oldCreds.Name,
		HubEndpoint: oldCreds.HubEndpoint,
		BrokerID:    oldCreds.BrokerID,
		AuthMode:    oldCreds.AuthMode,
		Credentials: oldCreds,
		SecretKey:   oldKey,
	}
	conn.authorityReady.Store(true)
	srv := &Server{
		config: ServerConfig{
			BrokerAuthEnabled:    true,
			BrokerAuthStrictMode: true,
		},
		hubConnections: map[string]*HubConnection{"hub-a": conn},
	}
	srv.buildAuthMiddleware()

	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan brokerRequestAuthority, 1)
	handler := srv.brokerAuthMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, ok := authenticatedBrokerAuthority(r)
		if !ok {
			t.Error("authenticated request had no bound authority")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		observed <- *authority
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	oldResponse := httptest.NewRecorder()
	oldRequest := httptest.NewRequest(http.MethodPost, "/api/v1/agents/retained/start", strings.NewReader(`{"runtimeRecovery":{}}`))
	oldRequest.Header.Set("X-Scion-Hub-Connection", "hub-a")
	signRequest(oldRequest, oldCreds.BrokerID, oldKey)
	requestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(oldResponse, oldRequest)
		close(requestDone)
	}()
	<-entered

	reinitializeDone := make(chan error, 1)
	go func() {
		reinitializeDone <- conn.Reinitialize(context.Background(), srv, newCreds)
	}()
	select {
	case err := <-reinitializeDone:
		t.Fatalf("Reinitialize published while an admitted request held its authority lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-requestDone
	if oldResponse.Code != http.StatusOK {
		t.Fatalf("in-flight old authority status %d, want %d", oldResponse.Code, http.StatusOK)
	}
	oldAuthority := <-observed
	if oldAuthority.brokerID != oldCreds.BrokerID || oldAuthority.hubEndpoint != oldCreds.HubEndpoint {
		t.Fatalf("in-flight authority changed: broker=%q endpoint=%q", oldAuthority.brokerID, oldAuthority.hubEndpoint)
	}
	if err := <-reinitializeDone; err != nil {
		t.Fatalf("Reinitialize: %v", err)
	}

	// The stale signature/identity cannot cross the publication boundary.
	staleRequest := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	staleRequest.Header.Set("X-Scion-Hub-Connection", "hub-a")
	signRequest(staleRequest, oldCreds.BrokerID, oldKey)
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusUnauthorized {
		t.Fatalf("stale authority status %d, want %d", staleResponse.Code, http.StatusUnauthorized)
	}

	newRequest := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	newRequest.Header.Set("X-Scion-Hub-Connection", "hub-a")
	signRequest(newRequest, newCreds.BrokerID, newKey)
	newResponse := httptest.NewRecorder()
	srv.brokerAuthMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, ok := authenticatedBrokerAuthority(r)
		if !ok || authority.brokerID != newCreds.BrokerID || authority.hubEndpoint != newCreds.HubEndpoint {
			t.Fatalf("new request observed incoherent authority: %#v", authority)
		}
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(newResponse, newRequest)
	if newResponse.Code != http.StatusOK {
		t.Fatalf("new authority status %d, want %d: %s", newResponse.Code, http.StatusOK, newResponse.Body.String())
	}
}

func TestAuthMiddlewarePublicationOrdersTwoConnectionReinitialize(t *testing.T) {
	const (
		connectionA = "hub-a"
		connectionB = "hub-b"
		brokerA     = "broker-a"
		oldBrokerB  = "broker-b-old"
		newBrokerB  = "broker-b-new"
	)
	keyA := []byte("connection-a-current-secret")
	oldKeyB := []byte("connection-b-stale-secret")
	newKeyB := []byte("connection-b-current-secret")

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hub.Close()

	srv := newTestServer(t)
	connA := configureAuthenticatedHubFixture(t, srv, connectionA, hub.URL, brokerA, keyA)
	connB := configureAuthenticatedHubFixture(t, srv, connectionB, hub.URL, oldBrokerB, oldKeyB)
	oldCredsB := &brokercredentials.BrokerCredentials{
		Name:        connectionB,
		BrokerID:    oldBrokerB,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(oldKeyB),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	connB.mu.Lock()
	connB.Credentials = oldCredsB
	connB.AuthMode = oldCredsB.AuthMode
	connB.mu.Unlock()

	snapshotBuilt := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	defer func() {
		select {
		case <-releaseSnapshot:
		default:
			close(releaseSnapshot)
		}
	}()
	var paused atomic.Bool
	srv.authMiddlewarePublicationHook = func(stage string, keys []secretKeyEntry) {
		if stage != "snapshot" || !paused.CompareAndSwap(false, true) {
			return
		}
		foundOldB := false
		for _, key := range keys {
			if key.hubName == connectionB && bytes.Equal(key.secretKey, oldKeyB) {
				foundOldB = true
				break
			}
		}
		if !foundOldB {
			t.Error("paused auth snapshot did not contain the stale connection-B key")
		}
		close(snapshotBuilt)
		<-releaseSnapshot
	}

	firstBuildDone := make(chan struct{})
	go func() {
		srv.buildAuthMiddleware()
		close(firstBuildDone)
	}()
	select {
	case <-snapshotBuilt:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first auth snapshot")
	}
	if srv.authMiddlewarePublicationMu.TryLock() {
		srv.authMiddlewarePublicationMu.Unlock()
		t.Fatal("auth snapshot was not serialized through UpdateKeys publication")
	}

	newCredsB := &brokercredentials.BrokerCredentials{
		Name:        connectionB,
		BrokerID:    newBrokerB,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(newKeyB),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	reinitializeActive := make(chan struct{})
	srv.hubConnectionLifecycleHook = func(stage string) {
		if stage == "reinitialize-active" {
			close(reinitializeActive)
		}
	}
	reinitializeDone := make(chan error, 1)
	go func() {
		reinitializeDone <- connB.Reinitialize(context.Background(), srv, newCredsB)
	}()
	select {
	case <-reinitializeActive:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection B to begin Reinitialize")
	}

	// The complete older snapshot remains serialized with its publication, so
	// connection B cannot change authority generation until that publication
	// finishes. This ties snapshot order to authority-transition order.
	if !connB.authorityReady.Load() || connB.authorityGeneration.Load() != 0 {
		t.Fatal("connection B authority changed while an older auth snapshot was unpublished")
	}
	select {
	case err := <-reinitializeDone:
		t.Fatalf("connection B Reinitialize completed ahead of the older auth publication: %v", err)
	default:
	}
	close(releaseSnapshot)
	select {
	case <-firstBuildDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first auth publication did not finish")
	}
	select {
	case err := <-reinitializeDone:
		if err != nil {
			t.Fatalf("Reinitialize connection B: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection B Reinitialize did not finish")
	}

	authenticate := func(name, brokerID string, key []byte) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
		request.Header.Set("X-Scion-Hub-Connection", name)
		signRequest(request, brokerID, key)
		response := httptest.NewRecorder()
		srv.brokerAuthMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		return response.Code
	}
	if status := authenticate(connectionA, brokerA, keyA); status != http.StatusNoContent {
		t.Fatalf("surviving connection A status %d, want %d", status, http.StatusNoContent)
	}
	if status := authenticate(connectionB, newBrokerB, newKeyB); status != http.StatusNoContent {
		t.Fatalf("current connection B status %d, want %d", status, http.StatusNoContent)
	}
	if status := authenticate(connectionB, oldBrokerB, oldKeyB); status != http.StatusUnauthorized {
		t.Fatalf("stale connection B status %d, want %d", status, http.StatusUnauthorized)
	}

	if !connA.authorityReady.Load() {
		t.Fatal("serializing connection B authority disturbed connection A")
	}
}

func TestHeartbeatService_ProjectFilter(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "agent-1", ProjectID: "grove-hub1", Phase: "running"},
			{Name: "agent-2", ProjectID: "grove-hub1", Phase: "running"},
			{Name: "agent-3", ProjectID: "grove-hub2", Phase: "running"},
			{Name: "agent-4", ProjectID: "grove-shared", Phase: "running"},
		},
	}

	// Filter: only include grove-hub1 projects
	projectFilter := func(projectID string) bool {
		return projectID == "grove-hub1"
	}

	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, projectFilter, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat

	// Should only include grove-hub1 (2 agents), not grove-hub2 or grove-shared
	if len(heartbeat.Projects) != 1 {
		t.Errorf("Expected 1 project in heartbeat (filtered), got %d", len(heartbeat.Projects))
	}

	if len(heartbeat.Projects) > 0 && heartbeat.Projects[0].ProjectID != "grove-hub1" {
		t.Errorf("Expected grove-hub1, got %q", heartbeat.Projects[0].ProjectID)
	}

	if len(heartbeat.Projects) > 0 && heartbeat.Projects[0].AgentCount != 2 {
		t.Errorf("Expected 2 agents in grove-hub1, got %d", heartbeat.Projects[0].AgentCount)
	}
}

func TestHeartbeatService_NilProjectFilter(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "agent-1", ProjectID: "grove-1", Phase: "running"},
			{Name: "agent-2", ProjectID: "grove-2", Phase: "running"},
		},
	}

	// Nil filter: include all projects
	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, nil, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat
	if len(heartbeat.Projects) != 2 {
		t.Errorf("Expected 2 projects with nil filter, got %d", len(heartbeat.Projects))
	}
}

// TestHydrateHarnessConfig_NoHubInfoFallsBack verifies that when the dispatch
// carries no Hub harness-config ID/hash, hydrateHarnessConfig returns ("", nil)
// so provisioning falls back to the broker's on-disk harness-config search —
// preserving behavior for agents that don't use Hub-managed harness-configs.
func TestHydrateHarnessConfig_NoHubInfoFallsBack(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	if conn == nil {
		t.Fatal("expected 'local' connection to exist")
	}

	cfg := &CreateAgentConfig{HarnessConfig: "claude"} // name only, no Hub ID/hash
	path, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path (fall back to disk), got %q", path)
	}
}

// TestHydrateHarnessConfig_NoResolverIsGraceful verifies that when a harness-config
// ID is present but no resolver is configured (e.g. cache not initialized in
// tests), hydrateHarnessConfig returns ("", nil) rather than erroring.
func TestHydrateHarnessConfig_NoResolverIsGraceful(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	conn.HCResolver = nil
	conn.LocalStorage = nil

	cfg := &CreateAgentConfig{HarnessConfigID: "hc-1", HarnessConfigHash: "abc"}
	path, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path when no resolver, got %q", path)
	}
}

type stubHCSClient struct {
	hubclient.Client
	hcs hubclient.HarnessConfigService
}

func (c *stubHCSClient) HarnessConfigs() hubclient.HarnessConfigService { return c.hcs }

type stubHCService struct {
	hubclient.HarnessConfigService
	getFunc                 func(ctx context.Context, id string) (*hubclient.HarnessConfig, error)
	requestDownloadURLsFunc func(ctx context.Context, id string) (*hubclient.DownloadResponse, error)
	downloadFileFunc        func(ctx context.Context, url string) ([]byte, error)
}

func (s *stubHCService) Get(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
	if s.getFunc != nil {
		return s.getFunc(ctx, id)
	}
	return nil, nil
}

func (s *stubHCService) RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
	if s.requestDownloadURLsFunc != nil {
		return s.requestDownloadURLsFunc(ctx, id)
	}
	return nil, nil
}

func (s *stubHCService) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	if s.downloadFileFunc != nil {
		return s.downloadFileFunc(ctx, url)
	}
	return nil, nil
}

func TestResolveHubConnection_WithHCResolverOnly(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.Lock()
	conn := srv.hubConnections["local"]
	// Clear all other resolvers, keep only HCResolver
	conn.Hydrator = nil
	conn.LocalStorage = nil
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	conn.HCResolver = templatecache.NewHarnessConfigResolver(cache, &stubHCSClient{})
	srv.hubMu.Unlock()

	// 1. Resolve with connection header
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/start", nil)
	req.Header.Set("X-Scion-Hub-Connection", "local")
	resolved := srv.resolveHubConnection(req)
	if resolved != conn {
		t.Errorf("expected connection to resolve when HCResolver is set, got %v", resolved)
	}

	// 2. Resolve via fallback (no connection header)
	reqNoHeader := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/start", nil)
	resolvedFallback := srv.resolveHubConnection(reqNoHeader)
	if resolvedFallback != conn {
		t.Errorf("expected fallback connection to resolve when HCResolver is set, got %v", resolvedFallback)
	}

	// 3. When HCResolver is also nil, resolveHubConnection returns nil
	srv.hubMu.Lock()
	conn.HCResolver = nil
	srv.hubMu.Unlock()

	resolvedNil := srv.resolveHubConnection(req)
	if resolvedNil != nil {
		t.Errorf("expected nil when all resolvers are cleared, got %v", resolvedNil)
	}
}

func TestHydrateHarnessConfig_ResolverColdAndWarmCache(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	cacheDir := t.TempDir()
	cache, err := templatecache.New(cacheDir, 0)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	fileBytes := []byte("harness: claude\nversion: 1\n")
	fileHash := transfer.HashBytes(fileBytes)
	contentHash := "test-content-hash-123"

	downloadCalls := 0
	stubService := &stubHCService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return &hubclient.HarnessConfig{
				ID:          id,
				ContentHash: contentHash,
			}, nil
		},
		requestDownloadURLsFunc: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{
						Path: "config.yaml",
						URL:  "http://hub.local/api/v1/harness-configs/" + id + "/files/config.yaml?raw=1",
						Hash: fileHash,
						Size: int64(len(fileBytes)),
					},
				},
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			downloadCalls++
			return fileBytes, nil
		},
	}

	srv.hubMu.Lock()
	conn := srv.hubConnections["local"]
	conn.HCResolver = templatecache.NewHarnessConfigResolver(cache, &stubHCSClient{hcs: stubService})
	srv.hubMu.Unlock()

	cfg := &CreateAgentConfig{
		HarnessConfigID:   "hc-remote-1",
		HarnessConfigHash: contentHash,
	}

	// Cold cache: downloads and hydrates
	path1, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateHarnessConfig failed on cold cache: %v", err)
	}
	if path1 == "" {
		t.Fatal("expected non-empty path from hydration")
	}
	if downloadCalls != 1 {
		t.Errorf("expected 1 download call on cold cache, got %d", downloadCalls)
	}

	// Verify file was written to disk
	gotContent, err := os.ReadFile(filepath.Join(path1, "config.yaml"))
	if err != nil {
		t.Fatalf("failed to read hydrated file: %v", err)
	}
	if string(gotContent) != string(fileBytes) {
		t.Errorf("hydrated content mismatch: got %q, want %q", string(gotContent), string(fileBytes))
	}

	// Warm cache: returns cached directory without downloading
	path2, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateHarnessConfig failed on warm cache: %v", err)
	}
	if path2 != path1 {
		t.Errorf("expected warm cache to return same path %q, got %q", path1, path2)
	}
	if downloadCalls != 1 {
		t.Errorf("expected no additional download calls on warm cache, got %d", downloadCalls)
	}
}

func TestHydrateHarnessConfig_ResolverHashMismatchFails(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	cacheDir := t.TempDir()
	cache, err := templatecache.New(cacheDir, 0)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	expectedHash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	corruptBytes := []byte("corrupted or tampered content")

	stubService := &stubHCService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return &hubclient.HarnessConfig{
				ID:          id,
				ContentHash: "hash-mismatch-test",
			}, nil
		},
		requestDownloadURLsFunc: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{
						Path: "config.yaml",
						URL:  "http://hub.local/api/v1/harness-configs/" + id + "/files/config.yaml?raw=1",
						Hash: expectedHash,
						Size: int64(len(corruptBytes)),
					},
				},
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			return corruptBytes, nil
		},
	}

	srv.hubMu.Lock()
	conn := srv.hubConnections["local"]
	conn.HCResolver = templatecache.NewHarnessConfigResolver(cache, &stubHCSClient{hcs: stubService})
	srv.hubMu.Unlock()

	cfg := &CreateAgentConfig{
		HarnessConfigID:   "hc-corrupt-1",
		HarnessConfigHash: "hash-mismatch-test",
	}

	path, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err == nil {
		t.Errorf("expected hash mismatch error, got path %q", path)
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("expected error to mention 'hash mismatch', got %v", err)
	}
}

func TestHydrateHarnessConfig_PlainNameFallsBackToLocalDisk(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	cacheDir := t.TempDir()
	cache, err := templatecache.New(cacheDir, 0)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	stubService := &stubHCService{
		getFunc: func(ctx context.Context, ref string) (*hubclient.HarnessConfig, error) {
			t.Errorf("unexpected call to Hub getFunc for plain harness config name: %q", ref)
			return nil, nil
		},
	}

	srv.hubMu.Lock()
	conn := srv.hubConnections["local"]
	conn.HCResolver = templatecache.NewHarnessConfigResolver(cache, &stubHCSClient{hcs: stubService})
	srv.hubMu.Unlock()

	// Plain HarnessConfig name without Hub identity (no ID and no ContentHash)
	// must not hydrate via Hub and must fall back to broker local search.
	cfg := &CreateAgentConfig{
		HarnessConfig: "claude",
	}

	path, err := srv.hydrateHarnessConfig(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path (fallback to disk) for plain harness config, got %q", path)
	}
}

func TestResolveHydrator_WithConnectionHeader(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Verify the hydrator resolves via header
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Hub-Connection", "local")

	hydrator := srv.resolveHydrator(req)
	// In test mode cache is nil, so hydrator is nil -- that's expected
	// What we're testing is the routing logic
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if conn == nil {
		t.Fatal("expected 'local' connection to exist")
	}

	// The hydrator from resolveHydrator should match the connection's hydrator
	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to return the local connection's hydrator")
	}
}

func TestResolveHydrator_FallbackToFirstAvailable(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Request without connection header should fall back to first available
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	hydrator := srv.resolveHydrator(req)

	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to fall back to first available hydrator")
	}
}

func TestResolveHydrator_UnknownConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Request with unknown connection name should fall back
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Hub-Connection", "nonexistent")
	hydrator := srv.resolveHydrator(req)

	// Should fall back to any available hydrator
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if hydrator != conn.Hydrator {
		t.Error("expected resolveHydrator to fall back when connection not found")
	}
}

func TestGlobalProjectRejection_MultiHub(t *testing.T) {
	// Create a server with two connections to simulate multi-hub mode
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a second connection to enable multi-hub mode
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Fatal("expected multi-hub mode with 2 connections")
	}

	// Try to create an agent with empty projectID (global project)
	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status %d for global project in multi-hub mode, got %d: %s",
			http.StatusConflict, w.Code, w.Body.String())
	}

	// Verify error code
	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "global_grove_disabled" {
		t.Errorf("expected error code 'global_grove_disabled', got %q", errObj["code"])
	}
}

func TestGlobalProjectRejection_SingleHub_Allowed(t *testing.T) {
	// Single-hub mode: global project should be allowed
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	if srv.isMultiHubMode() {
		t.Fatal("expected single-hub mode with 1 connection")
	}

	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// In single-hub mode, empty projectID should NOT be rejected
	if w.Code == http.StatusConflict {
		t.Error("single-hub mode should not reject global project agents")
	}
}

func TestGlobalProjectRejection_WithProjectID_MultiHub(t *testing.T) {
	// Multi-hub mode: agents with a specific projectID should be allowed
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, _ := srv.createHubConnection("hub-prod", creds2)
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	body := `{
		"name": "scoped-agent",
		"groveId": "my-project",
		"grovePath": "/some/path/.scion",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT be rejected (has explicit projectID and projectPath)
	if w.Code == http.StatusConflict {
		t.Errorf("expected non-global project to be allowed in multi-hub mode, got %d: %s",
			w.Code, w.Body.String())
	}
}

func TestGlobalProjectRejection_GitProjectWithProjectID_NoPath_MultiHub(t *testing.T) {
	// Multi-hub mode: a git-based project has a projectID but no projectPath or
	// projectSlug (the broker resolves workspace from the git remote). This
	// should NOT be treated as the global project.
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, _ := srv.createHubConnection("hub-prod", creds2)
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	body := `{
		"name": "git-grove-agent",
		"groveId": "abc-123-grove-id",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT be rejected — projectID is set, so this is not the global project
	if w.Code == http.StatusConflict {
		t.Errorf("expected git-based project (projectID set, no path) to be allowed in multi-hub mode, got %d: %s",
			w.Code, w.Body.String())
	}
}

func TestIsMultiHubMode(t *testing.T) {
	srv := newTestServer(t)

	// No connections = not multi-hub
	if srv.isMultiHubMode() {
		t.Error("expected single-hub mode with no connections")
	}

	// Add one connection
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{Name: "hub-1"}
	srv.hubMu.Unlock()

	if srv.isMultiHubMode() {
		t.Error("expected single-hub mode with 1 connection")
	}

	// Add second connection
	srv.hubMu.Lock()
	srv.hubConnections["hub-2"] = &HubConnection{Name: "hub-2"}
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Error("expected multi-hub mode with 2 connections")
	}
}

func TestIsGlobalProject(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name        string
		projectID   string
		projectPath string
		expected    bool
	}{
		{"empty both", "", "", true},
		{"global id", "global", "/some/path", true},
		{"global id empty path", "global", "", true},
		{"empty id with path", "", "/some/path", false},
		{"projectID set empty path", "my-project", "", false},
		{"both set", "my-project", "/some/path", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := srv.isGlobalProject(tc.projectID, tc.projectPath)
			if result != tc.expected {
				t.Errorf("isGlobalProject(%q, %q) = %v, expected %v",
					tc.projectID, tc.projectPath, result, tc.expected)
			}
		})
	}
}

func TestConnectionStatus(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusDisconnected,
	}

	if conn.GetStatus() != ConnectionStatusDisconnected {
		t.Errorf("expected disconnected, got %v", conn.GetStatus())
	}

	conn.setStatus(ConnectionStatusConnected)

	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected, got %v", conn.GetStatus())
	}

	conn.setStatus(ConnectionStatusError)

	if conn.GetStatus() != ConnectionStatusError {
		t.Errorf("expected error, got %v", conn.GetStatus())
	}
}

func TestConnectionStatus_Reconnecting(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusConnected,
	}

	conn.setStatus(ConnectionStatusReconnecting)

	if conn.GetStatus() != ConnectionStatusReconnecting {
		t.Errorf("expected reconnecting, got %v", conn.GetStatus())
	}

	// Simulate reconnect
	conn.setStatus(ConnectionStatusConnected)

	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected after reconnect, got %v", conn.GetStatus())
	}
}

func TestHubConnectionStatus_OnDisconnect(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusConnected,
	}

	// Simulate the callback that would be registered in Start()
	cb := func(connected bool) {
		if connected {
			conn.setStatus(ConnectionStatusConnected)
		} else {
			conn.setStatus(ConnectionStatusReconnecting)
		}
	}

	// Simulate disconnect
	cb(false)

	if conn.GetStatus() != ConnectionStatusReconnecting {
		t.Errorf("expected reconnecting on disconnect, got %v", conn.GetStatus())
	}
}

func TestHubConnectionStatus_OnReconnect(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusReconnecting,
	}

	// Simulate the callback that would be registered in Start()
	cb := func(connected bool) {
		if connected {
			conn.setStatus(ConnectionStatusConnected)
		} else {
			conn.setStatus(ConnectionStatusReconnecting)
		}
	}

	// Simulate reconnect
	cb(true)

	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected on reconnect, got %v", conn.GetStatus())
	}
}

func TestHubConnectionStatus_DisconnectReconnectCycle(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusConnected,
	}

	cb := func(connected bool) {
		if connected {
			conn.setStatus(ConnectionStatusConnected)
		} else {
			conn.setStatus(ConnectionStatusReconnecting)
		}
	}

	// Full cycle: connected -> disconnect -> reconnecting -> reconnect -> connected
	if conn.GetStatus() != ConnectionStatusConnected {
		t.Fatalf("expected initial status connected, got %v", conn.GetStatus())
	}

	cb(false)
	if conn.GetStatus() != ConnectionStatusReconnecting {
		t.Errorf("expected reconnecting after disconnect, got %v", conn.GetStatus())
	}

	cb(true)
	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected after reconnect, got %v", conn.GetStatus())
	}

	// Second cycle
	cb(false)
	if conn.GetStatus() != ConnectionStatusReconnecting {
		t.Errorf("expected reconnecting after second disconnect, got %v", conn.GetStatus())
	}

	cb(true)
	if conn.GetStatus() != ConnectionStatusConnected {
		t.Errorf("expected connected after second reconnect, got %v", conn.GetStatus())
	}
}

func TestCredentialWatcher_AddConnection(t *testing.T) {
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Start with one connection file
	creds1 := makeTestCreds("hub-one", "broker-1", "http://hub1.example.com")
	data1, _ := json.MarshalIndent(creds1, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-one.json"), data1, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server without hub integration to avoid auto-creating connections
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Manually set up multi-store and load initial connections
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	initialCount := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if initialCount != 1 {
		t.Fatalf("expected 1 initial connection, got %d", initialCount)
	}

	// Add a new credential file
	creds2 := makeTestCreds("hub-two", "broker-2", "http://hub2.example.com")
	data2, _ := json.MarshalIndent(creds2, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-two.json"), data2, 0600); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	srv.hubMu.RLock()
	newCount := len(srv.hubConnections)
	_, exists := srv.hubConnections["hub-two"]
	srv.hubMu.RUnlock()

	if newCount != 2 {
		t.Errorf("expected 2 connections after add, got %d", newCount)
	}

	if !exists {
		t.Error("expected 'hub-two' connection to exist after reload")
	}
}

func TestCredentialWatcher_RemoveConnection(t *testing.T) {
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Start with two connections
	creds1 := makeTestCreds("hub-one", "broker-1", "http://hub1.example.com")
	data1, _ := json.MarshalIndent(creds1, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-one.json"), data1, 0600); err != nil {
		t.Fatal(err)
	}

	creds2 := makeTestCreds("hub-two", "broker-2", "http://hub2.example.com")
	data2, _ := json.MarshalIndent(creds2, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-two.json"), data2, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server without hub integration
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 initial connections, got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Remove hub-two credential file
	if err := os.Remove(filepath.Join(credDir, "hub-two.json")); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	srv.hubMu.RLock()
	_, exists := srv.hubConnections["hub-two"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if exists {
		t.Error("expected 'hub-two' connection to be removed after credential deletion")
	}

	if count != 1 {
		t.Errorf("expected 1 connection after removal, got %d", count)
	}
}

func TestCredentialWatcher_RemovalRevokesSignedCreateWithoutDeadlock(t *testing.T) {
	const (
		connectionName = "hub-remove"
		brokerID       = "registered-broker"
		hubEndpoint    = "https://hub.remove.example"
		projectID      = "removal-project"
	)
	key := []byte("credential-removal-secret-key")

	srv := newTestServer(t)
	conn := configureAuthenticatedHubFixture(t, srv, connectionName, hubEndpoint, brokerID, key)
	srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())

	managerEntered := make(chan struct{})
	releaseManager := make(chan struct{})
	defer func() {
		select {
		case <-releaseManager:
		default:
			close(releaseManager)
		}
	}()
	manager := &admissionBoundaryManager{
		Manager: srv.manager,
		beforeStart: func(api.StartOptions) error {
			close(managerEntered)
			<-releaseManager
			return nil
		},
	}
	srv.manager = manager

	hubLocked := make(chan struct{})
	allowHubUnlock := make(chan struct{})
	authorityRevoked := make(chan struct{})
	defer func() {
		select {
		case <-allowHubUnlock:
		default:
			close(allowHubUnlock)
		}
	}()
	srv.hubConnectionLifecycleHook = func(stage string) {
		switch stage {
		case "hub-locked":
			close(hubLocked)
			<-allowHubUnlock
		case "authority-revoked":
			close(authorityRevoked)
		}
	}

	waitFor := func(ch <-chan struct{}, event string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", event)
		}
	}
	sign := func(req *http.Request, nonce string) {
		timestamp := fmt.Sprintf("%d", time.Now().Unix())
		req.Header.Set(apiclient.HeaderBrokerID, brokerID)
		req.Header.Set(apiclient.HeaderTimestamp, timestamp)
		req.Header.Set(apiclient.HeaderNonce, nonce)
		canonical := apiclient.BuildCanonicalString(req, timestamp, nonce)
		signature := apiclient.ComputeHMAC(key, canonical)
		req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(signature))
	}
	newCreateRequest := func(name, nonce string) *http.Request {
		body, err := json.Marshal(CreateAgentRequest{
			ID:          name + "-id",
			Name:        name,
			ProjectID:   projectID,
			HubEndpoint: hubEndpoint,
			NoAuth:      true,
			Config:      &CreateAgentConfig{Template: "claude"},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		// Control-channel dispatch injects this connection selector before the
		// request enters the same Server.Handler path exercised here.
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		sign(req, nonce)
		return req
	}

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- srv.checkAndReloadCredentials(context.Background())
	}()
	waitFor(hubLocked, "credential reload to hold hubMu")

	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.Handler().ServeHTTP(firstResponse, newCreateRequest("already-admitted", "before-removal"))
	}()

	// The request must hold the connection's read lease while waiting for
	// hubMu. This is the lock ordering that deadlocked when removal called Stop
	// while still holding hubMu.
	leaseDeadline := time.NewTimer(2 * time.Second)
	leasePoll := time.NewTicker(time.Millisecond)
	for conn.mu.TryLock() {
		conn.mu.Unlock()
		select {
		case <-leaseDeadline.C:
			t.Fatal("signed CREATE did not acquire the Hub authority lease")
		case <-leasePoll.C:
		}
	}
	leaseDeadline.Stop()
	leasePoll.Stop()

	close(allowHubUnlock)
	waitFor(authorityRevoked, "removed authority to be revoked")

	// A new request signed by the removed key must fail before Manager.Start,
	// even while removal is waiting for the already-admitted request's lease.
	secondResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(secondResponse, newCreateRequest("after-revocation", "after-removal"))
	if secondResponse.Code != http.StatusUnauthorized {
		t.Fatalf("post-revocation CREATE status %d, want %d: %s", secondResponse.Code, http.StatusUnauthorized, secondResponse.Body.String())
	}
	waitFor(managerEntered, "already-admitted CREATE to reach manager")
	if manager.starts != 1 {
		t.Fatalf("manager starts %d, want only the already-admitted request", manager.starts)
	}
	select {
	case err := <-reloadDone:
		t.Fatalf("credential removal returned before the admitted authority lease: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseManager)
	waitFor(firstDone, "already-admitted CREATE to complete")
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("already-admitted CREATE status %d, want %d: %s", firstResponse.Code, http.StatusCreated, firstResponse.Body.String())
	}
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("credential removal failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credential removal deadlocked after the authority lease was released")
	}

	if conn.authorityReady.Load() {
		t.Fatal("removed connection authority remained ready")
	}
	if conn.authorityGeneration.Load() == 0 {
		t.Fatal("removed connection authority generation was not invalidated")
	}
	srv.hubMu.RLock()
	_, retained := srv.hubConnections[connectionName]
	srv.hubMu.RUnlock()
	if retained {
		t.Fatal("removed connection remained published")
	}
}

func TestCredentialWatcher_RemovalWinsQueuedReinitialize(t *testing.T) {
	const (
		connectionName = "hub-remove-wins"
		brokerID       = "registered-broker"
		projectID      = "remove-wins-project"
	)
	oldKey := []byte("remove-wins-old-secret")
	newKey := []byte("remove-wins-new-secret")

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hub.Close()

	srv := newTestServer(t)
	conn := configureAuthenticatedHubFixture(t, srv, connectionName, hub.URL, brokerID, oldKey)
	oldCredentials := &brokercredentials.BrokerCredentials{
		Name:        connectionName,
		BrokerID:    brokerID,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(oldKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	conn.mu.Lock()
	conn.Credentials = oldCredentials
	conn.AuthMode = oldCredentials.AuthMode
	conn.mu.Unlock()
	srv.config.HeartbeatEnabled = true
	srv.config.HeartbeatInterval = time.Hour
	srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
	manager := srv.manager.(*mockManager)

	newCredentials := &brokercredentials.BrokerCredentials{
		Name:        connectionName,
		BrokerID:    brokerID,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(newKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}

	removalLocked := make(chan struct{})
	reinitializeWaiting := make(chan struct{})
	allowRemoval := make(chan struct{})
	defer func() {
		select {
		case <-allowRemoval:
		default:
			close(allowRemoval)
		}
	}()
	srv.hubConnectionLifecycleHook = func(stage string) {
		switch stage {
		case "removal-locked":
			close(removalLocked)
			<-allowRemoval
		case "reinitialize-waiting":
			close(reinitializeWaiting)
		}
	}

	waitFor := func(ch <-chan struct{}, event string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", event)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- srv.checkAndReloadCredentials(ctx)
	}()
	waitFor(removalLocked, "credential removal to acquire lifecycle serialization")

	reinitializeDone := make(chan error, 1)
	go func() {
		reinitializeDone <- conn.Reinitialize(ctx, srv, newCredentials)
	}()
	waitFor(reinitializeWaiting, "Reinitialize to queue behind credential removal")

	// Removal owns lifecycle serialization before detaching the exact pointer.
	// The queued Reinitialize must observe the completed removal and refuse to
	// publish replacement capabilities or restart outbound services.
	close(allowRemoval)
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("credential removal failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credential removal did not finish while owning lifecycle serialization")
	}
	select {
	case err := <-reinitializeDone:
		if err == nil {
			t.Fatal("queued Reinitialize succeeded after exact connection removal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued Reinitialize did not return after connection removal")
	}

	if conn.authorityReady.Load() {
		t.Fatal("removed connection authority was revived")
	}
	if generation := conn.authorityGeneration.Load(); generation != 1 {
		t.Fatalf("removed authority generation %d, want only the removal invalidation", generation)
	}
	conn.mu.RLock()
	heartbeat := conn.Heartbeat
	controlChannel := conn.ControlChannel
	gotSecret := append([]byte(nil), conn.SecretKey...)
	conn.mu.RUnlock()
	if heartbeat != nil || controlChannel != nil {
		t.Fatalf("removed connection retained services: heartbeat=%v controlChannel=%v", heartbeat != nil, controlChannel != nil)
	}
	if !bytes.Equal(gotSecret, oldKey) {
		t.Fatal("queued Reinitialize published replacement credentials after removal")
	}

	body, err := json.Marshal(CreateAgentRequest{
		ID:          "after-remove-wins-id",
		Name:        "after-remove-wins",
		ProjectID:   projectID,
		HubEndpoint: hub.URL,
		NoAuth:      true,
		Config:      &CreateAgentConfig{Template: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Scion-Hub-Connection", connectionName)
	signRequest(request, brokerID, oldKey)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("removed authority CREATE status %d, want %d: %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
	if manager.startCalls != 0 {
		t.Fatalf("removed authority reached manager %d times", manager.startCalls)
	}
}

func TestCredentialWatcher_ActiveReinitializeCompletesBeforeRemovalDetaches(t *testing.T) {
	const (
		connectionName = "hub-active-reinitialize"
		brokerID       = "registered-broker"
		projectID      = "active-reinitialize-project"
	)
	oldKey := []byte("active-reinitialize-old-secret")
	newKey := []byte("active-reinitialize-new-secret")

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hub.Close()

	srv := newTestServer(t)
	conn := configureAuthenticatedHubFixture(t, srv, connectionName, hub.URL, brokerID, oldKey)
	oldCredentials := &brokercredentials.BrokerCredentials{
		Name:        connectionName,
		BrokerID:    brokerID,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(oldKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	conn.mu.Lock()
	conn.Credentials = oldCredentials
	conn.AuthMode = oldCredentials.AuthMode
	conn.mu.Unlock()
	srv.config.HeartbeatEnabled = true
	srv.config.HeartbeatInterval = time.Hour
	srv.config.ControlChannelEnabled = true
	srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
	manager := srv.manager.(*mockManager)

	newCredentials := &brokercredentials.BrokerCredentials{
		Name:        connectionName,
		BrokerID:    brokerID,
		HubEndpoint: hub.URL,
		SecretKey:   base64.StdEncoding.EncodeToString(newKey),
		AuthMode:    brokercredentials.AuthModeHMAC,
	}

	reinitializeActive := make(chan struct{})
	allowReinitialize := make(chan struct{})
	removalWaiting := make(chan struct{})
	connectionDetached := make(chan struct{})
	allowRemoval := make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{allowReinitialize, allowRemoval} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	srv.hubConnectionLifecycleHook = func(stage string) {
		switch stage {
		case "reinitialize-active":
			close(reinitializeActive)
			<-allowReinitialize
		case "removal-waiting":
			close(removalWaiting)
		case "connections-detached":
			close(connectionDetached)
			<-allowRemoval
		}
	}

	waitFor := func(ch <-chan struct{}, event string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", event)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reinitializeDone := make(chan error, 1)
	go func() {
		reinitializeDone <- conn.Reinitialize(ctx, srv, newCredentials)
	}()
	waitFor(reinitializeActive, "Reinitialize to own lifecycle serialization")

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- srv.checkAndReloadCredentials(ctx)
	}()
	waitFor(removalWaiting, "credential removal to reach lifecycle serialization")

	// Removal has snapshotted the candidate but cannot detach it while the
	// active Reinitialize owns lifecycle serialization.
	srv.hubMu.RLock()
	published := srv.hubConnections[connectionName]
	srv.hubMu.RUnlock()
	if published != conn {
		t.Fatal("credential removal detached an active Reinitialize candidate before lifecycle serialization")
	}
	select {
	case <-connectionDetached:
		t.Fatal("connection detached while Reinitialize still owned lifecycle serialization")
	default:
	}

	close(allowReinitialize)
	waitFor(connectionDetached, "credential removal to detach after Reinitialize")
	select {
	case err := <-reinitializeDone:
		if err != nil {
			t.Fatalf("active Reinitialize failed before serialized removal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credential removal detached before active Reinitialize returned")
	}

	// At detachment, all Reinitialize publication and service-start work has
	// completed and removal has atomically revoked the published authority. No
	// writer can run again while removal owns reinitializeMu.
	if conn.authorityReady.Load() {
		t.Fatal("connection authority remained ready after serialized detachment")
	}
	if generation := conn.authorityGeneration.Load(); generation != 3 {
		t.Fatalf("authority generation at detachment %d, want Reinitialize publication plus removal invalidation", generation)
	}
	conn.mu.RLock()
	detachedSecret := append([]byte(nil), conn.SecretKey...)
	heartbeatStarted := conn.Heartbeat != nil
	controlChannelStarted := conn.ControlChannel != nil
	conn.mu.RUnlock()
	if !bytes.Equal(detachedSecret, newKey) {
		t.Fatal("Reinitialize replacement was not published before serialized detachment")
	}
	if !heartbeatStarted || !controlChannelStarted {
		t.Fatalf("Reinitialize services did not start before detachment: heartbeat=%v controlChannel=%v", heartbeatStarted, controlChannelStarted)
	}

	close(allowRemoval)
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("credential removal failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credential removal did not complete after serialized detachment")
	}

	if conn.authorityReady.Load() {
		t.Fatal("removed connection authority was restored after detachment")
	}
	if generation := conn.authorityGeneration.Load(); generation != 3 {
		t.Fatalf("final authority generation %d, want Reinitialize publication plus one removal invalidation", generation)
	}
	conn.mu.RLock()
	finalSecret := append([]byte(nil), conn.SecretKey...)
	heartbeat := conn.Heartbeat
	controlChannel := conn.ControlChannel
	conn.mu.RUnlock()
	if !bytes.Equal(finalSecret, detachedSecret) {
		t.Fatal("connection capabilities changed after detachment")
	}
	if heartbeat != nil || controlChannel != nil {
		t.Fatalf("removed connection revived services: heartbeat=%v controlChannel=%v", heartbeat != nil, controlChannel != nil)
	}
	srv.hubMu.RLock()
	_, retained := srv.hubConnections[connectionName]
	srv.hubMu.RUnlock()
	if retained {
		t.Fatal("serialized removal left the connection published")
	}

	body, err := json.Marshal(CreateAgentRequest{
		ID:          "after-active-remove-id",
		Name:        "after-active-remove",
		ProjectID:   projectID,
		HubEndpoint: hub.URL,
		NoAuth:      true,
		Config:      &CreateAgentConfig{Template: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Scion-Hub-Connection", connectionName)
	signRequest(request, brokerID, newKey)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("removed replacement authority CREATE status %d, want %d: %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
	if manager.startCalls != 0 {
		t.Fatalf("removed replacement authority reached manager %d times", manager.startCalls)
	}
}

func TestBuildAuthMiddleware_NoKeys(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerAuthEnabled = true
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	// With no connections and no keys, middleware should be nil
	if srv.brokerAuthMiddleware != nil {
		t.Error("expected nil middleware when no keys available")
	}
}

func TestBuildAuthMiddleware_WithKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds
	cfg.BrokerAuthEnabled = true

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	if srv.brokerAuthMiddleware == nil {
		t.Error("expected middleware to be created when keys available")
	}
}

func TestDefaultServerConfig_SecureBrokerAuthDefaults(t *testing.T) {
	cfg := DefaultServerConfig()
	if !cfg.BrokerAuthEnabled {
		t.Error("expected BrokerAuthEnabled to default to true")
	}
	if !cfg.BrokerAuthStrictMode {
		t.Error("expected BrokerAuthStrictMode to default to true")
	}
}

func TestRuntimeBroker_DefaultAuth_DeniesUnauthenticatedRequests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d for unauthenticated request, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestValidateBrokerAuthStartup_HubModeWithoutKeysLoopbackAllowed(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Host = "127.0.0.1"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.InMemoryCredentials = nil

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err != nil {
		t.Fatalf("loopback hub mode without keys should be allowed (pending registration), got: %v", err)
	}
}

func TestValidateBrokerAuthStartup_HubModeWithoutKeysNonLoopbackFails(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Host = "0.0.0.0"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.InMemoryCredentials = nil

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err == nil {
		t.Fatal("non-loopback hub mode without keys should fail")
	}
}

func TestValidateBrokerAuthStartup_NonLoopbackPermissiveModeFails(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")

	cfg := DefaultServerConfig()
	cfg.Host = "0.0.0.0"
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = creds
	cfg.BrokerAuthStrictMode = false

	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	if err := srv.validateBrokerAuthStartup(); err == nil {
		t.Fatal("expected startup validation to fail for non-loopback host without strict auth")
	}
}

func TestGetFirstHeartbeat_NoConnections(t *testing.T) {
	srv := newTestServer(t)

	hb := srv.getFirstHeartbeat()
	if hb != nil {
		t.Error("expected nil heartbeat when no connections")
	}
}

func TestHubConnection_Stop(t *testing.T) {
	conn := &HubConnection{
		Name:   "test",
		Status: ConnectionStatusConnected,
	}

	conn.Stop()

	if conn.GetStatus() != ConnectionStatusDisconnected {
		t.Errorf("expected disconnected after Stop, got %v", conn.GetStatus())
	}
}

// ============================================================================
// Phase 3: Co-located + Remote Combo Tests
// ============================================================================

func TestColocated_LocalConnection_GetsHeartbeat(t *testing.T) {
	// Co-located connections still need the broker heartbeat path so the hub
	// receives lifecycle updates from the embedded runtime broker.
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	if !ok {
		t.Fatal("expected 'local' connection to exist")
	}

	if !conn.IsColocated {
		t.Error("expected 'local' connection to be marked as co-located")
	}

	// Start the connection and verify the heartbeat service is created.
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false // disable control channel for this test
	srv.config = cfg

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx, srv); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if conn.Heartbeat == nil {
		t.Error("expected co-located connection to have heartbeat service")
	}
}

func TestColocated_RemoteConnection_GetsHeartbeat(t *testing.T) {
	// A non-co-located (remote) connection should get a heartbeat service
	// when HeartbeatEnabled is true.
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	conn, err := srv.createHubConnection("hub-prod", remoteCreds)
	if err != nil {
		t.Fatal(err)
	}
	// Explicitly NOT setting IsColocated — default is false

	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn
	srv.hubMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx, srv); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if conn.Heartbeat == nil {
		t.Error("expected remote connection to have heartbeat service")
	}
}

func TestColocated_ComboMode_HeartbeatPerConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// In combo mode (co-located local + remote), verify that both connections
	// get heartbeat services.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write a remote credential file
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Add remote connection from multi-store
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	// Start all connections
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.hubMu.RLock()
	for _, conn := range srv.hubConnections {
		if err := conn.Start(ctx, srv); err != nil {
			t.Fatalf("Start failed for %s: %v", conn.Name, err)
		}
	}
	srv.hubMu.RUnlock()

	// Verify: both local and remote connections have heartbeat services.
	srv.hubMu.RLock()
	localConn := srv.hubConnections["local"]
	remoteConn := srv.hubConnections["hub-prod"]
	srv.hubMu.RUnlock()

	if localConn == nil || remoteConn == nil {
		t.Fatal("expected both 'local' and 'hub-prod' connections to exist")
	}

	if !localConn.IsColocated {
		t.Error("expected 'local' to be co-located")
	}

	if localConn.Heartbeat == nil {
		t.Error("expected co-located 'local' connection to have heartbeat")
	}

	if remoteConn.IsColocated {
		t.Error("expected 'hub-prod' to NOT be co-located")
	}

	if remoteConn.Heartbeat == nil {
		t.Error("expected remote 'hub-prod' connection to have heartbeat")
	}
}

func TestColocated_CredentialWatcher_PreservesLocalConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// When credentials are reloaded, the "local" connection from InMemoryCredentials
	// must be preserved even if it's not in the multi-store.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write one remote credential
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-prod.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Manually set up multi-store and add remote
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Remove the remote credential file
	if err := os.Remove(filepath.Join(credDir, "hub-prod.json")); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	// Verify: local preserved, remote removed
	srv.hubMu.RLock()
	localConn, localExists := srv.hubConnections["local"]
	_, remoteExists := srv.hubConnections["hub-prod"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if !localExists {
		t.Error("expected 'local' connection to be preserved after reload")
	}

	if remoteExists {
		t.Error("expected 'hub-prod' connection to be removed after credential deletion")
	}

	if count != 1 {
		t.Errorf("expected 1 connection after reload, got %d", count)
	}

	if localConn != nil && !localConn.IsColocated {
		t.Error("expected preserved 'local' connection to still be co-located")
	}
}

func TestColocated_CredentialWatcher_AddRemoteAlongsideLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Start with only the local co-located connection, then add a remote one
	// via credential watcher.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create server with in-memory local creds only
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	srv.hubMu.RLock()
	if len(srv.hubConnections) != 1 {
		t.Fatalf("expected 1 initial connection (local), got %d", len(srv.hubConnections))
	}
	srv.hubMu.RUnlock()

	// Add a remote credential file
	remoteCreds := makeTestCreds("hub-staging", "broker-1", "https://hub.staging.example.com")
	remoteData, _ := json.MarshalIndent(remoteCreds, "", "  ")
	if err := os.WriteFile(filepath.Join(credDir, "hub-staging.json"), remoteData, 0600); err != nil {
		t.Fatal(err)
	}

	// Trigger credential reload
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatalf("checkAndReloadCredentials failed: %v", err)
	}

	// Verify: local preserved, remote added
	srv.hubMu.RLock()
	_, localExists := srv.hubConnections["local"]
	remoteConn, remoteExists := srv.hubConnections["hub-staging"]
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if !localExists {
		t.Error("expected 'local' connection to be preserved")
	}

	if !remoteExists {
		t.Error("expected 'hub-staging' connection to be added")
	}

	if count != 2 {
		t.Errorf("expected 2 connections, got %d", count)
	}

	// Verify the newly added connection is NOT co-located
	if remoteConn != nil && remoteConn.IsColocated {
		t.Error("expected 'hub-staging' to NOT be co-located")
	}
}

func TestColocated_GlobalProjectRejection_ComboMode(t *testing.T) {
	// In combo mode with local + remote, multi-hub mode should reject global project.
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(localCreds)

	// Add a remote connection to trigger multi-hub mode
	remoteCreds := makeTestCreds("hub-prod", "broker-1", "https://hub.prod.example.com")
	conn, err := srv.createHubConnection("hub-prod", remoteCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn
	srv.hubMu.Unlock()

	if !srv.isMultiHubMode() {
		t.Fatal("expected multi-hub mode with local + remote")
	}

	// Try to create a global project agent
	body := `{"name": "global-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status %d for global project in combo multi-hub mode, got %d: %s",
			http.StatusConflict, w.Code, w.Body.String())
	}
}

func TestColocated_MultipleRemoteConnections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Test co-located mode with multiple remote connections alongside the local.
	tmpDir := t.TempDir()
	credDir := filepath.Join(tmpDir, "hub-credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write two remote credential files
	for _, name := range []string{"hub-prod", "hub-staging"} {
		creds := makeTestCreds(name, "broker-1", "https://"+name+".example.com")
		data, _ := json.MarshalIndent(creds, "", "  ")
		if err := os.WriteFile(filepath.Join(credDir, name+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Create server with local + multi-store
	localCreds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://localhost:8080"
	cfg.InMemoryCredentials = localCreds

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	multiCreds, _ := srv.multiCredStore.List()
	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := srv.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := srv.createHubConnection(c.Name, c)
		if err != nil {
			t.Fatalf("Failed to create connection %q: %v", c.Name, err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[c.Name] = conn
		srv.hubMu.Unlock()
	}

	srv.hubMu.RLock()
	count := len(srv.hubConnections)
	srv.hubMu.RUnlock()

	if count != 3 {
		t.Fatalf("expected 3 hub connections (local + 2 remote), got %d", count)
	}

	// Verify co-located flags
	srv.hubMu.RLock()
	for name, conn := range srv.hubConnections {
		if name == "local" {
			if !conn.IsColocated {
				t.Errorf("expected 'local' to be co-located")
			}
		} else {
			if conn.IsColocated {
				t.Errorf("expected %q to NOT be co-located", name)
			}
		}
	}
	srv.hubMu.RUnlock()

	// Must be in multi-hub mode
	if !srv.isMultiHubMode() {
		t.Error("expected multi-hub mode with 3 connections")
	}
}

func TestLogHubConnections_NoConnections(t *testing.T) {
	// Verify logHubConnections doesn't panic with no connections
	srv := newTestServer(t)
	srv.logHubConnections() // should not panic
}

func TestLogHubConnections_WithConnections(t *testing.T) {
	// Verify logHubConnections doesn't panic with connections
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	srv.logHubConnections() // should not panic
}

func TestControlChannel_ConnectionNameHeader(t *testing.T) {
	// Verify that NewControlChannelClient stores the connectionName
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}

	cc := NewControlChannelClient(config, nil, nil, "hub-prod", slog.Default())

	if cc.connectionName != "hub-prod" {
		t.Errorf("expected connectionName 'hub-prod', got %q", cc.connectionName)
	}
}

func TestControlChannel_EmptyConnectionName(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}

	cc := NewControlChannelClient(config, nil, nil, "", slog.Default())

	if cc.connectionName != "" {
		t.Errorf("expected empty connectionName, got %q", cc.connectionName)
	}
}

// ============================================================================
// Phase 4: Hub Connections API Endpoint Tests
// ============================================================================

func TestHandleHubConnections_SingleConnection(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "single-hub" {
		t.Errorf("expected mode 'single-hub', got %q", resp.Mode)
	}

	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}

	conn := resp.Connections[0]
	if conn.Name != "local" {
		t.Errorf("expected connection name 'local', got %q", conn.Name)
	}
	if conn.HubEndpoint != "http://localhost:8080" {
		t.Errorf("expected endpoint 'http://localhost:8080', got %q", conn.HubEndpoint)
	}
	if conn.BrokerID != "broker-1" {
		t.Errorf("expected brokerId 'broker-1', got %q", conn.BrokerID)
	}
}

func TestHandleHubConnections_MultipleConnections(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a second connection
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "multi-hub" {
		t.Errorf("expected mode 'multi-hub', got %q", resp.Mode)
	}

	if len(resp.Connections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(resp.Connections))
	}

	// Build lookup
	connMap := make(map[string]HubConnectionInfo)
	for _, c := range resp.Connections {
		connMap[c.Name] = c
	}

	if _, ok := connMap["local"]; !ok {
		t.Error("expected 'local' connection in response")
	}
	if _, ok := connMap["hub-prod"]; !ok {
		t.Error("expected 'hub-prod' connection in response")
	}
}

func TestHandleHubConnections_ColocatedFlag(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Add a remote connection
	creds2 := makeTestCreds("hub-prod", "broker-2", "https://hub.prod.example.com")
	conn2, err := srv.createHubConnection("hub-prod", creds2)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-prod"] = conn2
	srv.hubMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	connMap := make(map[string]HubConnectionInfo)
	for _, c := range resp.Connections {
		connMap[c.Name] = c
	}

	localConn, ok := connMap["local"]
	if !ok {
		t.Fatal("expected 'local' connection")
	}
	if !localConn.IsColocated {
		t.Error("expected 'local' to be marked as co-located")
	}

	prodConn, ok := connMap["hub-prod"]
	if !ok {
		t.Fatal("expected 'hub-prod' connection")
	}
	if prodConn.IsColocated {
		t.Error("expected 'hub-prod' to NOT be marked as co-located")
	}
}

func TestHandleHubConnections_NoConnections(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "single-hub" {
		t.Errorf("expected mode 'single-hub' for 0 connections, got %q", resp.Mode)
	}

	if len(resp.Connections) != 0 {
		t.Errorf("expected 0 connections, got %d", len(resp.Connections))
	}
}

func TestHandleHubConnections_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestHandleHubConnections_ConnectionStatus(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	// Verify the status field is populated
	srv.hubMu.RLock()
	conn := srv.hubConnections["local"]
	srv.hubMu.RUnlock()

	// Set a known status
	conn.setStatus(ConnectionStatusConnected)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp HubConnectionStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}

	if resp.Connections[0].Status != "connected" {
		t.Errorf("expected status 'connected', got %q", resp.Connections[0].Status)
	}
}

// stubTemplateService stubs hubclient.TemplateService, returning canned
// metadata from Get. Unused methods are promoted from the embedded nil
// interface and panic if called.
type stubTemplateService struct {
	hubclient.TemplateService
	getFunc func(ctx context.Context, ref string) (*hubclient.Template, error)
}

func (s *stubTemplateService) Get(ctx context.Context, ref string) (*hubclient.Template, error) {
	return s.getFunc(ctx, ref)
}

// stubHubClient stubs hubclient.Client, exposing only Templates().
type stubHubClient struct {
	hubclient.Client
	templates hubclient.TemplateService
}

func (c *stubHubClient) Templates() hubclient.TemplateService { return c.templates }

// newLocalStorageWithTemplate builds a local storage backend and, if writeFiles
// is true, materializes the given global template's files on disk. It returns
// the backend and the expected on-disk directory for the template.
func newLocalStorageWithTemplate(t *testing.T, slug string, writeFiles bool) (storage.Storage, string) {
	t.Helper()
	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		LocalPath: t.TempDir(),
		Bucket:    "local",
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	objectPath := storage.TemplateStoragePath("", "global", "", slug)
	dir := stor.ObjectFSPath(objectPath)
	if writeFiles {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return stor, dir
}

func TestHydrateTemplate_LocalStorageDirectRead(t *testing.T) {
	stor, wantDir := newLocalStorageWithTemplate(t, "my-template", true)

	srv := newTestServer(t)

	// Co-located connection backed by a local storage backend resolves the
	// template directly from disk via metadata-derived storage path.
	conn := &HubConnection{
		Name:         "local",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				return &hubclient.Template{ID: "some-uuid", Slug: "my-template", Scope: "global"}, nil
			},
		}},
	}

	cfg := &CreateAgentConfig{
		Template:   "my-template",
		TemplateID: "some-uuid",
	}

	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != wantDir {
		t.Errorf("expected path %q, got %q", wantDir, path)
	}
}

func TestHydrateTemplate_LocalStorageFallsBackWhenMissing(t *testing.T) {
	// Backend has no files on disk for this template.
	stor, _ := newLocalStorageWithTemplate(t, "nonexistent-template", false)

	srv := newTestServer(t)

	conn := &HubConnection{
		Name:         "local",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				return &hubclient.Template{ID: "some-uuid", Slug: "nonexistent-template", Scope: "global"}, nil
			},
		}},
		// No Hydrator set — should return empty string after falling through.
	}

	cfg := &CreateAgentConfig{
		Template:   "nonexistent-template",
		TemplateID: "some-uuid",
	}

	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for missing local template, got %q", path)
	}
}

func TestHydrateTemplate_RemoteWithoutHydratorReturnsEmpty(t *testing.T) {
	srv := newTestServer(t)

	// Remote connection: no LocalStorage and no Hydrator — nothing to resolve.
	conn := &HubConnection{
		Name:        "remote",
		IsColocated: false,
	}

	cfg := &CreateAgentConfig{
		Template:   "my-template",
		TemplateID: "some-uuid",
	}

	path, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for remote connection without hydrator, got %q", path)
	}
}
