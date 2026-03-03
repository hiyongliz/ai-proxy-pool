package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/metrics"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

var (
	flagConfig string
	flagDaemon bool
	flagStop   bool
	flagLog    string
)

func init() {
	flag.StringVar(&flagConfig, "config", "", "path to config file")
	flag.BoolVar(&flagDaemon, "d", false, "run as daemon")
	flag.BoolVar(&flagStop, "stop", false, "stop the running daemon")
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

func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func daemonize() {
	args := make([]string, 0, len(os.Args))
	for _, arg := range os.Args[1:] {
		if arg == "-d" {
			continue
		}
		args = append(args, arg)
	}

	// 传递解析后的路径，确保子进程使用相同配置
	hasConfig := false
	hasLog := false
	for _, arg := range args {
		if arg == "-config" {
			hasConfig = true
		}
		if arg == "-log" {
			hasLog = true
		}
	}
	if !hasConfig {
		args = append(args, "-config", resolveConfigPath())
	}
	if !hasLog {
		args = append(args, "-log", resolveLogPath())
	}

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "_AI_PROXY_POOL_DAEMON=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		slog.Error("failed to start daemon", "error", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "daemon started, pid=%d, log=%s\n", cmd.Process.Pid, resolveLogPath())
	os.Exit(0)
}

func stopDaemon() {
	pidFile := pidPath()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read pid file: %v\n", err)
		os.Exit(1)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid pid: %v\n", err)
		os.Exit(1)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find process: %v\n", err)
		os.Exit(1)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "failed to send signal: %v\n", err)
		os.Exit(1)
	}

	// 等待进程退出
	for i := 0; i < 50; i++ { // 最多等待 5 秒
		time.Sleep(100 * time.Millisecond)
		if err := process.Signal(syscall.Signal(0)); err != nil {
			fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
			os.Exit(0)
		}
	}

	fmt.Fprintf(os.Stderr, "daemon did not stop in time, pid=%d\n", pid)
	os.Exit(1)
}

func main() {
	flag.Parse()

	if flagStop {
		stopDaemon()
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
			slog.Info("received SIGHUP, reloading config", "path", cfgPath)

			newCfg, err := config.Load(cfgPath)
			if err != nil {
				metrics.ConfigReloadsTotal.WithLabelValues("failure").Inc()
				slog.Error("config reload failed, keeping current config", "error", err)
				continue
			}

			if newCfg.Server.ListenAddr != cfg.Server.ListenAddr {
				slog.Warn("listen_addr changed, requires full restart to take effect",
					"old", cfg.Server.ListenAddr,
					"new", newCfg.Server.ListenAddr,
				)
			}

			newServer, err := proxy.NewServer(newCfg)
			if err != nil {
				metrics.ConfigReloadsTotal.WithLabelValues("failure").Inc()
				slog.Error("server rebuild failed, keeping current config", "error", err)
				continue
			}

			handler.Reload(newServer)
			cfg = newCfg
			metrics.ConfigReloadsTotal.WithLabelValues("success").Inc()
			slog.Info("config reloaded successfully")
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
