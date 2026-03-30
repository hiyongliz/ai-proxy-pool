package main

import (
	"bufio"
	"encoding/json"
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

type pidFileRecord struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable,omitempty"`
}

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
	record, err := readPIDRecord(pidFile)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}
	pid := record.PID

	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("find process: %w", err)
	}

	matches, err := managedProcessMatches(record)
	if err != nil {
		if isProcessNotRunningError(err) {
			_ = os.Remove(pidFile)
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("inspect process: %w", err)
	}
	if !matches {
		return 0, fmt.Errorf("pid file points to a different process, pid=%d", pid)
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
	record := pidFileRecord{
		PID:        os.Getpid(),
		Executable: currentExecutablePath(),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pid record: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

func readPIDRecord(path string) (pidFileRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pidFileRecord{}, err
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return pidFileRecord{}, fmt.Errorf("empty pid file")
	}

	var record pidFileRecord
	if err := json.Unmarshal([]byte(trimmed), &record); err == nil && record.PID > 0 {
		return record, nil
	}

	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return pidFileRecord{}, fmt.Errorf("invalid pid file contents: %w", err)
	}

	return pidFileRecord{PID: pid}, nil
}

func managedProcessMatches(record pidFileRecord) (bool, error) {
	commandLine, err := processCommandLine(record.PID)
	if err != nil {
		return false, err
	}

	expectedExecutable := record.Executable
	if expectedExecutable == "" {
		expectedExecutable = currentExecutablePath()
	}
	if expectedExecutable == "" {
		return true, nil
	}

	return executableMatchesCommandLine(expectedExecutable, commandLine), nil
}

func processCommandLine(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", os.ErrProcessDone
		}
		return "", err
	}
	commandLine := strings.TrimSpace(string(out))
	if commandLine == "" {
		return "", os.ErrProcessDone
	}
	return commandLine, nil
}

func executableMatchesCommandLine(expectedExecutable, commandLine string) bool {
	expectedExecutable = normalizeExecutablePath(expectedExecutable)
	fields := strings.Fields(commandLine)
	if len(fields) == 0 {
		return false
	}
	runningExecutable := normalizeExecutablePath(fields[0])
	if runningExecutable == expectedExecutable {
		return true
	}
	return filepath.Base(runningExecutable) == filepath.Base(expectedExecutable)
}

func currentExecutablePath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	return normalizeExecutablePath(executable)
}

func normalizeExecutablePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}
