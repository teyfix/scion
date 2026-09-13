package runtimebroker

import (
	"net/http"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/google/uuid"
)

// lockAgentLifecycle is shared by creation and every container-mutating
// lifecycle action. Fail immediately rather than queue an obsolete mutation.
func (s *Server) lockAgentLifecycle(w http.ResponseWriter, r *http.Request, id, projectID string) (func(), bool) {
	slug := api.Slugify(id)
	aliasKey := projectID + "/" + id
	if alias, ok := s.agentLifecycleAliases.Load(aliasKey); ok {
		slug = alias.(string)
	} else if _, err := uuid.Parse(id); err == nil {
		agents, err := s.manager.List(r.Context(), map[string]string{"scion.agent": "true"})
		if err != nil {
			RuntimeError(w, "Cannot resolve lifecycle identity: "+err.Error())
			return nil, false
		}
		for _, a := range agents {
			if matchesAgent(a, id, projectID) {
				slug = api.Slugify(a.Name)
				s.agentLifecycleAliases.LoadOrStore(aliasKey, slug)
				break
			}
		}
	}
	actual, _ := s.agentLifecycleMu.LoadOrStore(projectID+"/"+slug, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	if !mu.TryLock() {
		Conflict(w, "Another lifecycle operation is in progress for this agent; refresh before retrying")
		return nil, false
	}
	return mu.Unlock, true
}

// bindLifecycleAlias is called while the slug lifecycle lock is held, before
// removing a container. Keep the former UUID lock held too, so an operation
// that resolved an absent container before this binding cannot race creation.
func (s *Server) bindLifecycleAlias(w http.ResponseWriter, name, id, projectID string) (func(), bool) {
	if id == "" || id == name {
		return func() {}, true
	}
	key := projectID + "/" + id
	actual, _ := s.agentLifecycleMu.LoadOrStore(key, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	if !mu.TryLock() {
		Conflict(w, "Another lifecycle operation is in progress for this agent; refresh before retrying")
		return nil, false
	}
	slug := api.Slugify(name)
	if old, _ := s.agentLifecycleAliases.LoadOrStore(key, slug); old.(string) != slug {
		mu.Unlock()
		Conflict(w, "Lifecycle identity does not match the retained agent")
		return nil, false
	}
	return mu.Unlock, true
}
