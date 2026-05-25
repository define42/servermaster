package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// defaultListenAddress is the web server's default bind address, used when
	// -listen is not given. See parseListenAddress for the accepted forms (a
	// host:port for TCP, or a "unix:" path for a Unix-domain socket).
	defaultListenAddress = ":8080"
	apiHealthPath        = "/servermaster/health"
	apiStatusPath        = "/servermaster/status"
	apiConfigPath        = "/servermaster/config"
	apiRestartPath       = "/servermaster/restart"
	apiOstreeUploadPath  = "/servermaster/ostree/upload"
	apiOstreeUpgradePath = "/servermaster/ostree/upgrade"

	// maxConfigUploadBytes caps the body accepted by /servermaster/config. A node
	// config is a small JSON document; the limit stops an unauthenticated caller
	// from streaming an arbitrarily large body into memory.
	maxConfigUploadBytes = 1 << 20 // 1 MiB

)

// scheduleReboot seams keep the concrete reboot path unit-testable without
// sleeping or invoking systemctl on the test host.
//
//nolint:gochecknoglobals // injectable seams for scheduleReboot tests.
var (
	rebootDelay       = time.Second
	rebootCommandFunc = runCommand
)

// rebootScheduler performs the delayed host reboot. Tests replace it so handler
// paths can be exercised without rebooting the machine.
//
//nolint:gochecknoglobals // injectable seam so rebooting endpoints are testable.
var rebootScheduler = scheduleReboot

func startWebServer(address string, configPath string) (*http.Server, <-chan error, error) {
	mux := http.NewServeMux()
	mux.HandleFunc(apiHealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "servermaster running")
	})
	mux.HandleFunc(apiStatusPath, func(w http.ResponseWriter, r *http.Request) {
		handleServermasterStatus(w, r, configPath)
	})
	mux.HandleFunc(apiConfigPath, func(w http.ResponseWriter, r *http.Request) {
		handleConfigUpload(w, r, configPath)
	})
	mux.HandleFunc(apiRestartPath, handleRestart)
	mux.HandleFunc(apiOstreeUploadPath, func(w http.ResponseWriter, r *http.Request) {
		handleOstreeUpload(w, r, configPath)
	})
	mux.HandleFunc(apiOstreeUpgradePath, func(w http.ResponseWriter, r *http.Request) {
		handleOstreeUpgrade(w, r, configPath)
	})

	listener, err := listen(address)
	if err != nil {
		return nil, nil, fmt.Errorf("start webserver on %s failed: %w", address, err)
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	log.Printf("webserver listening on %s", address)
	return server, errCh, nil
}

// unixAddressPrefix marks a -listen value as a Unix-domain socket path rather
// than a TCP host:port. "unix://" is the URL-style form (so "unix:///run/x.sock"
// yields the absolute path "/run/x.sock"); a bare "unix:" prefix is also
// accepted.
const unixAddressPrefix = "unix:"

// parseListenAddress splits a -listen value into a net.Listen network and
// address. A "unix:"-prefixed value selects a Unix-domain socket and the
// remainder is its filesystem path; anything else is a TCP host:port. An
// absolute socket path ("unix:///run/...") is recommended, since a relative one
// resolves against the process working directory.
func parseListenAddress(address string) (network, addr string) {
	if rest, ok := strings.CutPrefix(address, unixAddressPrefix); ok {
		// Tolerate the "unix://path" URL form by dropping the authority
		// separator, leaving the path. "unix:///abs" therefore yields "/abs".
		return "unix", strings.TrimPrefix(rest, "//")
	}
	return "tcp", address
}

// listen binds the web server's listener for a -listen value, dispatching to a
// Unix-domain socket or a TCP listener.
func listen(address string) (net.Listener, error) {
	network, addr := parseListenAddress(address)
	if network == "unix" {
		return listenUnix(addr)
	}
	return net.Listen(network, addr)
}

// listenUnix binds a Unix-domain socket at path. It creates the parent
// directory, clears a stale socket left by an earlier ungraceful exit (systemd
// SIGTERM kills the process without closing the listener, so the socket file can
// linger), and restricts the socket to its owner group — the API is
// root-equivalent, so the socket must not be world-accessible. A non-socket file
// at path is left untouched and reported, so a misconfigured path cannot clobber
// real data.
func listenUnix(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("empty unix socket path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // operator-owned socket dir; traversable so the socket path is reachable.
			return nil, fmt.Errorf("create socket dir %q: %w", dir, err)
		}
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to remove non-socket file at %q", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %q: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat socket %q: %w", path, err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // root-equivalent control socket: 0660 keeps it non-world-accessible while allowing a dedicated owner group to connect.
		_ = listener.Close()
		return nil, fmt.Errorf("set socket mode %q: %w", path, err)
	}
	return listener, nil
}

func handleServermasterStatus(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := servermasterStatusCollector(r.Context(), configPath)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(status); err != nil {
		log.Printf("write servermaster status failed: %v", err)
	}
}

// handleConfigUpload accepts a raw config.json document, validates it, writes
// it atomically to the active config path, and converges the host to it (the
// same reconcile that runs at startup). The validated body is what lands on
// disk, so a successful upload becomes the new source of truth. Like the
// /servermaster/ostree endpoints it is unauthenticated: anyone who can reach
// :8080 can rewrite the node's folders, interfaces, firewall, and containers.
func handleConfigUpload(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigUploadBytes))
	if err != nil {
		msg := fmt.Sprintf("read config: %v", err)
		log.Printf("config upload rejected: %s", msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		msg := fmt.Sprintf("parse config: %v", err)
		log.Printf("config upload rejected: %s", msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Validate before touching disk so a rejected upload never replaces the
	// config on disk and never partially applies.
	if err := validateConfig(&cfg); err != nil {
		msg := fmt.Sprintf("invalid config: %v", err)
		log.Printf("config upload rejected: %s", msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Hold applyMu across the write and the apply so a second upload, or the
	// startup reconcile, cannot interleave with this convergence.
	applyMu.Lock()
	defer applyMu.Unlock()

	if err := writeConfigFile(configPath, body); err != nil {
		msg := fmt.Sprintf("save config: %v", err)
		log.Printf("config upload failed: %s", msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	// The config is now persisted. If the apply fails (for example firewalld is
	// down) the saved config is still the desired state, so report the failure
	// but leave it on disk for the next reconcile to retry.
	if err := configApplier(&cfg); err != nil {
		log.Printf("apply uploaded config failed: %v", err)
		http.Error(w, fmt.Sprintf("config saved to %s but apply failed: %v", configPath, err), http.StatusInternalServerError)
		return
	}

	log.Printf("config uploaded and applied to %s (%d bytes)", configPath, len(body))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "config saved to %s and applied\n", configPath)
}

// handleRestart schedules a host reboot. It is unauthenticated: anyone who can
// reach :8080 can reboot the node.
func handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("restart requested; scheduling reboot")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "rebooting")
	go rebootScheduler()
}

// handleOstreeUpload streams the request body to the configured upload path.
// The body is written to a temp file in the destination directory and then
// renamed into place so a partial upload can never be applied. The endpoint is
// unauthenticated: anyone who can reach :8080 can replace the staged image.
func handleOstreeUpload(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()

	cfg, err := loadConfig(configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("load config: %v", err), http.StatusInternalServerError)
		return
	}

	dest := ostreeUploadPath(cfg)
	written, err := streamToFileAtomic(dest, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("ostree image uploaded to %s (%d bytes)", dest, written)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "uploaded %d bytes to %s\n", written, dest)
}

// handleOstreeUpgrade runs the configured apply command and, unless the request
// sets ?reboot=false, reboots the host once the command succeeds. The reboot is
// scheduled after the response is written so the caller gets confirmation. Like
// the /servermaster/ostree/upload endpoint this is unauthenticated.
func handleOstreeUpgrade(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("load config: %v", err), http.StatusInternalServerError)
		return
	}

	if cfg.Ostree == nil || len(cfg.Ostree.ApplyCommand) == 0 {
		http.Error(w, "no ostree.apply_command configured", http.StatusBadRequest)
		return
	}

	reboot := r.URL.Query().Get("reboot") != "false"

	command := cfg.Ostree.ApplyCommand
	log.Printf("applying ostree update: %s", strings.Join(command, " "))
	// Bound the apply off context.Background(), not the request context, so it
	// runs to completion even if the caller disconnects, while still being capped
	// so a wedged apply cannot block the handler forever.
	ctx, cancel := context.WithTimeout(context.Background(), ostreeApplyTimeout)
	defer cancel()
	if err := runCommand(ctx, command[0], command[1:]...); err != nil {
		log.Printf("ostree apply failed: %v", err)
		http.Error(w, fmt.Sprintf("apply update failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !reboot {
		log.Println("ostree update applied; reboot skipped")
		_, _ = fmt.Fprintln(w, "update applied; reboot skipped")
		return
	}

	log.Println("ostree update applied; scheduling reboot")
	_, _ = fmt.Fprintln(w, "update applied; rebooting")
	go rebootScheduler()
}

// scheduleReboot reboots the host after a short grace period so the HTTP
// response can be flushed to the caller first.
func scheduleReboot() {
	time.Sleep(rebootDelay)
	ctx, cancel := context.WithTimeout(context.Background(), hostCommandTimeout)
	defer cancel()
	if err := rebootCommandFunc(ctx, "systemctl", "reboot"); err != nil {
		log.Printf("reboot failed: %v", err)
	}
}
