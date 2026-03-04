package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonize forks a new process to run as a background daemon.
func daemonize() {
	args := make([]string, 0, len(os.Args))
	for _, arg := range os.Args[1:] {
		if arg == "-d" || arg == "-stop" || arg == "-restart" || arg == "-logs" {
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

// stopDaemonAndWait sends SIGTERM to the daemon and waits for it to exit.
func stopDaemonAndWait() (int, error) {
	pidFile := pidPath()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("find process: %w", err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return 0, fmt.Errorf("send SIGTERM: %w", err)
	}

	// 等待进程退出
	for i := 0; i < 50; i++ { // 最多等待 5 秒
		time.Sleep(100 * time.Millisecond)
		if err := process.Signal(syscall.Signal(0)); err != nil {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("daemon did not stop in time, pid=%d", pid)
}

// stopDaemon stops the running daemon and exits.
func stopDaemon() {
	pid, err := stopDaemonAndWait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
	os.Exit(0)
}

// restartDaemon stops the running daemon and starts a new one.
func restartDaemon() {
	pid, err := stopDaemonAndWait()
	switch {
	case err == nil:
		fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(os.Stderr, "daemon not running, starting a new daemon\n")
	default:
		fmt.Fprintf(os.Stderr, "failed to restart daemon: %v\n", err)
		os.Exit(1)
	}

	daemonize()
}

// showLogs displays and follows the log output.
func showLogs() {
	path := resolveLogPath()
	cmd := exec.Command("tail", "-n", "200", "-f", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 130, 143:
				return
			}
		}
		fmt.Fprintf(os.Stderr, "failed to show logs: %v\n", err)
		os.Exit(1)
	}
}

// writePID writes the current process ID to a file.
func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}
