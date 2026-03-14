package proxy

import (
	"net/http"
	"sync/atomic"
)

// ReloadableHandler wraps a Server and allows hot-reloading without restarting.
type ReloadableHandler struct {
	current atomic.Pointer[Server]
}

// NewReloadableHandler creates a ReloadableHandler with the initial server.
func NewReloadableHandler(s *Server) *ReloadableHandler {
	h := &ReloadableHandler{}
	h.current.Store(s)
	return h
}

// ServeHTTP delegates to the current server's handler.
func (h *ReloadableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.current.Load().Handler().ServeHTTP(w, r)
}

// Reload atomically swaps in a new server configuration.
// It closes idle connections on the old server to avoid leaking resources.
func (h *ReloadableHandler) Reload(s *Server) {
	old := h.current.Swap(s)
	if old != nil {
		for _, c := range old.clients {
			c.CloseIdleConnections()
		}
	}
}
