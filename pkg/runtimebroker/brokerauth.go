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

// Package runtimebroker provides the Scion Runtime Broker API server.
package runtimebroker

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// authenticatingHubConnCtxKey is the context key MultiKeyBrokerAuthMiddleware
// uses to record which hub connection's key authenticated an incoming
// request (design t1-async-create-v11.md §3.8.5 routing rule 2). It is the
// authenticated source the broker uses to pick which hub connection owns
// a launch, so later reports
// for it are sent directly to that connection instead of fanning out.
type authenticatingHubConnCtxKey struct{}

// authenticatingHubConnFromContext returns the name of the hub connection
// whose secret key verified the request's HMAC signature, or "" when
// authentication is disabled, unauthenticated requests are allowed through,
// or no context value was set (e.g. a request built by a test without
// routing it through the middleware).
func authenticatingHubConnFromContext(ctx context.Context) string {
	name, _ := ctx.Value(authenticatingHubConnCtxKey{}).(string)
	return name
}

// BrokerAuthConfig configures host-side HMAC authentication.
type BrokerAuthConfig struct {
	// Enabled controls whether authentication is enforced.
	Enabled bool
	// MaxClockSkew is the maximum allowed time difference between client and server.
	MaxClockSkew time.Duration
	// SecretKey is the shared secret for HMAC verification.
	SecretKey []byte
	// AllowUnauthenticated allows requests without HMAC headers to pass through.
	// This is useful for development or when mixing authenticated and unauthenticated endpoints.
	AllowUnauthenticated bool
}

// DefaultBrokerAuthConfig returns the default broker authentication configuration.
func DefaultBrokerAuthConfig() BrokerAuthConfig {
	return BrokerAuthConfig{
		Enabled:              false,
		MaxClockSkew:         5 * time.Minute,
		AllowUnauthenticated: true,
	}
}

// BrokerAuthMiddleware provides HMAC-based authentication for incoming requests.
// This verifies that requests from the Hub are properly signed.
type BrokerAuthMiddleware struct {
	config BrokerAuthConfig
}

// NewBrokerAuthMiddleware creates a new broker authentication middleware.
func NewBrokerAuthMiddleware(cfg BrokerAuthConfig) *BrokerAuthMiddleware {
	return &BrokerAuthMiddleware{config: cfg}
}

// Middleware returns an HTTP middleware handler that validates HMAC signatures.
func (m *BrokerAuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.config.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		// Health endpoints are always public — no authentication required.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract HMAC headers
		brokerID := r.Header.Get(apiclient.HeaderBrokerID)
		timestamp := r.Header.Get(apiclient.HeaderTimestamp)
		nonce := r.Header.Get(apiclient.HeaderNonce)
		signature := r.Header.Get(apiclient.HeaderSignature)

		// If no HMAC headers present, check if unauthenticated requests are allowed
		if brokerID == "" && timestamp == "" && signature == "" {
			if m.config.AllowUnauthenticated {
				next.ServeHTTP(w, r)
				return
			}
			m.writeError(w, "missing authentication headers")
			return
		}

		// Validate required headers are all present
		if brokerID == "" {
			m.writeError(w, "missing X-Scion-Broker-ID header")
			return
		}
		if timestamp == "" {
			m.writeError(w, "missing X-Scion-Timestamp header")
			return
		}
		if signature == "" {
			m.writeError(w, "missing X-Scion-Signature header")
			return
		}

		// Validate timestamp
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			m.writeError(w, "invalid timestamp format")
			return
		}

		requestTime := time.Unix(ts, 0)
		clockSkew := time.Since(requestTime)
		if clockSkew < 0 {
			clockSkew = -clockSkew
		}
		if clockSkew > m.config.MaxClockSkew {
			m.writeError(w, fmt.Sprintf("timestamp outside acceptable range (skew: %v)", clockSkew))
			return
		}

		// Decode the signature
		sigBytes, err := base64.StdEncoding.DecodeString(signature)
		if err != nil {
			m.writeError(w, "invalid signature encoding")
			return
		}

		// Build canonical string and verify signature
		canonical := apiclient.BuildCanonicalString(r, timestamp, nonce)
		if !apiclient.VerifyHMAC(m.config.SecretKey, canonical, sigBytes) {
			m.writeError(w, "invalid signature")
			return
		}

		// Signature valid, continue to handler
		next.ServeHTTP(w, r)
	})
}

// writeError writes an authentication error response.
func (m *BrokerAuthMiddleware) writeError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"error":{"code":"broker_auth_failed","message":%q}}`, message)
}

// UpdateSecretKey updates the secret key used for verification.
// This can be used when credentials are rotated.
func (m *BrokerAuthMiddleware) UpdateSecretKey(key []byte) {
	m.config.SecretKey = key
}

// SetEnabled enables or disables authentication.
func (m *BrokerAuthMiddleware) SetEnabled(enabled bool) {
	m.config.Enabled = enabled
}

// secretKeyEntry associates a secret key with a hub connection name.
type secretKeyEntry struct {
	hubName     string
	brokerID    string
	hubEndpoint string
	generation  uint64
	secretKey   []byte
	connection  *HubConnection
}

type brokerRequestAuthority struct {
	hubName     string
	brokerID    string
	hubEndpoint string
	generation  uint64
	connection  *HubConnection
}

type brokerRequestAuthorityKey struct{}

func authenticatedBrokerAuthority(r *http.Request) (*brokerRequestAuthority, bool) {
	if r == nil {
		return nil, false
	}
	authority, ok := r.Context().Value(brokerRequestAuthorityKey{}).(*brokerRequestAuthority)
	return authority, ok && authority != nil
}

// MultiKeyBrokerAuthMiddleware provides HMAC-based authentication that supports
// verifying requests signed by any of multiple hub connections' secret keys.
type MultiKeyBrokerAuthMiddleware struct {
	mu                   sync.RWMutex
	keys                 []secretKeyEntry
	maxClockSkew         time.Duration
	allowUnauthenticated bool
	enabled              bool
}

// NewMultiKeyBrokerAuthMiddleware creates a new multi-key broker authentication middleware.
func NewMultiKeyBrokerAuthMiddleware(enabled bool, maxClockSkew time.Duration, allowUnauthenticated bool) *MultiKeyBrokerAuthMiddleware {
	return &MultiKeyBrokerAuthMiddleware{
		enabled:              enabled,
		maxClockSkew:         maxClockSkew,
		allowUnauthenticated: allowUnauthenticated,
	}
}

// UpdateKeys replaces the set of secret keys used for verification.
func (m *MultiKeyBrokerAuthMiddleware) UpdateKeys(keys []secretKeyEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = make([]secretKeyEntry, len(keys))
	for i := range keys {
		m.keys[i] = keys[i]
		m.keys[i].secretKey = append([]byte(nil), keys[i].secretKey...)
	}
}

// Middleware returns an HTTP middleware handler that validates HMAC signatures
// against any of the registered secret keys.
func (m *MultiKeyBrokerAuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.ServeWith(w, r, next)
	})
}

// ServeWith handles r as the handler returned by Middleware(next) does.
func (m *MultiKeyBrokerAuthMiddleware) ServeWith(w http.ResponseWriter, r *http.Request, next http.Handler) {
	m.mu.RLock()
	enabled := m.enabled
	allowUnauth := m.allowUnauthenticated
	keys := append([]secretKeyEntry(nil), m.keys...)
	maxSkew := m.maxClockSkew
	m.mu.RUnlock()

	if !enabled {
		next.ServeHTTP(w, r)
		return
	}

	// Health endpoints are always public — no authentication required.
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		next.ServeHTTP(w, r)
		return
	}

	// Extract HMAC headers
	brokerID := r.Header.Get(apiclient.HeaderBrokerID)
	timestamp := r.Header.Get(apiclient.HeaderTimestamp)
	nonce := r.Header.Get(apiclient.HeaderNonce)
	signature := r.Header.Get(apiclient.HeaderSignature)

	// If no HMAC headers present, check if unauthenticated requests are allowed
	if brokerID == "" && timestamp == "" && signature == "" {
		if allowUnauth {
			next.ServeHTTP(w, r)
			return
		}
		m.writeError(w, "missing authentication headers")
		return
	}

	// Validate required headers
	if brokerID == "" {
		m.writeError(w, "missing X-Scion-Broker-ID header")
		return
	}
	if timestamp == "" {
		m.writeError(w, "missing X-Scion-Timestamp header")
		return
	}
	if signature == "" {
		m.writeError(w, "missing X-Scion-Signature header")
		return
	}

	// Validate timestamp
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		m.writeError(w, "invalid timestamp format")
		return
	}

	requestTime := time.Unix(ts, 0)
	clockSkew := time.Since(requestTime)
	if clockSkew < 0 {
		clockSkew = -clockSkew
	}
	if clockSkew > maxSkew {
		m.writeError(w, fmt.Sprintf("timestamp outside acceptable range (skew: %v)", clockSkew))
		return
	}

	// Decode the signature
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		m.writeError(w, "invalid signature encoding")
		return
	}

	// Build canonical string
	canonical := apiclient.BuildCanonicalString(r, timestamp, nonce)

	// Broker ID and connection headers are not part of the HMAC canonical
	// string. Bind them to exactly one matching signing authority.
	var matches []secretKeyEntry
	for _, entry := range keys {
		if apiclient.VerifyHMAC(entry.secretKey, canonical, sigBytes) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		m.writeError(w, "invalid signature")
		return
	}
	if len(matches) != 1 {
		m.writeError(w, "ambiguous signing authority")
		return
	}
	entry := matches[0]
	if entry.brokerID == "" || brokerID != entry.brokerID {
		m.writeError(w, "broker identity does not match signing authority")
		return
	}
	if requested := r.Header.Get("X-Scion-Hub-Connection"); requested != "" && requested != entry.hubName {
		m.writeError(w, "hub connection does not match signing authority")
		return
	}
	if entry.connection != nil {
		// Keep admission separate from hc.mu: handlers may take snapshots
		// under hc.mu while a credential transition waits for this lease.
		conn := entry.connection
		conn.authorityMu.RLock()
		conn.mu.RLock()
		current := conn.authorityReady.Load() &&
			conn.authorityGeneration.Load() == entry.generation &&
			conn.Name == entry.hubName && conn.BrokerID == entry.brokerID &&
			conn.HubEndpoint == entry.hubEndpoint && bytes.Equal(conn.SecretKey, entry.secretKey)
		conn.mu.RUnlock()
		if !current {
			conn.authorityMu.RUnlock()
			m.writeError(w, "stale signing authority")
			return
		}
		defer conn.authorityMu.RUnlock()
	}
	authority := &brokerRequestAuthority{
		hubName: entry.hubName, brokerID: entry.brokerID, hubEndpoint: entry.hubEndpoint,
		generation: entry.generation, connection: entry.connection,
	}
	ctx := context.WithValue(r.Context(), brokerRequestAuthorityKey{}, authority)
	ctx = context.WithValue(ctx, authenticatingHubConnCtxKey{}, entry.hubName)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// writeError writes an authentication error response.
func (m *MultiKeyBrokerAuthMiddleware) writeError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"error":{"code":"broker_auth_failed","message":%q}}`, message)
}
