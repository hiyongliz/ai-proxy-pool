package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestStopDaemonAndWaitRunningProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := startDaemonHelperProcess(t)
	detachedPID := cmd.Process.Pid
	t.Cleanup(func() {
		proc, findErr := os.FindProcess(detachedPID)
		if findErr == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	})

	pidFile := pidPath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(detachedPID)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	pid, err := stopDaemonAndWait()
	if err != nil {
		t.Fatalf("stopDaemonAndWait: %v", err)
	}
	if pid != detachedPID {
		t.Fatalf("unexpected pid: got=%d want=%d", pid, detachedPID)
	}

	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file should be removed, stat err=%v", err)
	}
}

func TestStopDaemonAndWaitRejectsDifferentProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatalf("start detached sleep: %v", err)
	}
	detachedPIDStr := strings.TrimSpace(string(bytes.TrimSpace(out)))
	detachedPID, err := strconv.Atoi(detachedPIDStr)
	if err != nil {
		t.Fatalf("parse detached pid %q: %v", detachedPIDStr, err)
	}
	t.Cleanup(func() {
		proc, findErr := os.FindProcess(detachedPID)
		if findErr == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	})

	pidFile := pidPath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(detachedPID)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if _, err := stopDaemonAndWait(); err == nil || !strings.Contains(err.Error(), "different process") {
		t.Fatalf("expected different process error, got: %v", err)
	}

	process, err := os.FindProcess(detachedPID)
	if err != nil {
		t.Fatalf("find detached process: %v", err)
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("expected detached process to still be alive, got: %v", err)
	}
}

func TestStopDaemonAndWaitStalePIDFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short process: %v", err)
	}
	stalePID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short process: %v", err)
	}

	pidFile := pidPath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(stalePID)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if _, err := stopDaemonAndWait(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist for stale pid file, got: %v", err)
	}

	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file should be removed for stale pid, stat err=%v", err)
	}
}

func TestSignalDaemonReloadRejectsDifferentProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatalf("start detached sleep: %v", err)
	}
	detachedPIDStr := strings.TrimSpace(string(bytes.TrimSpace(out)))
	detachedPID, err := strconv.Atoi(detachedPIDStr)
	if err != nil {
		t.Fatalf("parse detached pid %q: %v", detachedPIDStr, err)
	}
	t.Cleanup(func() {
		proc, findErr := os.FindProcess(detachedPID)
		if findErr == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	})

	pidFile := pidPath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(detachedPID)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	result := signalDaemonReload()
	if result.signalSent {
		t.Fatal("expected reload signal to be rejected for different process")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "different process") {
		t.Fatalf("expected different process error, got: %v", result.err)
	}
}

func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("APPPOOL_HELPER_PROCESS") != "1" {
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		switch <-sigCh {
		case syscall.SIGTERM:
			os.Exit(0)
		case syscall.SIGHUP:
		}
	}
}

func startDaemonHelperProcess(t *testing.T) *exec.Cmd {
	t.Helper()

	out, err := exec.Command("sh", "-c", `APPPOOL_HELPER_PROCESS=1 "$1" -test.run=TestDaemonHelperProcess >/dev/null 2>&1 & echo $!`, "sh", os.Args[0]).Output()
	if err != nil {
		t.Fatalf("start detached helper process: %v", err)
	}

	pidStr := strings.TrimSpace(string(bytes.TrimSpace(out)))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("parse detached helper pid %q: %v", pidStr, err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find detached helper process: %v", err)
	}

	return &exec.Cmd{Process: process}
}
