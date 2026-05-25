package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

	// Only the /servermaster/* namespace is registered.
	assertGet(t, "http://"+addr+"/", http.StatusNotFound, "")
	assertGet(t, "http://"+addr+"/nope", http.StatusNotFound, "")
	assertGet(t, "http://"+addr+apiHealthPath, http.StatusOK, "servermaster running")

	// Each endpoint's wrong-method path is a cheap way to exercise its
	// registration without invoking the heavy collectors.
	assertStatus(t, http.MethodPost, "http://"+addr+apiStatusPath, http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+apiConfigPath, http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+apiRestartPath, http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+apiOstreeUploadPath, http.StatusMethodNotAllowed)
	assertStatus(t, http.MethodGet, "http://"+addr+apiOstreeUpgradePath, http.StatusMethodNotAllowed)

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

func TestParseListenAddress(t *testing.T) {
	cases := []struct {
		in          string
		wantNetwork string
		wantAddr    string
	}{
		{":8080", "tcp", ":8080"},
		{"127.0.0.1:8080", "tcp", "127.0.0.1:8080"},
		{"0.0.0.0:9000", "tcp", "0.0.0.0:9000"},
		{"unix:///run/servermaster/servermaster.sock", "unix", "/run/servermaster/servermaster.sock"},
		{"unix:/run/x.sock", "unix", "/run/x.sock"},
		{"unix://run/x.sock", "unix", "run/x.sock"},
	}
	for _, tc := range cases {
		network, addr := parseListenAddress(tc.in)
		if network != tc.wantNetwork || addr != tc.wantAddr {
			t.Errorf("parseListenAddress(%q) = (%q, %q), want (%q, %q)", tc.in, network, addr, tc.wantNetwork, tc.wantAddr)
		}
	}
}

func TestStartWebServerUnixSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "servermaster.sock")
	cfgPath := writeTempConfig(t, `{}`)

	// "unix://" + an absolute path is the documented URL form.
	server, errCh, err := startWebServer("unix://"+socketPath, cfgPath)
	if err != nil {
		t.Fatalf("startWebServer: %v", err)
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a socket at %s, got mode %v", socketPath, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Fatalf("socket mode = %#o, want 0660 (not world-accessible)", perm)
	}

	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://unix" + apiHealthPath)
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health over unix socket = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("serve returned error: %v", err)
	}
	// A graceful close unlinks the socket so the next start binds cleanly.
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("expected socket removed after close, stat err = %v", err)
	}
}

func TestListenUnixStaleSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "stale.sock")

	// Simulate the socket an ungraceful exit (SIGTERM) leaves behind: bind it,
	// then close without unlinking so the file lingers.
	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("pre-bind stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected stale socket to remain: %v", err)
	}

	listener, err := listen("unix://" + socketPath)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	_ = listener.Close()
}

func TestListenUnixNonSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real-data")
	if err := os.WriteFile(path, []byte("important"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := listen("unix://" + path); err == nil {
		t.Fatal("expected error rather than clobbering a non-socket file")
	}
	// The regular file must be left untouched.
	if data, err := os.ReadFile(path); err != nil || string(data) != "important" { //nolint:gosec // test reads a file it just wrote under t.TempDir.
		t.Fatalf("non-socket file was modified: data=%q err=%v", data, err)
	}
}

// unixHTTPClient returns an HTTP client that dials the given Unix-domain socket
// regardless of the request URL's host.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
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

func TestHandleConfigUploadTooLarge(t *testing.T) {
	big := strings.Repeat("a", (1<<20)+16)
	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(big))
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
	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(validConfigUploadBody))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleOstreeUploadLoadConfigError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, apiOstreeUploadPath, strings.NewReader("data"))
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
	req := httptest.NewRequest(http.MethodPost, apiOstreeUploadPath, strings.NewReader("data"))
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, cfgPath)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleOstreeUpgradeLoadConfigError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, apiOstreeUpgradePath, nil)
	rec := httptest.NewRecorder()
	handleOstreeUpgrade(rec, req, filepath.Join(t.TempDir(), "missing.json"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestListenUnixEarlyErrors(t *testing.T) {
	if _, err := listen("unix:"); err == nil {
		t.Fatal("expected empty unix socket path error")
	}

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, err := listen("unix://" + filepath.Join(blocker, "sock")); err == nil {
		t.Fatal("expected socket directory creation error")
	}
}

func stubServermasterStatusCollector(fn func(context.Context, string) servermasterStatus) func() {
	prev := servermasterStatusCollector
	servermasterStatusCollector = fn
	return func() { servermasterStatusCollector = prev }
}

func stubRebootScheduler(fn func()) func() {
	prev := rebootScheduler
	rebootScheduler = fn
	return func() { rebootScheduler = prev }
}

func TestHandleServermasterStatusMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, apiStatusPath, nil)
	rec := httptest.NewRecorder()
	handleServermasterStatus(rec, req, "unused")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// sampleServermasterStatus is a fully populated status document used to exercise
// the /servermaster/status handler's encoding.
func sampleServermasterStatus() servermasterStatus {
	return servermasterStatus{
		Status:      "ok",
		GeneratedAt: "2026-05-24T12:00:00Z",
		Ostree:      ostreeStatus{Source: "test", Version: "1.2.3", Booted: true},
		FreeDiskSpace: []diskStatus{{
			Path:           "/",
			TotalBytes:     100,
			FreeBytes:      40,
			AvailableBytes: 30,
			UsedBytes:      60,
			UsedPercent:    60,
		}},
		Network: networkStatus{
			Source: "netlink",
			Interfaces: []networkInterface{{
				Name:      "eth0",
				Index:     2,
				Type:      "device",
				State:     "up",
				Addresses: []networkAddress{{IP: "192.168.1.10", PrefixLength: 24, Family: "ipv4"}},
			}},
			DNS: []string{"1.1.1.1"},
		},
		Containers: []runningContainerStatus{{
			ID:      "abc123",
			Name:    "web",
			State:   "running",
			Image:   "docker.io/library/nginx:1.25",
			Version: "1.25",
			Logs:    []string{"stdout: ready"},
		}},
	}
}

func TestHandleServermasterStatusPrettyJSON(t *testing.T) {
	defer stubServermasterStatusCollector(func(context.Context, string) servermasterStatus {
		return sampleServermasterStatus()
	})()

	req := httptest.NewRequest(http.MethodGet, apiStatusPath, nil)
	rec := httptest.NewRecorder()
	handleServermasterStatus(rec, req, "unused")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if !strings.HasPrefix(rec.Body.String(), "{\n  ") {
		t.Fatalf("response is not pretty-printed JSON: %q", rec.Body.String())
	}

	var got servermasterStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != "ok" || got.Ostree.Version != "1.2.3" || len(got.Containers) != 1 {
		t.Fatalf("unexpected status document: %+v", got)
	}
	if len(got.Network.Interfaces) != 1 || got.Network.Interfaces[0].Name != "eth0" {
		t.Fatalf("unexpected network document: %+v", got.Network)
	}
	if got.Containers[0].Logs[0] != "stdout: ready" {
		t.Fatalf("logs = %v, want stdout line", got.Containers[0].Logs)
	}
}

// validConfigUploadBody is a minimal valid /servermaster/config request body
// shared by the upload tests.
const validConfigUploadBody = `{"containers":[{"name":"web","image":"nginx","ports":[{"host_port":8081,"container_port":80}]}]}`

func TestHandleConfigUploadMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, apiConfigPath, nil)
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, "unused")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleConfigUploadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "containers.json")
	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file must not be created on parse failure")
	}
}

func TestHandleConfigUploadInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "containers.json")
	applied := false
	defer stubConfigApplier(func(*Config) error { applied = true; return nil })()

	body := `{"firewall_ports":[{"port":"70000"}]}`
	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if applied {
		t.Fatalf("invalid config must not be applied")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid config must not be written")
	}
}

func TestHandleConfigUploadRejectionLogged(t *testing.T) {
	ring := newLogRing(servermasterLogTail)
	orig := log.Writer()
	log.SetOutput(ring)
	defer log.SetOutput(orig)

	path := filepath.Join(t.TempDir(), "containers.json")
	body := `{"interfaces":[{"name":"dummy0","ip_address":"192.168.1.10"}]}`
	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	found := false
	for _, line := range ring.snapshot() {
		if strings.Contains(line, "invalid config") && strings.Contains(line, "dummy0") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("rejection was not logged; ring = %v", ring.snapshot())
	}
}

func TestHandleConfigUploadValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "containers.json")
	var appliedCfg *Config
	defer stubConfigApplier(func(c *Config) error { appliedCfg = c; return nil })()

	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(validConfigUploadBody))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if appliedCfg == nil || len(appliedCfg.Containers) != 1 || appliedCfg.Containers[0].Name != "web" {
		t.Fatalf("apply received unexpected config: %+v", appliedCfg)
	}
	got, err := os.ReadFile(path) //nolint:gosec // reads a temp-dir fixture the test just wrote.
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if string(got) != validConfigUploadBody {
		t.Fatalf("saved config = %q, want the uploaded body verbatim", got)
	}
}

func TestHandleConfigUploadApplyFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "containers.json")
	defer stubConfigApplier(func(*Config) error { return fmt.Errorf("firewalld down") })()

	req := httptest.NewRequest(http.MethodPost, apiConfigPath, strings.NewReader(validConfigUploadBody))
	rec := httptest.NewRecorder()
	handleConfigUpload(rec, req, path)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config must remain saved after an apply failure: %v", err)
	}
}

func TestHandleRestart(t *testing.T) {
	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, apiRestartPath, nil)
		rec := httptest.NewRecorder()
		handleRestart(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("post schedules reboot", func(t *testing.T) {
		called := make(chan struct{}, 1)
		defer stubRebootScheduler(func() { called <- struct{}{} })()

		req := httptest.NewRequest(http.MethodPost, apiRestartPath, nil)
		rec := httptest.NewRecorder()
		handleRestart(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}

		select {
		case <-called:
		case <-time.After(time.Second):
			t.Fatal("reboot was not scheduled")
		}
	})
}

func TestHandleOstreeUpload(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "images", "update.tar") // nested dir must be created
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := fmt.Sprintf(`{"ostree":{"upload_path":%q}}`, dest)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	body := "fake-tar-contents"
	req := httptest.NewRequest(http.MethodPost, apiOstreeUploadPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, cfgPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(dest) //nolint:gosec // reads a temp-dir fixture the test just wrote.
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != body {
		t.Fatalf("dest contents = %q, want %q", got, body)
	}
}

func TestHandleOstreeUploadMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, apiOstreeUploadPath, nil)
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, "unused")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleOstreeUpgrade(t *testing.T) {
	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, apiOstreeUpgradePath, nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, "unused")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("no apply command", func(t *testing.T) {
		cfgPath := writeTempConfig(t, `{}`)
		req := httptest.NewRequest(http.MethodPost, apiOstreeUpgradePath, nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, cfgPath)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	// reboot=false keeps the reboot path from firing during the test.
	t.Run("apply succeeds without reboot", func(t *testing.T) {
		cfgPath := writeTempConfig(t, `{"ostree":{"apply_command":["true"]}}`)
		req := httptest.NewRequest(http.MethodPost, apiOstreeUpgradePath+"?reboot=false", nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, cfgPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "reboot skipped") {
			t.Fatalf("body = %q, want it to mention reboot skipped", rec.Body.String())
		}
	})

	t.Run("apply fails", func(t *testing.T) {
		cfgPath := writeTempConfig(t, `{"ostree":{"apply_command":["false"]}}`)
		req := httptest.NewRequest(http.MethodPost, apiOstreeUpgradePath+"?reboot=false", nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, cfgPath)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
	})
}
