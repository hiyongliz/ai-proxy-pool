package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

func resolveConfigPath() string {
	cfgPath := flag.String("config", "", "path to config file")
	flag.Parse()

	if *cfgPath != "" {
		return *cfgPath
	}

	// 未指定时，依次尝试当前目录和 home 目录
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}

	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ai_proxy_pool", "config.yaml")
	}

	return "config.yaml"
}

func main() {
	cfgPath := resolveConfigPath()

	slog.Info("using config", "path", cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("load config failed", "path", cfgPath, "error", err)
		os.Exit(1)
	}

	server, err := proxy.NewServer(cfg)
	if err != nil {
		slog.Error("build proxy server failed", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	go func() {
		slog.Info("proxy server started", "listen", cfg.Server.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("proxy server stopped")
}
