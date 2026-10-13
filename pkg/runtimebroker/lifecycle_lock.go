package runtimebroker

import (
	"errors"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/google/uuid"
	"net/http"
	"sync"
)

// Starts cannot queue obsolete filesystem mutations behind another start.
// Stop/delete retain upstream's cancellation-and-cleanup protocol.
func (s *Server) lockAgentLifecycle(w http.ResponseWriter, r *http.Request, id, projectID string) (func(), bool) {
	slug := api.Slugify(id)
	aliasKey := projectID + "/" + id
	if alias, ok := s.agentLifecycleAliases.Load(aliasKey); ok {
		slug = alias.(string)
	} else if _, err := uuid.Parse(id); err == nil {
		match, err := s.lookupAgentMatch(r.Context(), id, projectID)
		if err != nil && !errors.Is(err, ErrAgentNotFound) {
			writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeError, "Cannot resolve lifecycle identity", nil)
			return nil, false
		}
		if match.matched {
			slug = api.Slugify(match.entry.Name)
			s.agentLifecycleAliases.LoadOrStore(aliasKey, slug)
		}
	}
	actual, _ := s.agentLifecycleMu.LoadOrStore(projectID+"/"+slug, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	if !mu.TryLock() {
		writeError(w, http.StatusConflict, ErrCodeConflict, "Another lifecycle start is in progress for this agent; refresh before retrying", nil)
		return nil, false
	}
	return mu.Unlock, true
}
