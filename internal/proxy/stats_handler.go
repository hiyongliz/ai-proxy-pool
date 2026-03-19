package proxy

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// handleStatus 内部暴漏出一个零依赖的本地 HTTP API 供前端 status 命令查询
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Restrict internal status API to loopback requests only.
	if !isLoopbackRequest(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	stats := s.stats.Snapshot()
	payload := map[string]any{
		"server": map[string]any{
			"uptime_seconds": int(time.Since(serverStartTime).Seconds()),
			"providers":      len(s.cfg.Providers),
			"strategy":       s.cfg.Router.Strategy,
		},
		"providers": stats,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		slog.Error("handleStatus encode failed", "error", err)
	}
}

func (s *Server) handleStatusReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	if !isLoopbackRequest(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	if s.stats != nil {
		s.stats.ResetAllCounters()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func isLoopbackRequest(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
