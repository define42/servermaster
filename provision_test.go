package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFileMode(t *testing.T) {
	tests := []struct {
		name    string
		chmod   string
		want    os.FileMode
		wantErr bool
	}{
		{"leading zero", "0755", 0o755, false},
		{"no leading zero", "755", 0o755, false},
		{"setuid bits", "4755", 0o4755, false},
		{"trims whitespace", " 0644 ", 0o644, false},
		{"empty", "", 0, true},
		{"exceeds 07777", "10000", 0, true},
		{"non-octal digit", "8", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFileMode(tt.chmod)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFileMode(%q) error = %v, wantErr %v", tt.chmod, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parseFileMode(%q) = %o, want %o", tt.chmod, got, tt.want)
			}
		})
	}
}

func TestParseOwner(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		wantUID int
		wantGID int
		wantErr bool
	}{
		{"uid only", "1000", 1000, -1, false},
		{"uid and gid", "1000:2000", 1000, 2000, false},
		{"root", "0:0", 0, 0, false},
		{"trims whitespace", " 1000:2000 ", 1000, 2000, false},
		{"empty", "", -1, -1, true},
		{"missing user", ":2000", -1, -1, true},
		{"empty group", "1000:", -1, -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid, err := parseOwner(tt.owner)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOwner(%q) error = %v, wantErr %v", tt.owner, err, tt.wantErr)
			}
			if err == nil && (uid != tt.wantUID || gid != tt.wantGID) {
				t.Fatalf("parseOwner(%q) = (%d, %d), want (%d, %d)", tt.owner, uid, gid, tt.wantUID, tt.wantGID)
			}
		})
	}
}

func TestEnsureFileVariants(t *testing.T) {
	dir := t.TempDir()
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	// base64 content + explicit owner + mode exercises decode, chmod and chown.
	if err := ensureFiles([]FileConfig{{Path: filepath.Join(dir, "f"), Encoding: "base64", Content: "aGk=", Chmod: "0600", User: owner}}); err != nil {
		t.Fatalf("ensureFiles: %v", err)
	}

	bad := []FileConfig{
		{Path: filepath.Join(dir, "a"), Chmod: "99999"},
		{Path: filepath.Join(dir, "b"), Encoding: "rot13"},
		{Path: filepath.Join(dir, "c"), User: "no-such-user-xyz"},
	}
	for _, f := range bad {
		if err := ensureFiles([]FileConfig{f}); err == nil {
			t.Fatalf("expected error for %+v", f)
		}
	}
}

func TestEnsureHostname(t *testing.T) {
	if err := ensureHostname(""); err != nil {
		t.Fatalf("empty hostname must be a no-op: %v", err)
	}

	// A hostname equal to the running one is a no-op and never shells out, so
	// this is safe before the hostnamectl stub is installed.
	current, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	if err := ensureHostname(current); err != nil {
		t.Fatalf("hostname equal to current must be a no-op: %v", err)
	}

	// A different hostname runs hostnamectl, stubbed so the real host is untouched.
	fakeCommand(t, "hostnamectl", "exit 0")
	if err := ensureHostname("edgecommander-coverage-host"); err != nil {
		t.Fatalf("ensureHostname: %v", err)
	}
}

func TestDecodeFileContent(t *testing.T) {
	tests := []struct {
		name    string
		file    FileConfig
		want    string
		wantErr bool
	}{
		{"empty encoding is plain", FileConfig{Content: "Hello, world!\n"}, "Hello, world!\n", false},
		{"explicit plain", FileConfig{Content: "abc", Encoding: "plain"}, "abc", false},
		{"trims encoding whitespace", FileConfig{Content: "abc", Encoding: " plain "}, "abc", false},
		{"base64", FileConfig{Content: "SGVsbG8=", Encoding: "base64"}, "Hello", false},
		{"empty plain content", FileConfig{}, "", false},
		{"bad base64", FileConfig{Content: "not!base64", Encoding: "base64"}, "", true},
		{"unknown encoding", FileConfig{Content: "abc", Encoding: "rot13"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeFileContent(tt.file)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeFileContent error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && string(got) != tt.want {
				t.Fatalf("decodeFileContent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureHostnameApplyError(t *testing.T) {
	fakeCommand(t, "hostnamectl", "echo nope >&2; exit 1")
	if err := ensureHostname("edgecommander-coverage-host"); err == nil {
		t.Fatal("expected error when hostnamectl fails")
	}
}

func TestEnsureFiles(t *testing.T) {
	dir := t.TempDir()

	plainPath := filepath.Join(dir, "nested", "hello")
	files := []FileConfig{
		{Path: plainPath, Chmod: "0640", Content: "Hello, world!\n"},
		{Path: filepath.Join(dir, "raw"), Encoding: "base64", Content: "SGk="},
	}

	if err := ensureFiles(files); err != nil {
		t.Fatalf("ensureFiles: %v", err)
	}

	// Parent directories are created, content is written, and mode is exact.
	got, err := os.ReadFile(plainPath) //nolint:gosec // reads a temp-dir fixture the test just wrote.
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "Hello, world!\n" {
		t.Fatalf("content = %q, want %q", got, "Hello, world!\n")
	}
	info, err := os.Stat(plainPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "raw")) //nolint:gosec // reads a temp-dir fixture the test just wrote.
	if err != nil {
		t.Fatalf("read base64 file: %v", err)
	}
	if string(raw) != "Hi" {
		t.Fatalf("base64 content = %q, want %q", raw, "Hi")
	}

	// Rewriting is idempotent and overwrites prior content.
	files[0].Content = "changed\n"
	if err := ensureFiles(files[:1]); err != nil {
		t.Fatalf("ensureFiles rewrite: %v", err)
	}
	if got, _ := os.ReadFile(plainPath); string(got) != "changed\n" { //nolint:gosec // reads a temp-dir fixture the test just wrote.
		t.Fatalf("rewrite content = %q, want %q", got, "changed\n")
	}

	t.Run("missing path", func(t *testing.T) {
		if err := ensureFiles([]FileConfig{{Content: "x"}}); err == nil {
			t.Fatal("expected error for missing path")
		}
	})
}

func TestEnsureFoldersAppliesModeAndOwner(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b")
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	if err := ensureFolders([]FolderConfig{{Path: target, Chmod: "0700", User: owner}}); err != nil {
		t.Fatalf("ensureFolders: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestEnsureFolderErrors(t *testing.T) {
	cases := []struct {
		name   string
		folder FolderConfig
	}{
		{"missing path", FolderConfig{Chmod: "0755"}},
		{"bad chmod", FolderConfig{Path: filepath.Join(t.TempDir(), "x"), Chmod: "99999"}},
		{"bad user", FolderConfig{Path: filepath.Join(t.TempDir(), "y"), User: "no-such-user-xyz"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := ensureFolders([]FolderConfig{tt.folder}); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestParseOwnerVariants(t *testing.T) {
	if uid, gid, err := parseOwner("0:0"); err != nil || uid != 0 || gid != 0 {
		t.Fatalf("parseOwner(0:0) = %d,%d,%v", uid, gid, err)
	}
	if uid, _, err := parseOwner("root"); err != nil || uid != 0 {
		t.Fatalf("parseOwner(root) = %d,%v", uid, err)
	}
	if _, gid, err := parseOwner("0:root"); err != nil || gid != 0 {
		t.Fatalf("parseOwner(0:root) = %d,%v", gid, err)
	}
	for _, bad := range []string{"", ":0", "0:", "nobody-xyz", "0:nogroup-xyz"} {
		if _, _, err := parseOwner(bad); err == nil {
			t.Fatalf("parseOwner(%q) expected error", bad)
		}
	}
}
