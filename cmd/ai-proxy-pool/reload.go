package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/metrics"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

// reloadConfig reloads the configuration and rebuilds the server.
func reloadConfig(
	cfgPath string,
	cfg *config.Config,
	handler *proxy.ReloadableHandler,
	trigger string,
) {
	slog.Info("reloading config", "trigger", trigger, "path", cfgPath)

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		metrics.ConfigReloadsTotal.WithLabelValues("failure").Inc()
		slog.Error("config reload failed, keeping current config", "error", err)
		return
	}

	warnRestartRequiredServerChanges(*cfg, newCfg)

	newServer, err := proxy.NewServer(newCfg)
	if err != nil {
		metrics.ConfigReloadsTotal.WithLabelValues("failure").Inc()
		slog.Error("server rebuild failed, keeping current config", "error", err)
		return
	}

	handler.Reload(newServer)
	*cfg = newCfg
	metrics.ConfigReloadsTotal.WithLabelValues("success").Inc()
	slog.Info("config reloaded successfully")
}

// warnRestartRequiredServerChanges logs warnings for server config changes that require a restart.
func warnRestartRequiredServerChanges(oldCfg, newCfg config.Config) {
	changed := make([]string, 0, 4)
	if oldCfg.Server.ListenAddr != newCfg.Server.ListenAddr {
		changed = append(changed, fmt.Sprintf("listen_addr: %q -> %q", oldCfg.Server.ListenAddr, newCfg.Server.ListenAddr))
	}
	if oldCfg.Server.ReadTimeout != newCfg.Server.ReadTimeout {
		changed = append(changed, fmt.Sprintf("read_timeout: %s -> %s", oldCfg.Server.ReadTimeout, newCfg.Server.ReadTimeout))
	}
	if oldCfg.Server.WriteTimeout != newCfg.Server.WriteTimeout {
		changed = append(changed, fmt.Sprintf("write_timeout: %s -> %s", oldCfg.Server.WriteTimeout, newCfg.Server.WriteTimeout))
	}
	if oldCfg.Server.IdleTimeout != newCfg.Server.IdleTimeout {
		changed = append(changed, fmt.Sprintf("idle_timeout: %s -> %s", oldCfg.Server.IdleTimeout, newCfg.Server.IdleTimeout))
	}
	if len(changed) > 0 {
		slog.Warn(
			"some server settings changed but require full restart to take effect",
			"changes",
			strings.Join(changed, "; "),
		)
	}
}
