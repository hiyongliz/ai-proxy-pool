package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

var (
	flagConfig       string
	flagDaemon       bool
	flagStop         bool
	flagRestart      bool
	flagLogs         bool
	flagSwitchConfig bool
	flagLog          string
)

func init() {
	flag.StringVar(&flagConfig, "config", "", "path to config file")
	flag.BoolVar(&flagDaemon, "d", false, "run as daemon")
	flag.BoolVar(&flagStop, "stop", false, "stop the running daemon")
	flag.BoolVar(&flagRestart, "restart", false, "restart the running daemon")
	flag.BoolVar(&flagLogs, "logs", false, "show and follow log output")
	flag.BoolVar(&flagSwitchConfig, "switch-config", false, "interactive switch active config from ~/.ai_proxy_pool/configs and ~/.ai_proxy_pool/config.yaml")
	flag.StringVar(&flagLog, "log", "", "log file path (default: ~/.ai_proxy_pool/ai-proxy-pool.log)")
}

func defaultDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ai_proxy_pool")
	}
	return "."
}

func resolveConfigPath() string {
	if flagConfig != "" {
		return flagConfig
	}
	return filepath.Join(defaultDir(), "config.yaml")
}

func resolveLogPath() string {
	if flagLog != "" {
		return flagLog
	}
	return filepath.Join(defaultDir(), "ai-proxy-pool.log")
}

func resolveConfigsDir() string {
	return filepath.Join(defaultDir(), "configs")
}

func pidPath() string {
	return filepath.Join(defaultDir(), "ai-proxy-pool.pid")
}

func setupLogger(path string, daemon bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	var w io.Writer = f
	if !daemon {
		// 前台运行：同时输出到 stdout 和日志文件
		w = io.MultiWriter(os.Stdout, f)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return f, nil
}

func main() {
	flag.Parse()

	if flagSwitchConfig {
		switchConfigInteractive()
	}

	if flagLogs {
		showLogs()
	}

	if flagStop {
		stopDaemon()
	}

	if flagRestart {
		restartDaemon()
	}

	// 父进程：fork 子进程后退出
	if flagDaemon && os.Getenv("_AI_PROXY_POOL_DAEMON") != "1" {
		daemonize()
	}

	isDaemon := os.Getenv("_AI_PROXY_POOL_DAEMON") == "1"

	logPath := resolveLogPath()
	logFile, err := setupLogger(logPath, isDaemon)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup logger failed: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	cfgPath := resolveConfigPath()
	slog.Info("using config", "path", cfgPath)
	slog.Info("logging to", "path", logPath)

	if err := writePID(pidPath()); err != nil {
		slog.Error("write pid file failed", "error", err)
	}
	defer os.Remove(pidPath())

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

	handler := proxy.NewReloadableHandler(server)

	// 启动文件监听
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create file watcher", "error", err)
	} else {
		defer watcher.Close()
		cfgDir := filepath.Dir(cfgPath)
		cfgFile := filepath.Base(cfgPath)

		if err := watcher.Add(cfgDir); err != nil {
			slog.Error("failed to watch config directory", "path", cfgDir, "error", err)
		} else {
			slog.Info("watching config file for changes", "path", cfgPath, "watch_dir", cfgDir)
			go func() {
				for {
					select {
					case event, ok := <-watcher.Events:
						if !ok {
							return
						}
						if filepath.Base(event.Name) != cfgFile {
							continue
						}
						if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
							// 延迟一点，等待文件写入完成
							time.Sleep(100 * time.Millisecond)
							reloadConfig(cfgPath, &cfg, handler, "file_change")
						}
					case err, ok := <-watcher.Errors:
						if !ok {
							return
						}
						slog.Error("file watcher error", "error", err)
					}
				}
			}()
		}
	}

	httpServer := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      handler,
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
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			reloadConfig(cfgPath, &cfg, handler, "SIGHUP")
			continue
		}

		// SIGINT or SIGTERM
		break
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("proxy server stopped")
}
