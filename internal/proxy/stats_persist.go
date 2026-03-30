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

// Persist dumps the current global stats to the legacy default JSON file.
// Prefer PersistTo(path) in runtime code so the caller controls the stats bucket.
func (g *GlobalStats) Persist() {
	g.PersistTo(resolveStatsPath())
}

// PersistTo dumps the current global stats to the provided JSON file.
func (g *GlobalStats) PersistTo(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Error("failed to create stats directory", "path", filepath.Dir(path), "error", err)
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
		slog.Error("failed to marshal stats to JSON", "path", path, "error", err)
		return
	}

	if err := writeFileAtomically(path, data, 0o600); err != nil {
		slog.Error("failed to write stats file", "path", path, "error", err)
		return
	}
}

func writeFileAtomically(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := tmp.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true

	return os.Rename(tmpPath, path)
}

// LoadFromDisk reads the legacy default stats JSON file and populates the counters.
// Prefer LoadFromDiskAt(path) in runtime code so the caller controls the stats bucket.
func (g *GlobalStats) LoadFromDisk() {
	g.LoadFromDiskAt(resolveStatsPath())
}

// LoadFromDiskAt reads the provided stats JSON file and populates the counters.
func (g *GlobalStats) LoadFromDiskAt(path string) {
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
		atomic.StoreInt64(&ps.CircuitOpenUntil, view.CircuitOpenUntil)
		atomic.StoreInt32(&ps.ConsecutiveErrors, view.ConsecutiveErrors)
	}

	slog.Info("loaded persistent stats", "path", path, "providers_count", len(snap))
}
