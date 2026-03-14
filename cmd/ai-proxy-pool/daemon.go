package main

import (
	"bufio"
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
	args := []string{"run", "--config", resolveConfigPath(), "--log", resolveLogPath()}

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
		if isProcessNotRunningError(err) {
			_ = os.Remove(pidFile)
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("send SIGTERM: %w", err)
	}

	// 等待进程退出
	for i := 0; i < 50; i++ { // 最多等待 5 秒
		time.Sleep(100 * time.Millisecond)
		if err := process.Signal(syscall.Signal(0)); err != nil {
			if !isProcessNotRunningError(err) {
				return 0, fmt.Errorf("check daemon status: %w", err)
			}
			_ = os.Remove(pidFile)
			return pid, nil
		}
	}

	return 0, fmt.Errorf("daemon did not stop in time, pid=%d", pid)
}

func isProcessNotRunningError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "process already finished") || strings.Contains(msg, "no such process")
}

// showLogs displays and follows the log output with colored log levels.
func showLogs() error {
	path := resolveLogPath()
	cmd := exec.Command("tail", "-n", "200", "-f", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to show logs: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		fmt.Fprintln(os.Stdout, colorizeLine(scanner.Text()))
	}

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 130, 143:
				return nil
			}
		}
		return fmt.Errorf("failed to show logs: %w", err)
	}
	return nil
}

// writePID writes the current process ID to a file.
func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}
