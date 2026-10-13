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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

func newAuthorityTestServer() *Server {
	return &Server{config: ServerConfig{BrokerAuthEnabled: true, BrokerAuthStrictMode: true}, hubConnections: make(map[string]*HubConnection)}
}

func configureAuthorityTestConnection(t *testing.T, srv *Server, name, endpoint, brokerID string, key []byte) *HubConnection {
	t.Helper()
	creds := &brokercredentials.BrokerCredentials{Name: name, HubEndpoint: endpoint, BrokerID: brokerID, SecretKey: base64.StdEncoding.EncodeToString(key), AuthMode: brokercredentials.AuthModeHMAC}
	conn, err := srv.createHubConnection(name, creds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubConnections[name] = conn
	srv.buildAuthMiddleware()
	return conn
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
	handler := srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, ok := authenticatedBrokerAuthority(r)
		if !ok {
			t.Error("authenticated request had no bound authority")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		observed <- *authority
		close(entered)
		<-release
		// Snapshotting during a pending rotation must not recursively take
		// the authority lease, or block behind its waiting writer.
		_ = authority.connection.snapshot()
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
	srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	srv := newAuthorityTestServer()
	connA := configureAuthorityTestConnection(t, srv, connectionA, hub.URL, brokerA, keyA)
	connB := configureAuthorityTestConnection(t, srv, connectionB, hub.URL, oldBrokerB, oldKeyB)
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
	if srv.authMiddlewareMu.TryLock() {
		srv.authMiddlewareMu.Unlock()
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
		srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestMultiKeyBrokerAuth_BindsSigningAuthority(t *testing.T) {
	key := []byte("authority-binding-secret")
	for _, tc := range []struct {
		name, brokerID, hubName string
		duplicate               bool
		want                    int
	}{
		{name: "valid", brokerID: "broker-a", hubName: "hub-a", want: http.StatusOK},
		{name: "no connection header", brokerID: "broker-a", want: http.StatusOK},
		{name: "tampered broker", brokerID: "broker-b", hubName: "hub-a", want: http.StatusUnauthorized},
		{name: "tampered connection", brokerID: "broker-a", hubName: "hub-b", want: http.StatusUnauthorized},
		{name: "duplicate key", brokerID: "broker-a", duplicate: true, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mw := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
			entries := []secretKeyEntry{{hubName: "hub-a", brokerID: "broker-a", hubEndpoint: "https://a.example", secretKey: key}}
			if tc.duplicate {
				entries = append(entries, secretKeyEntry{hubName: "hub-b", brokerID: "broker-b", secretKey: key})
			}
			mw.UpdateKeys(entries)
			called := false
			handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				authority, ok := authenticatedBrokerAuthority(r)
				if !ok || authority.hubName != "hub-a" || authority.brokerID != "broker-a" || authority.hubEndpoint != "https://a.example" {
					t.Errorf("incorrect authority: %+v", authority)
				}
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
			signRequest(req, "broker-a", key)
			// Changing these unsigned headers must not change the admitted identity.
			req.Header.Set(apiclient.HeaderBrokerID, tc.brokerID)
			req.Header.Set("X-Scion-Hub-Connection", tc.hubName)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want || called != (tc.want == http.StatusOK) {
				t.Fatalf("status=%d called=%v want=%d: %s", rr.Code, called, tc.want, rr.Body.String())
			}
		})
	}
}

func TestMultiKeyBrokerAuth_UpdateKeysOwnsSnapshot(t *testing.T) {
	key := []byte("immutable-authority-key")
	original := append([]byte(nil), key...)
	entries := []secretKeyEntry{{hubName: "hub-a", brokerID: "broker-a", secretKey: key}}
	mw := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	mw.UpdateKeys(entries)
	entries[0].brokerID = "tampered"
	key[0] ^= 255
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(req, "broker-a", original)
	rr := httptest.NewRecorder()
	mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("caller mutation changed the published snapshot: %d", rr.Code)
	}
}

func TestCredentialRemoval_RevokesBeforeDrainingAuthorityLease(t *testing.T) {
	srv := newAuthorityTestServer()
	key := []byte("remove-authority-secret")
	conn := configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
	srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
	oldMiddleware := srv.loadBrokerMiddleware()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	revoked := make(chan struct{})
	srv.hubConnectionLifecycleHook = func(stage string) {
		if stage == "authority-revoked" {
			close(revoked)
		}
	}
	handler := oldMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = conn.snapshot()
		// The handler may still need the map while its authority drains.
		_ = srv.resolveHubNameForLaunch(r)
		srv.hubMu.RLock()
		srv.hubMu.RUnlock()
		w.WriteHeader(http.StatusOK)
	}))
	requestDone := make(chan struct{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(req, "broker-a", key)
	go func() { handler.ServeHTTP(rr, req); close(requestDone) }()
	awaitAuthorityTestEvent(t, entered, "admitted request")
	removeDone := make(chan error, 1)
	go func() { removeDone <- srv.checkAndReloadCredentials(context.Background()) }()
	awaitAuthorityTestEvent(t, revoked, "authority revocation")
	if conn.authorityReady.Load() {
		t.Fatal("removed authority is ready")
	}
	select {
	case err := <-removeDone:
		t.Fatalf("removal did not drain the admitted request: %v", err)
	default:
	}
	// Even a captured, older middleware entry must reject after revocation.
	stale := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(stale, "broker-a", key)
	staleResponse := httptest.NewRecorder()
	oldMiddleware.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("removed authority reached handler") })).ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusUnauthorized {
		t.Fatalf("stale authority status=%d", staleResponse.Code)
	}
	close(release)
	awaitAuthorityTestEvent(t, requestDone, "admitted request completion")
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal deadlocked")
	}
	if rr.Code != http.StatusOK || !conn.isClosed() {
		t.Fatalf("response=%d closed=%v", rr.Code, conn.isClosed())
	}
	replacement := *conn.Credentials
	replacement.BrokerID = "broker-replacement"
	if err := conn.Reinitialize(context.Background(), srv, &replacement); err != nil {
		t.Fatal(err)
	}
	conn.requestReinitialize(&replacement)
	if applied, err := conn.applyRequestedReinitialize(context.Background(), srv); applied || err != nil {
		t.Fatalf("closed reinitialize applied=%v err=%v", applied, err)
	}
	if conn.authorityReady.Load() || conn.snapshot().BrokerID != "broker-a" {
		t.Fatal("removed connection was rebound")
	}
}

func awaitAuthorityTestEvent(t *testing.T, event <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestCredentialRemoval_SerializesActiveReinitialize(t *testing.T) {
	srv := newAuthorityTestServer()
	oldKey, newKey := []byte("old-active-key"), []byte("new-active-key")
	conn := configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", oldKey)
	srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
	replacement := *conn.Credentials
	replacement.SecretKey = base64.StdEncoding.EncodeToString(newKey)
	replacement.BrokerID = "broker-b"
	active, removeWaiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	srv.hubConnectionLifecycleHook = func(stage string) {
		switch stage {
		case "reinitialize-active":
			close(active)
			<-release
		case "removal-waiting":
			close(removeWaiting)
		}
	}
	reinitDone := make(chan error, 1)
	go func() { reinitDone <- conn.Reinitialize(context.Background(), srv, &replacement) }()
	awaitAuthorityTestEvent(t, active, "active reinitialize")
	removeDone := make(chan error, 1)
	go func() { removeDone <- srv.checkAndReloadCredentials(context.Background()) }()
	awaitAuthorityTestEvent(t, removeWaiting, "waiting removal")
	srv.hubMu.RLock()
	published := srv.hubConnections[conn.Name] == conn
	srv.hubMu.RUnlock()
	if !published {
		t.Fatal("removal detached while reinitialize held lifecycle ownership")
	}
	close(release)
	select {
	case err := <-reinitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reinitialize did not finish")
	}
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal did not finish")
	}
	if conn.authorityReady.Load() || !conn.isClosed() || conn.authorityGeneration.Load() != 3 {
		t.Fatalf("removed state ready=%v closed=%v generation=%d", conn.authorityReady.Load(), conn.isClosed(), conn.authorityGeneration.Load())
	}
	srv.buildAuthMiddleware()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(req, replacement.BrokerID, newKey)
	rr := httptest.NewRecorder()
	srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("removed replacement reached handler") })).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("removed replacement status=%d", rr.Code)
	}
}

func TestHubConnection_InvalidReplacementClearsCapabilities(t *testing.T) {
	srv := newAuthorityTestServer()
	key := []byte("valid-before-invalid-rotation")
	conn := configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
	replacement := *conn.Credentials
	replacement.SecretKey = "%%%invalid-base64"
	replacement.BrokerID = "broker-b"
	if err := conn.Reinitialize(context.Background(), srv, &replacement); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	conn.mu.RLock()
	cleared := conn.SecretKey == nil && conn.HubClient == nil && conn.Hydrator == nil && conn.HCResolver == nil && conn.TransportSource == nil && conn.TransportMode == 0
	conn.mu.RUnlock()
	if !cleared || conn.authorityReady.Load() || !srv.hasRegisteredHubAuthority() {
		t.Fatal("failed replacement retained executable capabilities or lost scoped authority")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(req, "broker-a", key)
	rr := httptest.NewRecorder()
	srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("old authority reached handler") })).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old authority status=%d", rr.Code)
	}
}

// A tracked control-channel request that already verified the old key can
// still be waiting to acquire its authority lease when rotation starts.
// ControlChannel.Close waits for this request, so it must not hold the lease
// writer while doing so.
func TestHubAuthority_TunneledAdmissionDrainsBeforePublication(t *testing.T) {
	for _, operation := range []string{"rotate", "remove"} {
		t.Run(operation, func(t *testing.T) {
			srv := newAuthorityTestServer()
			key := []byte("tunneled-old-authority")
			conn := configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
			mw := srv.loadBrokerMiddleware()
			selected, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			mw.beforeAuthorityAdmission = func() { close(selected); <-release }
			var calls atomic.Int32
			handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
			cc, _ := newCancelTestClient(t, handler, 1)
			cc.connectionName = conn.Name
			conn.ControlChannel = cc
			req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
			signRequest(req, conn.BrokerID, key)
			headers := make(map[string]string)
			for name := range req.Header {
				headers[name] = req.Header.Get(name)
			}
			feed(t, cc, wsprotocol.NewRequestEnvelope("authority-waiter", req.Method, req.URL.Path, "", headers, nil))
			awaitAuthorityTestEvent(t, selected, "verified tunneled request")
			transitionDone := make(chan error, 1)
			go func() {
				if operation == "rotate" {
					replacement := *conn.Credentials
					replacement.SecretKey = base64.StdEncoding.EncodeToString([]byte("tunneled-new-authority"))
					transitionDone <- conn.Reinitialize(context.Background(), srv, &replacement)
				} else {
					srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
					transitionDone <- srv.checkAndReloadCredentials(context.Background())
				}
			}()
			// Close cancels the transport before its c.wg wait. The paused request
			// must be able to enter and reject its now-stale authority after this.
			awaitAuthorityTestEvent(t, cc.ctx.Done(), "transport shutdown")
			close(release)
			select {
			case err := <-transitionDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("transport drain deadlocked on authority writer")
			}
			waitWG(t, cc)
			if calls.Load() != 0 {
				t.Fatal("stale tunneled request reached handler")
			}
		})
	}
}

func TestHubAuthority_RevocationCancelsHealthyDirectAttach(t *testing.T) {
	for _, operation := range []string{"rotate", "remove", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			useFastPTYTimings(t)
			var srv *Server
			var conn *HubConnection
			key := []byte("direct-attach-old-authority")
			currentKey := key
			dial, tmuxCmd := newDirectAttachFixtureConfigured(t, func(server *Server) {
				srv = server
				srv.config.HeartbeatEnabled = false
				srv.config.ControlChannelEnabled = false
				srv.config.BrokerAuthEnabled = true
				srv.config.BrokerAuthStrictMode = true
				conn = configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
			}, func(req *http.Request) { signRequest(req, "broker-a", currentKey) })
			ws, _, err := dial()
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			socketDone := make(chan struct{})
			go func() {
				defer close(socketDone)
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				out, err := tmuxCmd("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
				if err == nil && out == "80x24" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("signed terminal did not attach")
				}
				time.Sleep(20 * time.Millisecond)
			}
			transitionDone := make(chan error, 1)
			go func() {
				switch operation {
				case "rotate":
					replacement := *conn.Credentials
					replacement.SecretKey = base64.StdEncoding.EncodeToString([]byte("direct-attach-new-authority"))
					transitionDone <- conn.Reinitialize(context.Background(), srv, &replacement)
				case "remove":
					srv.multiCredStore = brokercredentials.NewMultiStore(t.TempDir())
					transitionDone <- srv.checkAndReloadCredentials(context.Background())
				case "shutdown":
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer cancel()
					transitionDone <- srv.Shutdown(ctx)
				}
			}()
			select {
			case err := <-transitionDone:
				if err != nil && !(operation == "shutdown" && errors.Is(err, context.DeadlineExceeded)) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("authority transition waited on healthy direct attach")
			}
			awaitAuthorityTestEvent(t, socketDone, "cancelled direct terminal")
			if operation == "rotate" {
				currentKey = []byte("direct-attach-new-authority")
				fresh, _, err := dial()
				if err != nil {
					t.Fatalf("replacement authority could not attach: %v", err)
				}
				fresh.Close()
			}
		})
	}
}

func TestHubAuthority_ShutdownDeadlineBoundsLeaseDrain(t *testing.T) {
	srv := newTestServer(t)
	srv.config.BrokerAuthEnabled = true
	srv.config.BrokerAuthStrictMode = true
	key := []byte("shutdown-lease-key")
	conn := configureAuthorityTestConnection(t, srv, "hub-a", "https://a.example", "broker-a", key)
	// Run an actual listener as well as the admitted lease below: a timed
	// out authority drain must still stop accepting fresh HTTP requests.
	live := httptest.NewServer(srv.Handler())
	defer live.Close()
	srv.mu.Lock()
	srv.httpServer = live.Config
	srv.mu.Unlock()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(live.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthy listener status=%d", response.StatusCode)
	}
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	handler := srv.loadBrokerMiddleware().Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		// Simulate cleanup that has yet to acknowledge cancellation. Shutdown
		// must respect its deadline without allowing this tuple to be rebound.
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	requestDone := make(chan struct{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	signRequest(req, "broker-a", key)
	go func() { handler.ServeHTTP(httptest.NewRecorder(), req); close(requestDone) }()
	awaitAuthorityTestEvent(t, entered, "active shutdown lease")
	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() { shutdownDone <- srv.Shutdown(ctx) }()
	awaitAuthorityTestEvent(t, cancelled, "shutdown lease cancellation")
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error=%v want deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown ignored its deadline")
	}
	if response, err := client.Get(live.URL + "/healthz"); err == nil {
		response.Body.Close()
		t.Fatal("HTTP listener still accepted a fresh request after the shutdown deadline")
	}
	if conn.authorityReady.Load() {
		t.Fatal("timed-out shutdown left authority ready")
	}
	close(release)
	awaitAuthorityTestEvent(t, requestDone, "lease cleanup")
	conn.lifecycleMu.Lock()
	closed := conn.isClosed()
	conn.lifecycleMu.Unlock()
	if !closed {
		t.Fatal("background final cleanup did not retain the terminal lifecycle state")
	}
}
