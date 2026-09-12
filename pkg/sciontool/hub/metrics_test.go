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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReportMetricsRejectsInvalidSessionBeforeRequest(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	now := time.Now().UTC()
	valid := MetricsPayload{
		Session: SessionMetrics{
			ID:        "session-1",
			StartedAt: now.Format(time.RFC3339),
			EndedAt:   now.Add(time.Minute).Format(time.RFC3339),
		},
	}
	tests := []struct {
		name    string
		mutate  func(*MetricsPayload)
		wantErr string
	}{
		{
			name: "missing session ID",
			mutate: func(payload *MetricsPayload) {
				payload.Session.ID = " "
			},
			wantErr: "session.id is required",
		},
		{
			name: "zero session start time",
			mutate: func(payload *MetricsPayload) {
				payload.Session.StartedAt = time.Time{}.UTC().Format(time.RFC3339)
			},
			wantErr: "session.started_at must be a non-zero RFC3339 timestamp",
		},
		{
			name: "malformed session start time",
			mutate: func(payload *MetricsPayload) {
				payload.Session.StartedAt = "not-a-time"
			},
			wantErr: "session.started_at must be a non-zero RFC3339 timestamp",
		},
		{
			name: "session end precedes start",
			mutate: func(payload *MetricsPayload) {
				payload.Session.EndedAt = now.Add(-time.Minute).Format(time.RFC3339)
			},
			wantErr: "session.ended_at cannot be before session.started_at",
		},
	}

	client := NewClientWithConfig(server.URL, "token", "agent-1")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := valid
			tc.mutate(&payload)
			err := client.ReportMetrics(context.Background(), payload)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ReportMetrics error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	if requestCount != 0 {
		t.Fatalf("invalid payloads made %d HTTP requests, want 0", requestCount)
	}
}

func TestReportMetricsAcceptsValidLifecycleSession(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	now := time.Now().UTC()
	payload := MetricsPayload{
		Session: SessionMetrics{
			ID:        "session-1",
			StartedAt: now.Format(time.RFC3339),
			EndedAt:   now.Add(time.Minute).Format(time.RFC3339),
		},
	}
	client := NewClientWithConfig(server.URL, "token", "agent-1")

	if err := client.ReportMetrics(context.Background(), payload); err != nil {
		t.Fatalf("ReportMetrics: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("valid payload made %d HTTP requests, want 1", requestCount)
	}
}
