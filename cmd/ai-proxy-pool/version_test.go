package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestVersionCommandDefaultOutput(t *testing.T) {
	origVersion, origCommit, origBuildDate, origBuiltBy := version, commit, buildDate, builtBy
	version, commit, buildDate, builtBy = "1.2.3", "abc1234", "2026-03-05T00:00:00Z", "ci"
	t.Cleanup(func() {
		version, commit, buildDate, builtBy = origVersion, origCommit, origBuildDate, origBuiltBy
	})

	buf := bytes.NewBuffer(nil)
	cmd := newVersionCommand()
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{"appool version", "version: 1.2.3", "commit: abc1234", "build_date: 2026-03-05T00:00:00Z", "built_by: ci"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("expected output to contain %q, got: %s", expected, out)
		}
	}
}

func TestVersionCommandShortOutput(t *testing.T) {
	origVersion := version
	version = "9.9.9"
	t.Cleanup(func() { version = origVersion })

	buf := bytes.NewBuffer(nil)
	cmd := newVersionCommand()
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--short"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version --short: %v", err)
	}

	if got, want := buf.String(), "9.9.9\n"; got != want {
		t.Fatalf("unexpected short output: got=%q want=%q", got, want)
	}
}

func TestVersionCommandJSONOutput(t *testing.T) {
	origVersion, origCommit, origBuildDate, origBuiltBy := version, commit, buildDate, builtBy
	version, commit, buildDate, builtBy = "2.0.0", "ff00ff", "2026-03-05T01:02:03Z", "release-bot"
	t.Cleanup(func() {
		version, commit, buildDate, builtBy = origVersion, origCommit, origBuildDate, origBuiltBy
	})

	buf := bytes.NewBuffer(nil)
	cmd := newVersionCommand()
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version --json: %v", err)
	}

	var info versionInfo
	if err := json.Unmarshal(buf.Bytes(), &info); err != nil {
		t.Fatalf("unmarshal json output: %v, output=%q", err, buf.String())
	}

	if info.Version != "2.0.0" || info.Commit != "ff00ff" || info.BuildDate != "2026-03-05T01:02:03Z" || info.BuiltBy != "release-bot" {
		t.Fatalf("unexpected version fields: %#v", info)
	}
	if info.Go != runtime.Version() {
		t.Fatalf("unexpected go version: got=%q want=%q", info.Go, runtime.Version())
	}
	if info.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("unexpected platform: got=%q", info.Platform)
	}
}

func TestVersionCommandRejectsConflictingFlags(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	cmd := newVersionCommand()
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--short", "--json"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("unexpected error: %v", err)
	}
}
