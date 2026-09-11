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

package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLegacyStoragePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "empty path returns empty",
			path: "",
			want: "",
		},
		{
			name: "non-hub path returns empty",
			path: "templates/global/my-template",
			want: "",
		},
		{
			name: "hub path strips prefix",
			path: "hubs/my-hub/templates/global/my-template",
			want: "templates/global/my-template",
		},
		{
			name: "hub path with project scope",
			path: "hubs/hub-1/templates/projects/g-1/t1",
			want: "templates/projects/g-1/t1",
		},
		{
			name: "hub path with harness-config",
			path: "hubs/hub-1/harness-configs/global/h1",
			want: "harness-configs/global/h1",
		},
		{
			name: "hubs/ prefix with no hub ID slash returns empty",
			path: "hubs/only-hub-id",
			want: "",
		},
		{
			name: "hubs/ prefix with hub ID and trailing slash",
			path: "hubs/my-hub/",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := legacyStoragePath(tt.path)
			if got != tt.want {
				t.Errorf("legacyStoragePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestEscapePathSegments(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"config.yaml", "config.yaml"},
		{"home/.bashrc", "home/.bashrc"},
		{"dialects/docker/Dockerfile", "dialects/docker/Dockerfile"},
		{"dir with spaces/file 1.txt", "dir%20with%20spaces/file%201.txt"},
		{"dialects/tool#1.yaml", "dialects/tool%231.yaml"},
		{"nested/path with #hash/and space/file?.txt", "nested/path%20with%20%23hash/and%20space/file%3F.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := escapePathSegments(tt.input)
			if got != tt.want {
				t.Errorf("escapePathSegments(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRewriteLocalDownloadURLs(t *testing.T) {
	urls := []DownloadURLInfo{
		{
			Path: "config.yaml",
			URL:  "file:///var/scion/storage/hc1/config.yaml",
			Hash: "sha256:111",
			Size: 100,
		},
		{
			Path: "home/nested config#1.yaml",
			URL:  "file:///var/scion/storage/hc1/home/nested config#1.yaml",
			Hash: "sha256:222",
			Size: 200,
		},
		{
			Path: "external.tar.gz",
			URL:  "https://storage.googleapis.com/my-bucket/external.tar.gz",
			Hash: "sha256:333",
			Size: 300,
		},
	}

	hubEndpoint := "https://hub.example.com:8443/"
	rewritten := rewriteLocalDownloadURLs(urls, hubEndpoint, "harness-configs", "hc_123")

	if len(rewritten) != 3 {
		t.Fatalf("expected 3 URLs, got %d", len(rewritten))
	}

	// First: file:// rewritten to proxy URL with ?raw=1
	want0 := "https://hub.example.com:8443/api/v1/harness-configs/hc_123/files/config.yaml?raw=1"
	if rewritten[0].URL != want0 {
		t.Errorf("rewritten[0].URL = %q, want %q", rewritten[0].URL, want0)
	}

	// Second: file:// with spaces and # correctly escaped
	want1 := "https://hub.example.com:8443/api/v1/harness-configs/hc_123/files/home/nested%20config%231.yaml?raw=1"
	if rewritten[1].URL != want1 {
		t.Errorf("rewritten[1].URL = %q, want %q", rewritten[1].URL, want1)
	}

	// Third: GCS URL unchanged
	want2 := "https://storage.googleapis.com/my-bucket/external.tar.gz"
	if rewritten[2].URL != want2 {
		t.Errorf("rewritten[2].URL = %q, want %q", rewritten[2].URL, want2)
	}

	// Empty hubEndpoint returns original
	unchanged := rewriteLocalDownloadURLs(urls, "", "harness-configs", "hc_123")
	if unchanged[0].URL != urls[0].URL {
		t.Errorf("expected unchanged URL when hubEndpoint is empty, got %q", unchanged[0].URL)
	}
}

func TestRewriteLocalUploadURLs(t *testing.T) {
	urls := []UploadURLInfo{
		{
			Path: "config.yaml",
			URL:  "file:///var/scion/storage/hc1/config.yaml",
		},
		{
			Path: "dialects/custom#dialect.yaml",
			URL:  "file:///var/scion/storage/hc1/dialects/custom#dialect.yaml",
		},
		{
			Path:   "external.txt",
			URL:    "https://storage.googleapis.com/upload/external.txt",
			Method: http.MethodPut,
		},
	}

	hubEndpoint := "http://hub.local:8080"
	rewritten := rewriteLocalUploadURLs(urls, hubEndpoint, "harness-configs", "hc_456")

	if len(rewritten) != 3 {
		t.Fatalf("expected 3 URLs, got %d", len(rewritten))
	}

	want0 := "http://hub.local:8080/api/v1/harness-configs/hc_456/files/config.yaml"
	if rewritten[0].URL != want0 {
		t.Errorf("rewritten[0].URL = %q, want %q", rewritten[0].URL, want0)
	}
	if rewritten[0].Method != http.MethodPut {
		t.Errorf("rewritten[0].Method = %q, want PUT", rewritten[0].Method)
	}
	if rewritten[0].Headers["Content-Type"] != "application/octet-stream" {
		t.Errorf("rewritten[0].Headers[Content-Type] = %q, want application/octet-stream", rewritten[0].Headers["Content-Type"])
	}

	want1 := "http://hub.local:8080/api/v1/harness-configs/hc_456/files/dialects/custom%23dialect.yaml"
	if rewritten[1].URL != want1 {
		t.Errorf("rewritten[1].URL = %q, want %q", rewritten[1].URL, want1)
	}

	// Cloud upload URL unchanged
	if rewritten[2].URL != urls[2].URL {
		t.Errorf("rewritten[2].URL = %q, want %q", rewritten[2].URL, urls[2].URL)
	}
}

func TestAdvertisedOrRequestURL(t *testing.T) {
	// Case 1: HubEndpoint configured on server takes priority
	srvWithEndpoint := &Server{
		config: ServerConfig{
			HubEndpoint: "https://hub.production.org:8443/",
		},
	}
	req := httptest.NewRequest(http.MethodGet, "http://10.0.0.5:9000/api/v1/test", nil)
	if got := srvWithEndpoint.advertisedOrRequestURL(req); got != "https://hub.production.org:8443" {
		t.Errorf("advertisedOrRequestURL() = %q, want %q", got, "https://hub.production.org:8443")
	}

	// Case 2: HubEndpoint not configured; falls back to requestBaseURL
	srvWithoutEndpoint := &Server{
		config: ServerConfig{},
	}
	req2 := httptest.NewRequest(http.MethodGet, "http://myhub.internal:9800/api/v1/test", nil)
	req2.Host = "myhub.internal:9800"
	if got := srvWithoutEndpoint.advertisedOrRequestURL(req2); got != "http://myhub.internal:9800" {
		t.Errorf("advertisedOrRequestURL() = %q, want %q", got, "http://myhub.internal:9800")
	}

	// Case 3: X-Forwarded-Proto respected in request fallback
	req3 := httptest.NewRequest(http.MethodGet, "http://myhub.internal:9800/api/v1/test", nil)
	req3.Host = "myhub.internal:9800"
	req3.Header.Set("X-Forwarded-Proto", "https")
	if got := srvWithoutEndpoint.advertisedOrRequestURL(req3); got != "https://myhub.internal:9800" {
		t.Errorf("advertisedOrRequestURL() = %q, want %q", got, "https://myhub.internal:9800")
	}
}
