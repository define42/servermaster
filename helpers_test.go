package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCommand installs an executable shell script named name on PATH for the
// duration of the test, so code that shells out hits a controlled stand-in
// instead of a real host tool.
func fakeCommand(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil { //nolint:gosec // a test stub must be executable.
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// stubConfigApplier replaces the host-mutating apply with fn for the duration
// of a test and returns a function that restores the original.
func stubConfigApplier(fn func(*Config) error) func() {
	prev := configApplier
	configApplier = fn
	return func() { configApplier = prev }
}
