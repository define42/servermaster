package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestStartWebServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfgPath := writeTempConfig(t, `{}`)
	server, errCh, err := startWebServer(addr, cfgPath)
	if err != nil {
		t.Fatalf("startWebServer: %v", err)
	}

	// "/" serves the running banner; an unknown path under "/" is a 404.
	assertGet(t, "http://"+addr+"/", http.StatusOK, "servermaster running")
	assertGet(t, "http://"+addr+"/nope", http.StatusNotFound, "")

	// Each endpoint's wrong-method path is a cheap way to exercise its
	// registration without invoking the heavy collectors.
	assertStatus(t, http.MethodPost, "http://"+addr+"/servermaster", http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+"/config", http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+"/ostree/upload", http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+"/ostree/upgrade", http.StatusMethodNotAllowed)

	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("serve returned error: %v", err)
	}
}

func TestStartWebServerBadAddress(t *testing.T) {
	if _, _, err := startWebServer("256.256.256.256:99999", "unused"); err == nil {
		t.Fatal("expected error for an unbindable address")
	}
}

func assertGet(t *testing.T, url string, wantCode int, wantBody string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantCode {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, wantCode)
	}
	if wantBody != "" {
		body, _ := readAll(resp)
		if !strings.Contains(body, wantBody) {
			t.Fatalf("GET %s body = %q, want it to contain %q", url, body, wantBody)
		}
	}
}

func assertStatus(t *testing.T, method, url string, wantCode int) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantCode {
		t.Fatalf("%s %s = %d, want %d", method, url, resp.StatusCode, wantCode)
	}
}

func readAll(resp *http.Response) (string, error) {
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}

func TestWaitForUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	if err := waitForUnixSocket(sock, time.Second); err != nil {
		t.Fatalf("reachable socket: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.sock")
	if err := waitForUnixSocket(missing, 150*time.Millisecond); err == nil {
		t.Fatal("expected timeout error for absent socket")
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

func TestValidateConfigOwnerErrors(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "folder bad user", cfg: &Config{Folders: []FolderConfig{{Path: "/d", User: "no-user-xyz"}}}, want: "user"},
		{name: "file bad user", cfg: &Config{Files: []FileConfig{{Path: "/d/f", User: "no-user-xyz"}}}, want: "user"},
	})
}

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

func TestHandleConfigUploadTooLarge(t *testing.T) {
	big := strings.Repeat("a", (1<<20)+16)
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(big))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, filepath.Join(t.TempDir(), "c.json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleConfigUploadSaveError(t *testing.T) {
	defer stubConfigApplier(func(*Config) error { return nil })()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	// The config path's parent is a regular file, so the atomic write fails
	// after validation passes.
	path := filepath.Join(blocker, "sub", "config.json")
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(validConfigUploadBody))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleOstreeUploadLoadConfigError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ostree/upload", strings.NewReader("data"))
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, filepath.Join(t.TempDir(), "missing.json"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleOstreeUploadMkdirError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	// upload_path lives under a regular file, so creating its dir must fail.
	cfgPath := writeTempConfig(t, fmt.Sprintf(`{"ostree":{"upload_path":%q}}`, filepath.Join(blocker, "sub", "update.tar")))
	req := httptest.NewRequest(http.MethodPost, "/ostree/upload", strings.NewReader("data"))
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, cfgPath)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleOstreeUpgradeLoadConfigError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ostree/upgrade", nil)
	rec := httptest.NewRecorder()
	handleOstreeUpgrade(rec, req, filepath.Join(t.TempDir(), "missing.json"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestConfigureHostInterfaces(t *testing.T) {
	if err := configureHostInterfaces(nil); err != nil {
		t.Fatalf("empty interfaces should be a no-op: %v", err)
	}

	prev := nmstateStatePath
	nmstateStatePath = filepath.Join(t.TempDir(), "nmstate", "state.yml")
	defer func() { nmstateStatePath = prev }()
	fakeCommand(t, "nmstatectl", "exit 0")

	ifaces := []InterfaceConfig{{Name: "eth0", IPAddress: "10.0.0.2", Subnet: "10.0.0.0/24"}}
	if err := configureHostInterfaces(ifaces); err != nil {
		t.Fatalf("configureHostInterfaces: %v", err)
	}
	if _, err := os.Stat(nmstateStatePath); err != nil {
		t.Fatalf("desired-state file not written: %v", err)
	}

	if err := configureHostInterfaces([]InterfaceConfig{{Name: ""}}); err == nil {
		t.Fatal("expected error for an interface with no name")
	}
}

func TestConfigureHostInterfacesApplyFails(t *testing.T) {
	prev := nmstateStatePath
	nmstateStatePath = filepath.Join(t.TempDir(), "state.yml")
	defer func() { nmstateStatePath = prev }()
	fakeCommand(t, "nmstatectl", "echo nope >&2; exit 1")

	err := configureHostInterfaces([]InterfaceConfig{{Name: "eth0"}})
	if err == nil || !strings.Contains(err.Error(), "apply host interface configuration failed") {
		t.Fatalf("err = %v, want apply failure", err)
	}
}

func TestCollectOstreeStatusSuccess(t *testing.T) {
	fakeCommand(t, "rpm-ostree", `echo '{"deployments":[{"booted":true,"checksum":"abc123"}]}'`)
	st, err := collectOstreeStatus(context.Background())
	if err != nil {
		t.Fatalf("collectOstreeStatus: %v", err)
	}
	if st.Checksum != "abc123" || !st.Booted {
		t.Fatalf("status = %+v", st)
	}
}
