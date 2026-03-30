package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

func resolveStatsIdentityPath(cfgPath string) string {
	return resolveActiveConfigForDisplay(cfgPath)
}

func resolveStatsPath(cfgPath string) string {
	identity := normalizedPath(resolveStatsIdentityPath(cfgPath))
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(defaultDir(), "stats", hex.EncodeToString(sum[:8])+".json")
}
