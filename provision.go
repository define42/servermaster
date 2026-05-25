package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// ensureHostname sets the node's static hostname through hostnamectl when one is
// declared and it differs from the running hostname. hostnamectl writes
// /etc/hostname and updates the live hostname via systemd-hostnamed, so the
// change persists across reboots. An empty hostname leaves it unmanaged.
func ensureHostname(hostname string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil
	}

	current, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("read current hostname failed: %w", err)
	}
	if current == hostname {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), hostCommandTimeout)
	defer cancel()
	if err := runCommand(ctx, "hostnamectl", "set-hostname", hostname); err != nil {
		return fmt.Errorf("set hostname to %q failed: %w", hostname, err)
	}

	log.Printf("hostname set to %s", hostname)
	return nil
}

func ensureFolders(folders []FolderConfig) error {
	for i, folder := range folders {
		if err := ensureFolder(folder, labelOrIndex(folder.Path, i)); err != nil {
			return err
		}
	}
	return nil
}

func ensureFolder(folder FolderConfig, label string) error {
	if folder.Path == "" {
		return fmt.Errorf("folder %s is missing path", label)
	}

	mode, hasMode, err := parseOptionalMode(folder.Chmod, "folder", label)
	if err != nil {
		return err
	}

	uid, gid, err := parseOptionalOwner(folder.User, "folder", label)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(folder.Path, 0o755); err != nil { //nolint:gosec // operator-declared folder; 0755 lets non-root container users traverse it.
		return fmt.Errorf("create folder %q failed: %w", folder.Path, err)
	}

	if uid != -1 || gid != -1 {
		if err := os.Chown(folder.Path, uid, gid); err != nil {
			return fmt.Errorf("set owner for folder %q failed: %w", folder.Path, err)
		}
	}

	if hasMode {
		if err := os.Chmod(folder.Path, mode); err != nil {
			return fmt.Errorf("set chmod for folder %q failed: %w", folder.Path, err)
		}
	}
	return nil
}

// parseOptionalMode parses an optional chmod string. has reports whether a chmod
// was declared, so the caller only applies a mode when one was requested. kind
// ("folder"/"file") names the entry in the error message.
func parseOptionalMode(chmod, kind, label string) (mode os.FileMode, has bool, err error) {
	if chmod == "" {
		return 0, false, nil
	}
	mode, err = parseFileMode(chmod)
	if err != nil {
		return 0, false, fmt.Errorf("invalid chmod %q for %s %s: %w", chmod, kind, label, err)
	}
	return mode, true, nil
}

// parseOptionalOwner parses an optional "user[:group]" string, returning
// (-1, -1) when none is declared so the caller skips chown. kind ("folder"/
// "file") names the entry in the error message.
func parseOptionalOwner(user, kind, label string) (uid, gid int, err error) {
	if user == "" {
		return -1, -1, nil
	}
	uid, gid, err = parseOwner(user)
	if err != nil {
		return -1, -1, fmt.Errorf("invalid user %q for %s %s: %w", user, kind, label, err)
	}
	return uid, gid, nil
}

// ensureFiles writes each declared file to its path, creating parent directories
// as needed, then applies the requested owner and mode. Content is taken from
// the config literally ("plain") or base64-decoded ("base64"). Like ensureFolders
// it is idempotent: rewriting a file that already matches is harmless.
func ensureFiles(files []FileConfig) error {
	for i, file := range files {
		if err := ensureFile(file, labelOrIndex(file.Path, i)); err != nil {
			return err
		}
	}
	return nil
}

func ensureFile(file FileConfig, label string) error {
	if file.Path == "" {
		return fmt.Errorf("file %s is missing path", label)
	}

	content, err := decodeFileContent(file)
	if err != nil {
		return fmt.Errorf("decode content for file %s: %w", label, err)
	}

	mode, hasMode, err := parseOptionalMode(file.Chmod, "file", label)
	if err != nil {
		return err
	}
	if !hasMode {
		mode = os.FileMode(0o644)
	}

	uid, gid, err := parseOptionalOwner(file.User, "file", label)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(file.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // parent of an operator-declared file; 0755 lets non-root container users traverse it.
			return fmt.Errorf("create parent dir for file %q failed: %w", file.Path, err)
		}
	}

	// Write with the target mode, then Chmod to defeat the process umask so
	// the file lands at exactly the requested permissions.
	if err := os.WriteFile(file.Path, content, mode); err != nil {
		return fmt.Errorf("write file %q failed: %w", file.Path, err)
	}
	if err := os.Chmod(file.Path, mode); err != nil {
		return fmt.Errorf("set chmod for file %q failed: %w", file.Path, err)
	}

	if uid != -1 || gid != -1 {
		if err := os.Chown(file.Path, uid, gid); err != nil {
			return fmt.Errorf("set owner for file %q failed: %w", file.Path, err)
		}
	}
	return nil
}

// decodeFileContent returns the bytes to write for a file, interpreting its
// content according to the declared encoding. An empty encoding means "plain".
func decodeFileContent(file FileConfig) ([]byte, error) {
	switch strings.TrimSpace(file.Encoding) {
	case "", "plain":
		return []byte(file.Content), nil
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 content: %w", err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q (want \"plain\" or \"base64\")", file.Encoding)
	}
}

func parseFileMode(chmod string) (os.FileMode, error) {
	cleaned := strings.TrimSpace(chmod)
	if cleaned == "" {
		return 0, fmt.Errorf("empty mode")
	}

	value, err := strconv.ParseUint(cleaned, 8, 32)
	if err != nil {
		return 0, err
	}
	if value > 0o7777 {
		return 0, fmt.Errorf("mode exceeds 07777")
	}

	return os.FileMode(value), nil
}

func parseOwner(owner string) (int, int, error) {
	userPart, groupPart, hasGroup := strings.Cut(strings.TrimSpace(owner), ":")
	if userPart == "" {
		return -1, -1, fmt.Errorf("missing user")
	}

	uid, gid, err := parseUser(userPart)
	if err != nil {
		return -1, -1, err
	}

	if hasGroup {
		parsedGID, err := parseGroup(groupPart)
		if err != nil {
			return -1, -1, err
		}
		gid = parsedGID
	}

	return uid, gid, nil
}

func parseUser(value string) (int, int, error) {
	if uid, err := strconv.Atoi(value); err == nil {
		return uid, -1, nil
	}

	userInfo, err := osuser.Lookup(value)
	if err != nil {
		return -1, -1, err
	}

	uid, err := strconv.Atoi(userInfo.Uid)
	if err != nil {
		return -1, -1, fmt.Errorf("invalid uid %q for user %q: %w", userInfo.Uid, value, err)
	}

	gid, err := strconv.Atoi(userInfo.Gid)
	if err != nil {
		return -1, -1, fmt.Errorf("invalid gid %q for user %q: %w", userInfo.Gid, value, err)
	}

	return uid, gid, nil
}

func parseGroup(value string) (int, error) {
	if value == "" {
		return -1, fmt.Errorf("missing group")
	}

	if gid, err := strconv.Atoi(value); err == nil {
		return gid, nil
	}

	groupInfo, err := osuser.LookupGroup(value)
	if err != nil {
		return -1, err
	}

	gid, err := strconv.Atoi(groupInfo.Gid)
	if err != nil {
		return -1, fmt.Errorf("invalid gid %q for group %q: %w", groupInfo.Gid, value, err)
	}

	return gid, nil
}
