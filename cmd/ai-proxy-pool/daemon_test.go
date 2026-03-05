package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestStopDaemonAndWaitRunningProcess(t *testing.T) {
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
