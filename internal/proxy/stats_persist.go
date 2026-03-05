package proxy

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
)

// resolveStatsPath returns the path to the stats JSON file.
// Hardcoded to ~/.ai_proxy_pool/stats.json to align with KISS principles.
func resolveStatsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".ai_proxy_pool", "stats.json")
}

// Persist dumps the current global stats to a JSON file.
func (g *GlobalStats) Persist() {
	path := resolveStatsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Error("failed to create stats directory", "error", err)
		return
	}

	snap := g.Snapshot()
	// Zero out active connections for the persisted state (they aren't active once restarted)
	for k, v := range snap {
		v.ActiveConnections = 0
		snap[k] = v
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		slog.Error("failed to marshal stats to JSON", "error", err)
		return
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("failed to write stats file", "path", path, "error", err)
		return
	}
}

// LoadFromDisk reads the stats JSON file and populates the counters.
func (g *GlobalStats) LoadFromDisk() {
	path := resolveStatsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("failed to read stats file", "path", path, "error", err)
		}
		return
	}

	var snap map[string]ProviderStatView
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Error("failed to unmarshal stats JSON", "error", err)
		return
	}

	for name, view := range snap {
		ps := g.GetOrCreate(name)
		atomic.StoreInt64(&ps.TotalRequests, view.TotalRequests)
		atomic.StoreInt64(&ps.SuccessRequests, view.SuccessRequests)
		atomic.StoreInt64(&ps.ErrorRequests, view.ErrorRequests)
		atomic.StoreInt64(&ps.TotalDurationMs, view.AvgDurationMs*view.TotalRequests) // Reconstruct total duration
		atomic.StoreInt64(&ps.TotalBytes, view.TotalBytes)
		atomic.StoreInt64(&ps.PromptTokens, view.PromptTokens)
		atomic.StoreInt64(&ps.CompletionTokens, view.CompletionTokens)
	}

	slog.Info("loaded persistent stats", "path", path, "providers_count", len(snap))
}
