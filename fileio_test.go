package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteConfigFileMkdirError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	// The parent of the target is a regular file, so MkdirAll must fail.
	if err := writeConfigFile(filepath.Join(file, "sub", "config.json"), []byte("{}")); err == nil {
		t.Fatal("expected mkdir error when a parent path component is a file")
	}
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on a real directory: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error fsyncing a missing directory")
	}
}

func TestWriteConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "containers.json") // nested dir must be created
	body := []byte(`{"podman_mode":"rootful"}`)

	if err := writeConfigFile(path, body); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}

	got, err := os.ReadFile(path) //nolint:gosec // reads a temp-dir fixture the test just wrote.
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("contents = %q, want %q", got, body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("mode = %o, want 0644", perm)
	}
}
