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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func testHarnessConfigFileServer(t *testing.T) (*Server, store.Store, *contentMockStorage) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

func createTestHarnessConfigWithFiles(t *testing.T, s store.Store, stor *contentMockStorage, files map[string]string) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:            tid("hc-file-test-1"),
		Name:          "test-hc",
		Slug:          "test-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/test-hc",
		StorageBucket: "test-bucket",
		Updated:       time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	}

	hcFiles := make([]store.TemplateFile, 0, len(files))
	for path, content := range files {
		objectPath := hc.StoragePath + "/" + path
		stor.content[objectPath] = []byte(content)
		stor.objects[objectPath] = &storage.Object{
			Name: objectPath,
			Size: int64(len(content)),
		}
		hcFiles = append(hcFiles, store.TemplateFile{
			Path: path,
			Size: int64(len(content)),
			Hash: "sha256:placeholder",
		})
	}
	hc.Files = hcFiles
	hc.ContentHash = computeContentHash(hcFiles)

	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create test harness config: %v", err)
	}
	return hc
}

func TestHandleHarnessConfigFileWrite_UpdatesImage(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	ctx := context.Background()

	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	// Provide storage via the server (createTestHarnessConfigWithFiles used nil
	// storage because we want the PUT handler to write to the server's storage).
	stor := srv.GetStorage().(*contentMockStorage)

	// Pre-populate storage with the existing placeholder so the harness config
	// has a valid storage path.
	newImage := "us-docker.pkg.dev/my-project/repo/new-image:v2"
	configYAML := "harness: claude\nimage: " + newImage + "\n"

	body := `{"content": "` + strings.ReplaceAll(configYAML, "\n", `\n`) + `"}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/harness-configs/"+hc.ID+"/files/config.yaml",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HarnessConfigFileWriteResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Path != "config.yaml" {
		t.Errorf("expected path config.yaml, got %s", resp.Path)
	}

	// Verify storage content
	storedContent := stor.content[hc.StoragePath+"/config.yaml"]
	if string(storedContent) != configYAML {
		t.Errorf("unexpected stored content: %s", string(storedContent))
	}

	// Verify hc.Config.Image was updated
	updated, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get updated harness config: %v", err)
	}
	if updated.Config == nil {
		t.Fatal("expected Config to be non-nil after writing config.yaml")
	}
	if updated.Config.Image != newImage {
		t.Errorf("expected Config.Image = %q, got %q", newImage, updated.Config.Image)
	}
}

func TestHandleHarnessConfigFileRead_Raw_QueryParam(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	files := map[string]string{
		"config.yaml":        "harness: claude\nversion: 1\n",
		"dialects/script.sh": "#!/bin/bash\necho 'hello world'\n",
		"home/.bashrc":       "export FOO=bar\n",
	}
	hc := createTestHarnessConfigWithFiles(t, s, stor, files)

	// Test reading nested dialect script via ?raw=1
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/dialects/script.sh?raw=1", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("expected Content-Type application/octet-stream, got %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "attachment; filename=script.sh" && cd != `attachment; filename="script.sh"` {
		t.Errorf("expected Content-Disposition attachment for script.sh, got %q", cd)
	}
	if got := w.Body.String(); got != files["dialects/script.sh"] {
		t.Errorf("expected body %q, got %q", files["dialects/script.sh"], got)
	}
}

func TestHandleHarnessConfigFileRead_Raw_AcceptHeader(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	content := "harness: claude\nversion: 2\n"
	files := map[string]string{
		"config.yaml": content,
	}
	hc := createTestHarnessConfigWithFiles(t, s, stor, files)

	// Test raw download triggered by Accept: application/octet-stream header
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/config.yaml", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.Header.Set("Accept", "application/octet-stream")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("expected Content-Type application/octet-stream, got %q", ct)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("expected body %q, got %q", content, got)
	}
}

func TestHandleHarnessConfigFileRead_Raw_BypassesSizeLimit(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	// Create a large file (1.5 MB) exceeding maxInlineFileSize (1 MB)
	largeData := strings.Repeat("X", 1500*1024)
	files := map[string]string{
		"large.bin": largeData,
	}
	hc := createTestHarnessConfigWithFiles(t, s, stor, files)

	// 1. Raw download should SUCCEED and bypass the 1MB cap
	reqRaw := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/large.bin?raw=1", nil)
	reqRaw.Header.Set("Authorization", "Bearer "+testDevToken)
	wRaw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wRaw, reqRaw)

	if wRaw.Code != http.StatusOK {
		t.Fatalf("expected 200 for raw read of 1.5MB file, got %d: %s", wRaw.Code, wRaw.Body.String())
	}
	if wRaw.Body.Len() != len(largeData) {
		t.Errorf("expected %d bytes, got %d", len(largeData), wRaw.Body.Len())
	}

	// 2. Inline JSON view should FAIL with 413 Payload Too Large
	reqJSON := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/large.bin", nil)
	reqJSON.Header.Set("Authorization", "Bearer "+testDevToken)
	wJSON := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wJSON, reqJSON)

	if wJSON.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for inline JSON view of 1.5MB file, got %d: %s", wJSON.Code, wJSON.Body.String())
	}
}

func TestHandleHarnessConfigFileRead_NotFound(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	hc := createTestHarnessConfigWithFiles(t, s, stor, map[string]string{
		"config.yaml": "harness: claude\n",
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/nonexistent.sh?raw=1", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing file, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleHarnessConfigFileRead_Unauthorized(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	hc := createTestHarnessConfigWithFiles(t, s, stor, map[string]string{
		"config.yaml": "harness: claude\n",
	})

	// Missing authorization header
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/harness-configs/"+hc.ID+"/files/config.yaml?raw=1", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
}
