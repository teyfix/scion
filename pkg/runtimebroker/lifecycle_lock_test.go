package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetainedRecoveryLifecycleLockExcludesAllCompetingMutations(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start?projectId=project", nil)
	unlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), req, "test-agent-1", "project")
	require.True(t, ok)
	for _, action := range []string{"start", "stop", "suspend", "restart"} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/"+action+"?projectId=project", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		require.Equal(t, http.StatusConflict, w.Code, "%s: %s", action, w.Body.String())
	}
	deleteRecorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/test-agent-1?projectId=project", nil))
	require.Equal(t, http.StatusConflict, deleteRecorder.Code)
	createRecorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name":"test-agent-1","projectId":"project"}`)))
	require.Equal(t, http.StatusConflict, createRecorder.Code)
	// Unrelated agent lifecycles remain independent.
	otherUnlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), req, "another-agent", "project")
	require.True(t, ok)
	otherUnlock()
	unlock()
	retryUnlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), req, "test-agent-1", "project")
	require.True(t, ok)
	retryUnlock()
}

func TestMalformedRetainedRecoveryCannotFallThroughToOrdinaryStart(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	for _, body := range []string{`{"runtimeRecovery":`, `{"runtimeRecovery":{"update":{}}}`, `{"runtimeRecovery":null}`, `{"runtimeRecovery":null,"branch":"replacement"}`} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	require.Equal(t, 0, mgr.startCalls)
	require.Equal(t, 0, mgr.deleteCalls)
}

func TestRetainedRecoveryLifecycleUUIDAliasExcludesContainerGap(t *testing.T) {
	srv := newTestServer(t)
	const id = "b2d668be-2fd8-4195-aa1d-7ed7717bb9fc"
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	unlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), r, "retained", "project")
	require.True(t, ok)
	aliasUnlock, ok := srv.bindLifecycleAlias(httptest.NewRecorder(), "retained", id, "project")
	require.True(t, ok)
	// With no retained container in List, both route forms must still conflict.
	for _, routeID := range []string{"retained", id} {
		w := httptest.NewRecorder()
		_, ok = srv.lockAgentLifecycle(w, r, routeID, "project")
		require.False(t, ok)
		require.Equal(t, http.StatusConflict, w.Code)
	}
	aliasUnlock()
	unlock()
	uuidUnlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), r, id, "project")
	require.True(t, ok)
	w := httptest.NewRecorder()
	_, ok = srv.lockAgentLifecycle(w, r, "retained", "project")
	require.False(t, ok)
	uuidUnlock()
}

func TestRetainedRecoveryLifecycleAliasRejectsEarlierUUIDOperation(t *testing.T) {
	srv := newTestServer(t)
	const id = "b2d668be-2fd8-4195-aa1d-7ed7717bb9fc"
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	uuidUnlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), r, id, "project")
	require.True(t, ok)
	slugUnlock, ok := srv.lockAgentLifecycle(httptest.NewRecorder(), r, "retained", "project")
	require.True(t, ok)
	w := httptest.NewRecorder()
	_, ok = srv.bindLifecycleAlias(w, "retained", id, "project")
	require.False(t, ok)
	require.Equal(t, http.StatusConflict, w.Code)
	uuidUnlock()
	aliasUnlock, ok := srv.bindLifecycleAlias(httptest.NewRecorder(), "retained", id, "project")
	require.True(t, ok)
	aliasUnlock()
	slugUnlock()
}
