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

//go:build !no_sqlite

package runtimebroker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestBrokerHarnessConfigHydration_RealHTTP exercises the complete production
// hydration path between a real SCION Hub server (backed by real local storage
// and Ent database) and a Runtime Broker with a separate cache directory.
// It verifies:
// 1. Hub harness-config metadata lookup.
// 2. /download URL generation using the Hub's configured advertised URL (no file://).
// 3. Authenticated raw /files/... HTTP download including nested filenames with spaces and #.
// 4. Broker hash verification and cold-cache materialization.
// 5. Agent start using the hydrated HarnessConfigPath.
// 6. Warm restart cache reuse when the Hub server is unavailable.
// 7. Strict filesystem isolation: broker never receives or opens Hub storage paths.
func TestBrokerHarnessConfigHydration_RealHTTP(t *testing.T) {
	hubStorageDir := t.TempDir()
	brokerDir := t.TempDir()
	brokerTemplateCacheDir := filepath.Join(brokerDir, "cache", "templates")
	brokerCacheDir := filepath.Join(brokerDir, "cache", "harness-configs")
	brokerStateDir := filepath.Join(brokerDir, "state")
	brokerWorktreeBase := filepath.Join(brokerDir, "worktrees")

	const testDevAuthToken = "scion_dev_test_bearer_token_12345"
	hcID := uuid.New().String()
	const hcSlug = "claude-custom"

	// 1. Initialize real local storage for Hub
	locStor, err := storage.NewLocal(storage.Config{
		Bucket:    "test-bucket",
		LocalPath: hubStorageDir,
	})
	if err != nil {
		t.Fatalf("failed to create hub local storage: %v", err)
	}

	// 2. Initialize real Ent SQLite store for Hub
	entClient := enttest.NewClient(t)
	hubStore := entadapter.NewCompositeStore(entClient)

	// Materialize test bundle files into Hub local storage (including nested filename with space and #)
	bundleFiles := map[string]string{
		"config.yaml":                  "harness: claude\nversion: 1\n",
		"dialects/custom dialect#1.sh": "#!/bin/bash\necho 'custom dialect'\n",
		"home/.bashrc":                 "export FOO=bar\n",
	}

	storagePath := "harness-configs/global/" + hcSlug
	hcFiles := make([]store.TemplateFile, 0, len(bundleFiles))
	var transferFileInfos []transfer.FileInfo
	for relPath, content := range bundleFiles {
		objectPath := storagePath + "/" + relPath
		data := []byte(content)
		if _, err := locStor.Upload(context.Background(), objectPath, bytes.NewReader(data), storage.UploadOptions{}); err != nil {
			t.Fatalf("failed to upload bundle file %s to hub storage: %v", relPath, err)
		}
		h := transfer.HashBytes(data)
		hcFiles = append(hcFiles, store.TemplateFile{
			Path: relPath,
			Size: int64(len(data)),
			Hash: h,
		})
		transferFileInfos = append(transferFileInfos, transfer.FileInfo{
			Path: relPath,
			Hash: h,
		})
	}
	contentHash := transfer.ComputeContentHash(transferFileInfos)

	now := time.Now()
	hc := &store.HarnessConfig{
		ID:            hcID,
		Name:          "Claude Custom",
		Slug:          hcSlug,
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Visibility:    store.VisibilityPublic,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   storagePath,
		StorageBucket: "test-bucket",
		Files:         hcFiles,
		ContentHash:   contentHash,
		Created:       now,
		Updated:       now,
	}
	if err := hubStore.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("failed to create harness config in hub store: %v", err)
	}

	// 3. Configure and start real Hub Server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	advertisedHubURL := "http://" + listener.Addr().String()

	hubCfg := hub.DefaultServerConfig()
	hubCfg.DevAuthToken = testDevAuthToken
	hubCfg.HubEndpoint = advertisedHubURL
	hubCfg.DevUserConfig = hub.DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}

	hubSrv, err := hub.New(hubCfg, hubStore)
	if err != nil {
		t.Fatalf("failed to create real hub server: %v", err)
	}
	hubSrv.SetHubID("test-hub-id")
	hubSrv.SetStorage(locStor)
	t.Cleanup(func() { _ = hubSrv.Shutdown(context.Background()) })

	ts := httptest.NewUnstartedServer(hubSrv.Handler())
	ts.Listener = listener
	ts.Start()
	defer ts.Close()

	// 4. Broker setup with separate cache and client pointing to advertisedHubURL
	cache, err := templatecache.New(brokerCacheDir, 0)
	if err != nil {
		t.Fatalf("failed to create broker cache: %v", err)
	}

	client, err := hubclient.New(advertisedHubURL, hubclient.WithBearerToken(testDevAuthToken))
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}

	// Assert download URL generation produces HTTP URLs with advertisedHubURL and no file:// paths
	dlResp, err := client.HarnessConfigs().RequestDownloadURLs(context.Background(), hcID)
	if err != nil {
		t.Fatalf("failed to request download URLs from hub: %v", err)
	}
	if len(dlResp.Files) != len(bundleFiles) {
		t.Fatalf("expected %d download files, got %d", len(bundleFiles), len(dlResp.Files))
	}
	for _, f := range dlResp.Files {
		if strings.HasPrefix(f.URL, "file://") {
			t.Fatalf("broker received file:// URL from Hub: %s", f.URL)
		}
		if strings.Contains(f.URL, hubStorageDir) {
			t.Fatalf("broker received Hub storage path in URL: %s", f.URL)
		}
		if !strings.HasPrefix(f.URL, advertisedHubURL) {
			t.Fatalf("download URL does not start with advertised Hub URL %s: %s", advertisedHubURL, f.URL)
		}
	}

	creds := makeTestCreds("local", "broker-1", advertisedHubURL)
	cfg := DefaultServerConfig()
	cfg.BrokerID = creds.BrokerID
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = creds.HubEndpoint
	cfg.InMemoryCredentials = creds
	cfg.BrokerAuthEnabled = false
	cfg.ForceRuntime = "mock"
	cfg.TemplateCacheDir = brokerTemplateCacheDir
	cfg.StateDir = brokerStateDir
	cfg.WorktreeBase = brokerWorktreeBase

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	srv := New(cfg, mgr, rt)
	srv.hcCache = cache

	srv.hubMu.Lock()
	conn, ok := srv.hubConnections["local"]
	if !ok || conn == nil {
		srv.hubMu.Unlock()
		t.Fatalf("expected hub connection 'local' to be initialized, got nil")
	}
	conn.HubClient = client
	conn.HCResolver = templatecache.NewHarnessConfigResolver(cache, client)
	conn.LocalStorage = nil
	srv.hubMu.Unlock()

	// --- Step 1: Cold start hydration via real HTTP ---
	startBody := fmt.Sprintf(`{
		"harnessConfig": %q,
		"harnessConfigId": %q,
		"harnessConfigHash": %q,
		"task": "build feature"
	}`, hcSlug, hcID, contentHash)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status 202 Accepted on start, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
	}

	hydratedPath := mgr.lastStartOpts.HarnessConfigPath
	if hydratedPath == "" {
		t.Fatal("expected HarnessConfigPath to be set on start")
	}
	if !strings.HasPrefix(hydratedPath, brokerCacheDir) {
		t.Fatalf("expected HarnessConfigPath %q to be inside broker cache %q", hydratedPath, brokerCacheDir)
	}
	if strings.Contains(hydratedPath, hubStorageDir) {
		t.Fatalf("expected HarnessConfigPath %q to NOT be inside hub storage %q", hydratedPath, hubStorageDir)
	}

	// Verify all bundle files exist in the hydrated cache dir with exact content
	for relPath, wantContent := range bundleFiles {
		filePath := filepath.Join(hydratedPath, relPath)
		data, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("hydrated file %s not found in cache: %v", relPath, err)
		}
		if string(data) != wantContent {
			t.Errorf("content mismatch for %s: got %q, want %q", relPath, string(data), wantContent)
		}
	}

	// --- Step 2: Warm restart cache reuse (Hub is closed) ---
	ts.Close() // Proves network is inaccessible and warm cache hit skips download

	restartBody := fmt.Sprintf(`{
		"harnessConfig": %q,
		"harnessConfigId": %q,
		"harnessConfigHash": %q
	}`, hcSlug, hcID, contentHash)

	reqRestart := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", strings.NewReader(restartBody))
	reqRestart.Header.Set("Content-Type", "application/json")
	wRestart := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wRestart, reqRestart)

	if wRestart.Code != http.StatusAccepted {
		t.Fatalf("expected status 202 Accepted on restart, got %d: %s", wRestart.Code, wRestart.Body.String())
	}
	if mgr.startCalls != 2 {
		t.Fatalf("expected Start to be called twice after restart, got %d", mgr.startCalls)
	}
	if mgr.lastStartOpts.HarnessConfigPath != hydratedPath {
		t.Errorf("expected warm restart to reuse cached path %q, got %q", hydratedPath, mgr.lastStartOpts.HarnessConfigPath)
	}
}
