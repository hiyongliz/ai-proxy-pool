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

	"github.com/charmbracelet/lipgloss"
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
		// Signal(0) 返回 nil 表示进程还在运行，返回 err (如 ESRCH) 表示进程已退出
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

	// 跨平台兜底：不同系统/Go 版本可能返回不同文本
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "process already finished") || strings.Contains(msg, "no such process")
}

// showLogs displays and follows the log output with colored log levels.
func showLogs() {
	path := resolveLogPath()
	cmd := exec.Command("tail", "-n", "200", "-f", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pipe: %v\n", err)
		os.Exit(1)
	}
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to show logs: %v\n", err)
		os.Exit(1)
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
				return
			}
		}
		fmt.Fprintf(os.Stderr, "failed to show logs: %v\n", err)
		os.Exit(1)
	}
}

var (
	logTimeStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	logInfoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	logWarnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	logErrorStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	logAddrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	logNumStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	logStatus2xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	logStatus3xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	logStatus4xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	logStatus5xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// colorizeLine highlights fields in a slog text line.
func colorizeLine(line string) string {
	// Level coloring
	switch {
	case strings.Contains(line, "level=ERROR"):
		line = strings.Replace(line, "level=ERROR", "level="+logErrorStyle.Render("ERROR"), 1)
	case strings.Contains(line, "level=WARN"):
		line = strings.Replace(line, "level=WARN", "level="+logWarnStyle.Render("WARN"), 1)
	case strings.Contains(line, "level=INFO"):
		line = strings.Replace(line, "level=INFO", "level="+logInfoStyle.Render("INFO"), 1)
	}

	// Time coloring: time=2026-03-03T17:01:18.985+08:00
	if idx := strings.Index(line, "time="); idx >= 0 {
		end := strings.Index(line[idx:], " ")
		if end > 0 {
			timeVal := line[idx+5 : idx+end]
			line = strings.Replace(line, "time="+timeVal, "time="+logTimeStyle.Render(timeVal), 1)
		}
	}

	// Address coloring: remote_addr, listen, upstream_host
	for _, key := range []string{"remote_addr=", "listen=", "upstream_host="} {
		if idx := strings.Index(line, key); idx >= 0 {
			rest := line[idx+len(key):]
			end := strings.Index(rest, " ")
			if end < 0 {
				end = len(rest)
			}
			val := rest[:end]
			line = strings.Replace(line, key+val, key+logAddrStyle.Render(val), 1)
		}
	}

	// Special handling for HTTP Status code with dynamic coloring
	if idx := strings.Index(line, "status="); idx >= 0 {
		rest := line[idx+7:]
		end := strings.Index(rest, " ")
		if end < 0 {
			end = len(rest)
		}
		val := rest[:end]
		if statusInt, err := strconv.Atoi(val); err == nil {
			var coloredStatus string
			switch {
			case statusInt >= 200 && statusInt < 300:
				coloredStatus = logStatus2xxStyle.Render(val)
			case statusInt >= 300 && statusInt < 400:
				coloredStatus = logStatus3xxStyle.Render(val)
			case statusInt >= 400 && statusInt < 500:
				coloredStatus = logStatus4xxStyle.Render(val)
			case statusInt >= 500 && statusInt < 600:
				coloredStatus = logStatus5xxStyle.Render(val)
			default:
				coloredStatus = logNumStyle.Render(val)
			}
			line = strings.Replace(line, "status="+val, "status="+coloredStatus, 1)
		}
	}

	// Numeric values: duration_ms=, response_bytes=
	for _, key := range []string{"duration_ms=", "response_bytes="} {
		if idx := strings.Index(line, key); idx >= 0 {
			rest := line[idx+len(key):]
			end := strings.Index(rest, " ")
			if end < 0 {
				end = len(rest)
			}
			val := rest[:end]
			line = strings.Replace(line, key+val, key+logNumStyle.Render(val), 1)
		}
	}

	return line
}

// writePID writes the current process ID to a file.
func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}
