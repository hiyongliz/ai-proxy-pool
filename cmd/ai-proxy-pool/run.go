package main

import (
	"context"
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
	"github.com/spf13/cobra"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

func init() {
	rootCmd.AddCommand(runCmd)
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the proxy server in the foreground",
	RunE:  runServer,
}

type colorWriter struct {
	w io.Writer
}

func (cw *colorWriter) Write(p []byte) (n int, err error) {
	colored := colorizeLine(string(p))
	// 如果 colored 结尾没有换行符而原始数据有，补回来（尽管 slog.TextHandler 通常以 \n 结尾）
	return cw.w.Write([]byte(colored))
}

func setupLogger(path string, daemon bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	if daemon {
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	} else {
		// 前台运行：文件写原始文本，stdout 写美化后的文本
		fileLogger := slog.NewTextHandler(f, nil)
		stdoutHandler := slog.NewTextHandler(&colorWriter{os.Stdout}, nil)
		// 采用简单的多写模式，分别往文件和带颜色包装的 stdout 输出
		slog.SetDefault(slog.New(&multiHandler{fileLogger, stdoutHandler}))
	}

	return f, nil
}

type multiHandler struct {
	h1 slog.Handler
	h2 slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return m.h1.Enabled(ctx, level) || m.h2.Enabled(ctx, level)
}

func (m *multiHandler) Handle(ctx context.Context, rec slog.Record) error {
	_ = m.h1.Handle(ctx, rec)
	return m.h2.Handle(ctx, rec)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{
		h1: m.h1.WithAttrs(attrs),
		h2: m.h2.WithAttrs(attrs),
	}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{
		h1: m.h1.WithGroup(name),
		h2: m.h2.WithGroup(name),
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	isDaemon := os.Getenv("_AI_PROXY_POOL_DAEMON") == "1"

	logPath := resolveLogPath()
	logFile, err := setupLogger(logPath, isDaemon)
	if err != nil {
		return fmt.Errorf("setup logger failed: %w", err)
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
		return fmt.Errorf("load config failed: path=%s, %w", cfgPath, err)
	}

	proxy.GetGlobalStats().LoadFromDisk()

	server, err := proxy.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("build proxy server failed: %w", err)
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
				var debounceTimer *time.Timer
				for {
					select {
					case event, ok := <-watcher.Events:
						if !ok {
							return
						}
						// 只关注目标配置文件
						if filepath.Base(event.Name) != cfgFile {
							continue
						}
						if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
							// Debounce: 延迟 200ms 触发，合并连续的保存操作事件
							if debounceTimer != nil {
								debounceTimer.Stop()
							}
							debounceTimer = time.AfterFunc(200*time.Millisecond, func() {
								reloadConfig(cfgPath, &cfg, handler, "file_change")
							})
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

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// 后台定期落盘持久化
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			proxy.GetGlobalStats().Persist()
		}
	}()

	for sig := range done {
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
		return fmt.Errorf("shutdown failed: %w", err)
	}

	// 退出前最后一次强制落盘
	proxy.GetGlobalStats().Persist()

	slog.Info("proxy server stopped")
	return nil
}
