package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// syncDir fsyncs a directory so a rename into it is durable. os.Rename is atomic
// — a reader always sees either the old file or the new one, never a partial one
// — but the directory entry the rename creates is not persisted to disk until
// the directory itself is flushed. Without this, a power loss (the expected
// failure mode on edge hardware) right after a rename can lose the rename or
// surface a zero-length file, even when the file's own contents were fsynced.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is an operator-declared destination directory, not request input.
	if err != nil {
		return fmt.Errorf("open dir %q for fsync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("fsync dir %q: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close dir %q after fsync: %w", dir, err)
	}
	return nil
}

// streamToFileAtomic streams r into a temp file beside dest, fsyncs it, and
// renames it onto dest, fsyncing the directory afterward so the result survives
// a power loss and never a partial stream. It returns the number of bytes
// written. Errors are wrapped with the stage that failed.
func streamToFileAtomic(dest string, r io.Reader) (int64, error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // operator-owned staging dir; traversable by design on this single-tenant node.
		return 0, fmt.Errorf("create upload dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // best-effort; a no-op once the rename succeeds

	written, copyErr := io.Copy(tmp, r)
	// fsync the staged file before the rename so a power loss right after it
	// cannot later surface a truncated file.
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return written, fmt.Errorf("write upload: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("close upload: %w", closeErr)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return written, fmt.Errorf("finalize upload: %w", err)
	}
	// fsync the directory so the rename itself is durable across a power loss.
	if err := syncDir(dir); err != nil {
		return written, fmt.Errorf("finalize upload: %w", err)
	}
	return written, nil
}

// writeConfigFile writes the config body to a temp file in the destination
// directory and renames it into place, so a crash mid-write can never leave a
// truncated config where the next boot would load it. The temp file and the
// destination directory are fsynced around the rename so the new config also
// survives a power loss, not just a clean crash.
func writeConfigFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // operator-owned config dir; traversable by design on this single-tenant node.
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // best-effort; a no-op once the rename succeeds

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	// config is intentionally world-readable for operator inspection on the node.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config mode: %w", err)
	}
	// fsync the contents (and mode) before the rename so a power loss right after
	// it cannot surface a zero-length or stale config on the next boot.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("finalize config: %w", err)
	}

	return syncDir(dir)
}
