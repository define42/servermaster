package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
)

const (
	defaultConfigPath       = "/data/config/containers.json"
	webServerAddress        = ":8080"
	defaultOstreeUploadPath = "/data/ostree/update.tar"
	podmanRootfulMode       = "rootful"
	podmanSocketPath        = "/run/podman/podman.sock"
	servermasterLogTail     = 100
	statusCommandTimeout    = 5 * time.Second

	// maxConfigUploadBytes caps the body accepted by /config. A node config is
	// a small JSON document; the limit stops an unauthenticated caller from
	// streaming an arbitrarily large body into memory.
	maxConfigUploadBytes = 1 << 20 // 1 MiB

	// nmstateStatePath is where the generated nmstate desired-state document is
	// written before it is applied. The .yml extension (JSON is valid YAML) lets
	// nmstate.service reapply it at boot in addition to the apply call below.
	nmstateStatePath = "/etc/nmstate/servermaster.yml"

	firewalldBusName       = "org.fedoraproject.FirewallD1"
	firewalldObjectPath    = "/org/fedoraproject/FirewallD1"
	firewalldZoneInterface = "org.fedoraproject.FirewallD1.zone"

	// Permanent configuration lives behind the config interface, addressed by
	// an explicit zone name, and survives a firewalld reload and reboot.
	firewalldConfigPath          = "/org/fedoraproject/FirewallD1/config"
	firewalldConfigInterface     = "org.fedoraproject.FirewallD1.config"
	firewalldConfigZoneInterface = "org.fedoraproject.FirewallD1.config.zone"
)

// applyMu serializes host convergence so the startup reconcile and concurrent
// /config uploads cannot interleave changes to folders, interfaces, firewall,
// or containers. Callers hold it across the whole apply.
var applyMu sync.Mutex

// configApplier converges the host to a parsed config. It is a package variable
// so tests can substitute the host-mutating apply; production uses applyConfig.
var configApplier = applyConfig

// servermasterStatusCollector gathers the /servermaster response. Tests replace
// it so the handler can be exercised without requiring Podman or ostree.
var servermasterStatusCollector = collectServermasterStatus

type Config struct {
	PodmanMode    string               `json:"podman_mode"`
	Folders       []FolderConfig       `json:"folders"`
	Interfaces    []InterfaceConfig    `json:"interfaces"`
	FirewallPorts []FirewallPortConfig `json:"firewall_ports"`
	Containers    []ContainerConfig    `json:"containers"`
	Ostree        *OstreeConfig        `json:"ostree,omitempty"`
}

type OstreeConfig struct {
	UploadPath   string   `json:"upload_path"`
	ApplyCommand []string `json:"apply_command"`
}

type FolderConfig struct {
	Path  string `json:"path"`
	Chmod string `json:"chmod"`
	User  string `json:"user"`
}

type ContainerConfig struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	User       string            `json:"user"`
	Env        map[string]string `json:"env"`
	Ports      []PortConfig      `json:"ports"`
	Volumes    []VolumeConfig    `json:"volumes"`
	Interfaces []InterfaceConfig `json:"interfaces"`
	Command    []string          `json:"command"`
	Restart    string            `json:"restart"`
}

type InterfaceConfig struct {
	Name      string   `json:"name"`
	IPAddress string   `json:"ip_address"`
	Subnet    string   `json:"subnet"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
}

type FirewallPortConfig struct {
	Zone     string `json:"zone"`
	Port     string `json:"port"`
	Protocol string `json:"protocol"`
}

type PortConfig struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type VolumeConfig struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only"`
	// SELinux selects the Podman relabel option for the bind mount: "z" for a
	// shared label (multiple containers may use the source) or "Z" for a
	// private label. Required on SELinux-enforcing hosts (Red Hat Device Edge
	// defaults to enforcing) or the container is denied access to the source.
	SELinux string `json:"selinux"`
}

type containerSpec struct {
	Name          string            `json:"name,omitempty"`
	Image         string            `json:"image"`
	User          string            `json:"user,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Command       []string          `json:"command,omitempty"`
	PortMappings  []portMapping     `json:"portmappings,omitempty"`
	Mounts        []mount           `json:"mounts,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
}

type portMapping struct {
	HostIP        string `json:"host_ip"`
	ContainerPort uint16 `json:"container_port"`
	HostPort      uint16 `json:"host_port"`
	Range         uint16 `json:"range"`
	Protocol      string `json:"protocol"`
}

type mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type imagePullReport struct {
	Stream string   `json:"stream"`
	Error  string   `json:"error"`
	Images []string `json:"images"`
	ID     string   `json:"id"`
}

type listedContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
}

type servermasterStatus struct {
	Status        string                   `json:"status"`
	GeneratedAt   string                   `json:"generated_at"`
	Ostree        ostreeStatus             `json:"ostree"`
	FreeDiskSpace []diskStatus             `json:"free_diskspace"`
	Containers    []runningContainerStatus `json:"containers"`
	Errors        []string                 `json:"errors,omitempty"`
}

type ostreeStatus struct {
	Source     string `json:"source,omitempty"`
	Version    string `json:"version,omitempty"`
	Checksum   string `json:"checksum,omitempty"`
	Image      string `json:"image,omitempty"`
	Booted     bool   `json:"booted"`
	Deployment string `json:"deployment,omitempty"`
	Error      string `json:"error,omitempty"`
}

type diskStatus struct {
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"total_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type runningContainerStatus struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Names       []string `json:"names,omitempty"`
	State       string   `json:"state"`
	Image       string   `json:"image,omitempty"`
	ImageID     string   `json:"image_id,omitempty"`
	ImageDigest string   `json:"image_digest,omitempty"`
	Version     string   `json:"version,omitempty"`
	Logs        []string `json:"logs"`
	Error       string   `json:"error,omitempty"`
}

type containerInspectResponse struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	Image       string `json:"Image"`
	ImageName   string `json:"ImageName"`
	ImageDigest string `json:"ImageDigest"`
}

type rpmOstreeStatus struct {
	Deployments []rpmOstreeDeployment `json:"deployments"`
}

type rpmOstreeDeployment struct {
	Booted                  bool           `json:"booted"`
	Version                 string         `json:"version"`
	Checksum                string         `json:"checksum"`
	BaseCommit              string         `json:"base-commit"`
	ContainerImageReference string         `json:"container-image-reference"`
	Origin                  string         `json:"origin"`
	BaseCommitMeta          map[string]any `json:"base-commit-meta"`
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to config JSON file")
	flag.Parse()

	if err := runService(*configPath); err != nil {
		log.Fatal(err)
	}
}

func runService(configPath string) error {
	_, webServerErrors, err := startWebServer(webServerAddress, configPath)
	if err != nil {
		return err
	}

	// A reconcile failure is logged but does not exit the process: the unit
	// is configured with Restart=always/RestartSec=10s, so returning here
	// would tear down and recreate every container on a tight crash loop.
	// The web server stays up so the host remains observable. applyMu is held
	// so an early /config upload cannot race the startup convergence.
	applyMu.Lock()
	runErr := run(configPath)
	applyMu.Unlock()
	if runErr != nil {
		log.Printf("reconcile failed: %v", runErr)
	}

	if err := <-webServerErrors; err != nil {
		return err
	}

	return nil
}

func startWebServer(address string, configPath string) (*http.Server, <-chan error, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "servermaster running")
	})
	mux.HandleFunc("/servermaster", func(w http.ResponseWriter, r *http.Request) {
		handleServermasterStatus(w, r, configPath)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		handleConfigUpload(w, r, configPath)
	})
	mux.HandleFunc("/ostree/upload", func(w http.ResponseWriter, r *http.Request) {
		handleOstreeUpload(w, r, configPath)
	})
	mux.HandleFunc("/ostree/upgrade", func(w http.ResponseWriter, r *http.Request) {
		handleOstreeUpgrade(w, r, configPath)
	})

	listener, err := net.Listen("tcp", address)
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

func collectServermasterStatus(ctx context.Context, configPath string) servermasterStatus {
	status := servermasterStatus{
		Status:      "ok",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("load config: %v", err))
	}

	ostree, err := collectOstreeStatus(ctx)
	if err != nil {
		ostree.Error = err.Error()
		status.Errors = append(status.Errors, fmt.Sprintf("ostree: %v", err))
	}
	status.Ostree = ostree

	diskPaths := servermasterDiskPaths(configPath, cfg)
	disks, diskErrors := collectDiskStatuses(diskPaths)
	status.FreeDiskSpace = disks
	for _, err := range diskErrors {
		status.Errors = append(status.Errors, fmt.Sprintf("disk: %v", err))
	}

	containers, err := collectRunningContainerStatuses(ctx, servermasterLogTail)
	if err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("containers: %v", err))
	}
	status.Containers = containers

	for _, container := range containers {
		if container.Error != "" {
			status.Errors = append(status.Errors, fmt.Sprintf("container %s: %s", container.Name, container.Error))
		}
	}

	if len(status.Errors) > 0 {
		status.Status = "degraded"
	}

	return status
}

func servermasterDiskPaths(configPath string, cfg *Config) []string {
	paths := []string{"/", "/data"}
	if configPath != "" {
		paths = append(paths, filepath.Dir(configPath))
	}
	if cfg != nil {
		paths = append(paths, filepath.Dir(ostreeUploadPath(cfg)))
	}
	return paths
}

func collectDiskStatuses(paths []string) ([]diskStatus, []error) {
	var statuses []diskStatus
	var errs []error
	seen := make(map[string]struct{})

	for _, path := range paths {
		statPath := nearestExistingPath(path)
		if statPath == "" {
			continue
		}
		if _, exists := seen[statPath]; exists {
			continue
		}
		seen[statPath] = struct{}{}

		status, err := diskStatusForPath(statPath)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		statuses = append(statuses, status)
	}

	return statuses, errs
}

func nearestExistingPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}

	candidate := filepath.Clean(path)
	for {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return ""
		}
		candidate = parent
	}
}

func diskStatusForPath(path string) (diskStatus, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskStatus{}, fmt.Errorf("%s: %w", path, err)
	}

	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bfree * blockSize
	available := stat.Bavail * blockSize
	used := uint64(0)
	if total > free {
		used = total - free
	}

	usedPercent := 0.0
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}

	return diskStatus{
		Path:           path,
		TotalBytes:     total,
		FreeBytes:      free,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    usedPercent,
	}, nil
}

// handleConfigUpload accepts a raw config.json document, validates it, writes
// it atomically to the active config path, and converges the host to it (the
// same reconcile that runs at startup). The validated body is what lands on
// disk, so a successful upload becomes the new source of truth. Like the ostree
// endpoints it is unauthenticated: anyone who can reach :8080 can rewrite the
// node's folders, interfaces, firewall, and containers.
func handleConfigUpload(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigUploadBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("read config: %v", err), http.StatusBadRequest)
		return
	}

	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		http.Error(w, fmt.Sprintf("parse config: %v", err), http.StatusBadRequest)
		return
	}

	// Validate before touching disk so a rejected upload never replaces the
	// config on disk and never partially applies.
	if err := validateConfig(&cfg); err != nil {
		http.Error(w, fmt.Sprintf("invalid config: %v", err), http.StatusBadRequest)
		return
	}

	// Hold applyMu across the write and the apply so a second upload, or the
	// startup reconcile, cannot interleave with this convergence.
	applyMu.Lock()
	defer applyMu.Unlock()

	if err := writeConfigFile(configPath, body); err != nil {
		http.Error(w, fmt.Sprintf("save config: %v", err), http.StatusInternalServerError)
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

// writeConfigFile writes the config body to a temp file in the destination
// directory and renames it into place, so a crash mid-write can never leave a
// truncated config where the next boot would load it.
func writeConfigFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // best-effort; a no-op once the rename succeeds

	_, writeErr := tmp.Write(body)
	closeErr := tmp.Close()
	if writeErr != nil {
		return fmt.Errorf("write config: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close config: %w", closeErr)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("set config mode: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("finalize config: %w", err)
	}

	return nil
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
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("create upload dir: %v", err), http.StatusInternalServerError)
		return
	}

	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		http.Error(w, fmt.Sprintf("create temp file: %v", err), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // best-effort; a no-op once the rename succeeds

	written, copyErr := io.Copy(tmp, r.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		http.Error(w, fmt.Sprintf("write upload: %v", copyErr), http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		http.Error(w, fmt.Sprintf("close upload: %v", closeErr), http.StatusInternalServerError)
		return
	}

	if err := os.Rename(tmpName, dest); err != nil {
		http.Error(w, fmt.Sprintf("finalize upload: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("ostree image uploaded to %s (%d bytes)", dest, written)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "uploaded %d bytes to %s\n", written, dest)
}

// handleOstreeUpgrade runs the configured apply command and, unless the request
// sets ?reboot=false, reboots the host once the command succeeds. The reboot is
// scheduled after the response is written so the caller gets confirmation. Like
// the upload endpoint this is unauthenticated.
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
	if err := runCommand(command[0], command[1:]...); err != nil {
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
	go scheduleReboot()
}

func ostreeUploadPath(cfg *Config) string {
	if cfg != nil && cfg.Ostree != nil {
		if path := strings.TrimSpace(cfg.Ostree.UploadPath); path != "" {
			return path
		}
	}
	return defaultOstreeUploadPath
}

func collectOstreeStatus(ctx context.Context) (ostreeStatus, error) {
	var attempts []error

	if output, err := runStatusCommand(ctx, "rpm-ostree", "status", "--json"); err == nil {
		status, parseErr := parseRPMOstreeStatus(output)
		if parseErr == nil {
			return status, nil
		}
		attempts = append(attempts, parseErr)
	} else {
		attempts = append(attempts, err)
	}

	if output, err := runStatusCommand(ctx, "bootc", "status", "--json"); err == nil {
		status, parseErr := parseBootcStatus(output)
		if parseErr == nil {
			return status, nil
		}
		attempts = append(attempts, parseErr)
	} else {
		attempts = append(attempts, err)
	}

	if output, err := runStatusCommand(ctx, "ostree", "admin", "status"); err == nil {
		status := parseOstreeAdminStatus(output)
		if status.Deployment != "" {
			return status, nil
		}
		attempts = append(attempts, fmt.Errorf("ostree admin status did not report a booted deployment"))
	} else {
		attempts = append(attempts, err)
	}

	return ostreeStatus{}, fmt.Errorf("ostree status unavailable: %w", errors.Join(attempts...))
}

func parseRPMOstreeStatus(raw []byte) (ostreeStatus, error) {
	var parsed rpmOstreeStatus
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ostreeStatus{}, fmt.Errorf("parse rpm-ostree status: %w", err)
	}
	if len(parsed.Deployments) == 0 {
		return ostreeStatus{}, fmt.Errorf("rpm-ostree status has no deployments")
	}

	deployment := parsed.Deployments[0]
	for _, candidate := range parsed.Deployments {
		if candidate.Booted {
			deployment = candidate
			break
		}
	}

	version := deployment.Version
	if version == "" && deployment.BaseCommitMeta != nil {
		if value, ok := deployment.BaseCommitMeta["version"].(string); ok {
			version = value
		}
	}

	checksum := deployment.Checksum
	if checksum == "" {
		checksum = deployment.BaseCommit
	}
	if version == "" {
		version = imageReferenceVersion(deployment.ContainerImageReference)
	}
	if version == "" {
		version = checksum
	}

	return ostreeStatus{
		Source:     "rpm-ostree status --json",
		Version:    version,
		Checksum:   checksum,
		Image:      deployment.ContainerImageReference,
		Booted:     deployment.Booted,
		Deployment: deployment.Origin,
	}, nil
}

func parseBootcStatus(raw []byte) (ostreeStatus, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return ostreeStatus{}, fmt.Errorf("parse bootc status: %w", err)
	}

	booted := nestedMap(root, "status", "booted")
	if booted == nil {
		booted = root
	}

	status := ostreeStatus{
		Source: "bootc status --json",
		Version: firstNestedString(booted,
			[]string{"image", "version"},
			[]string{"version"},
			[]string{"base", "version"},
		),
		Checksum: firstNestedString(booted,
			[]string{"image", "image_digest"},
			[]string{"image", "digest"},
			[]string{"checksum"},
			[]string{"base", "checksum"},
		),
		Image: firstNestedString(booted,
			[]string{"image", "image"},
			[]string{"image", "reference"},
			[]string{"image"},
		),
		Booted: true,
	}

	if status.Version == "" && status.Checksum == "" && status.Image == "" {
		return ostreeStatus{}, fmt.Errorf("bootc status has no booted image/version fields")
	}
	if status.Version == "" {
		status.Version = imageReferenceVersion(status.Image)
	}
	if status.Version == "" {
		status.Version = status.Checksum
	}

	return status, nil
}

func parseOstreeAdminStatus(raw []byte) ostreeStatus {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "*") {
			continue
		}
		return ostreeStatus{
			Source:     "ostree admin status",
			Booted:     true,
			Deployment: strings.TrimSpace(strings.TrimPrefix(trimmed, "*")),
		}
	}
	return ostreeStatus{Source: "ostree admin status"}
}

func nestedMap(root map[string]any, keys ...string) map[string]any {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func firstNestedString(root map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value := nestedString(root, path...); value != "" {
			return value
		}
	}
	return ""
}

func nestedString(root map[string]any, keys ...string) string {
	var current any = root
	for _, key := range keys {
		currentMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = currentMap[key]
	}

	switch value := current.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func runStatusCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, statusCommandTimeout)
	defer cancel()
	return runCommandOutput(commandCtx, name, args...)
}

// scheduleReboot reboots the host after a short grace period so the HTTP
// response can be flushed to the caller first.
func scheduleReboot() {
	time.Sleep(time.Second)
	if err := runCommand("systemctl", "reboot"); err != nil {
		log.Printf("reboot failed: %v", err)
	}
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	return applyConfig(cfg)
}

// applyConfig validates the desired node configuration and converges the host
// to it: host folders, host interfaces, firewall ports, the Podman socket, and
// the declared containers. Callers must hold applyMu so two convergence runs
// (the startup reconcile and a concurrent /config upload) cannot interleave
// host changes.
func applyConfig(cfg *Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}

	if err := ensureFolders(cfg.Folders); err != nil {
		return err
	}

	if err := configureHostInterfaces(cfg.Interfaces); err != nil {
		return err
	}

	if err := configureFirewallPorts(cfg.FirewallPorts); err != nil {
		return err
	}

	if err := startPodmanSocket(); err != nil {
		return err
	}

	if err := waitForUnixSocket(podmanSocketPath, 10*time.Second); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ctx, err := bindings.NewConnection(ctx, "unix:"+podmanSocketPath)
	if err != nil {
		return err
	}

	if err := stopUnmanagedContainers(ctx, cfg.Containers); err != nil {
		return err
	}

	var reconcileErrors []error
	for _, c := range cfg.Containers {
		if err := recreateContainer(ctx, c); err != nil {
			log.Printf("reconcile container %q failed: %v", c.Name, err)
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	if len(reconcileErrors) > 0 {
		return fmt.Errorf("%d of %d containers failed to reconcile: %w", len(reconcileErrors), len(cfg.Containers), errors.Join(reconcileErrors...))
	}

	log.Println("all containers started")
	return nil
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	if mode := strings.TrimSpace(cfg.PodmanMode); mode != "" && mode != podmanRootfulMode {
		return fmt.Errorf("podman_mode must be %q or empty", podmanRootfulMode)
	}

	for i, folder := range cfg.Folders {
		folderLabel := folder.Path
		if folderLabel == "" {
			folderLabel = fmt.Sprintf("#%d", i)
		}

		if folder.Path == "" {
			return fmt.Errorf("folder %s is missing path", folderLabel)
		}
		if folder.Chmod != "" {
			if _, err := parseFileMode(folder.Chmod); err != nil {
				return fmt.Errorf("invalid chmod %q for folder %s: %w", folder.Chmod, folderLabel, err)
			}
		}
		if folder.User != "" {
			if _, _, err := parseOwner(folder.User); err != nil {
				return fmt.Errorf("invalid user %q for folder %s: %w", folder.User, folderLabel, err)
			}
		}
	}

	// buildNMState validates the interface config (names, paired ip/subnet,
	// addresses within subnet, parseable gateway/DNS) without side effects.
	if _, err := buildNMState(cfg.Interfaces); err != nil {
		return err
	}

	for i, port := range cfg.FirewallPorts {
		portLabel := strings.TrimSpace(port.Port)
		if portLabel == "" {
			portLabel = fmt.Sprintf("#%d", i)
		}

		if err := validateFirewallPort(port.Port); err != nil {
			return fmt.Errorf("invalid firewall port %s: %w", portLabel, err)
		}

		if err := validateFirewallProtocol(port.Protocol); err != nil {
			return fmt.Errorf("invalid firewall protocol for port %s: %w", portLabel, err)
		}
	}

	for _, c := range cfg.Containers {
		if len(c.Interfaces) > 0 {
			return fmt.Errorf("container %q defines interfaces; interfaces configure host interfaces and must be declared at the top level", c.Name)
		}

		for _, p := range c.Ports {
			if err := validateContainerPort(p.HostPort); err != nil {
				return fmt.Errorf("container %q has invalid host_port %d: %w", c.Name, p.HostPort, err)
			}
			if err := validateContainerPort(p.ContainerPort); err != nil {
				return fmt.Errorf("container %q has invalid container_port %d: %w", c.Name, p.ContainerPort, err)
			}
		}

		for _, v := range c.Volumes {
			if err := validateSELinuxRelabel(v.SELinux); err != nil {
				return fmt.Errorf("container %q volume %q: %w", c.Name, v.ContainerPath, err)
			}
		}
	}

	return nil
}

func validateSELinuxRelabel(value string) error {
	switch strings.TrimSpace(value) {
	case "", "z", "Z":
		return nil
	default:
		return fmt.Errorf(`selinux must be "z", "Z", or empty`)
	}
}

func validateContainerPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}

func validateFirewallPort(port string) error {
	port = strings.TrimSpace(port)
	if port == "" {
		return fmt.Errorf("missing port")
	}

	start, end, hasRange := strings.Cut(port, "-")
	startPort, err := parsePortNumber(start)
	if err != nil {
		return err
	}

	if !hasRange {
		return nil
	}

	endPort, err := parsePortNumber(end)
	if err != nil {
		return err
	}
	if startPort > endPort {
		return fmt.Errorf("range start is greater than range end")
	}

	return nil
}

func parsePortNumber(port string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return 0, err
	}
	if value < 1 || value > 65535 {
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}

	return value, nil
}

func validateFirewallProtocol(protocol string) error {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "tcp", "udp", "sctp", "dccp":
		return nil
	default:
		return fmt.Errorf("protocol must be tcp, udp, sctp, or dccp")
	}
}

func ensureFolders(folders []FolderConfig) error {
	for i, folder := range folders {
		folderLabel := folder.Path
		if folderLabel == "" {
			folderLabel = fmt.Sprintf("#%d", i)
		}

		if folder.Path == "" {
			return fmt.Errorf("folder %s is missing path", folderLabel)
		}

		var mode os.FileMode
		if folder.Chmod != "" {
			parsedMode, err := parseFileMode(folder.Chmod)
			if err != nil {
				return fmt.Errorf("invalid chmod %q for folder %s: %w", folder.Chmod, folderLabel, err)
			}
			mode = parsedMode
		}

		uid, gid := -1, -1
		if folder.User != "" {
			parsedUID, parsedGID, err := parseOwner(folder.User)
			if err != nil {
				return fmt.Errorf("invalid user %q for folder %s: %w", folder.User, folderLabel, err)
			}
			uid, gid = parsedUID, parsedGID
		}

		if err := os.MkdirAll(folder.Path, 0o755); err != nil {
			return fmt.Errorf("create folder %q failed: %w", folder.Path, err)
		}

		if uid != -1 || gid != -1 {
			if err := os.Chown(folder.Path, uid, gid); err != nil {
				return fmt.Errorf("set owner for folder %q failed: %w", folder.Path, err)
			}
		}

		if folder.Chmod != "" {
			if err := os.Chmod(folder.Path, mode); err != nil {
				return fmt.Errorf("set chmod for folder %q failed: %w", folder.Path, err)
			}
		}
	}

	return nil
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

func configureFirewallPorts(ports []FirewallPortConfig) error {
	if len(ports) == 0 {
		return nil
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect to system bus failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	firewalld := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldObjectPath))
	config := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldConfigPath))
	for _, port := range ports {
		zone := strings.TrimSpace(port.Zone)
		portValue := strings.TrimSpace(port.Port)
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}

		// Runtime config takes effect immediately, without a firewalld reload.
		enabled, err := queryFirewallPort(firewalld, zone, portValue, protocol)
		if err != nil {
			return fmt.Errorf("query firewall port %s/%s failed: %w", portValue, protocol, err)
		}
		if !enabled {
			if err := addFirewallPort(firewalld, zone, portValue, protocol); err != nil {
				return fmt.Errorf("open firewall port %s/%s failed: %w", portValue, protocol, err)
			}
			log.Printf("opened firewall port %s/%s", portValue, protocol)
		}

		// Permanent config survives a firewalld reload and a reboot.
		if err := ensurePermanentFirewallPort(conn, firewalld, config, zone, portValue, protocol); err != nil {
			return fmt.Errorf("persist firewall port %s/%s failed: %w", portValue, protocol, err)
		}
	}

	return nil
}

func queryFirewallPort(firewalld dbus.BusObject, zone string, port string, protocol string) (bool, error) {
	var enabled bool
	err := firewalld.Call(firewalldZoneInterface+".queryPort", 0, zone, port, protocol).Store(&enabled)
	return enabled, err
}

func addFirewallPort(firewalld dbus.BusObject, zone string, port string, protocol string) error {
	var appliedZone string
	return firewalld.Call(firewalldZoneInterface+".addPort", 0, zone, port, protocol, int32(0)).Store(&appliedZone)
}

// ensurePermanentFirewallPort writes the port into firewalld's permanent
// configuration. The runtime config opened above is reset to the permanent
// config on `firewall-cmd --reload`, so without this the port would silently
// close until the next reconcile at boot. An empty zone resolves to firewalld's
// default zone, since the permanent config is addressed by an explicit name.
func ensurePermanentFirewallPort(conn *dbus.Conn, firewalld, config dbus.BusObject, zone, port, protocol string) error {
	zoneName := zone
	if zoneName == "" {
		if err := firewalld.Call(firewalldBusName+".getDefaultZone", 0).Store(&zoneName); err != nil {
			return fmt.Errorf("get default zone failed: %w", err)
		}
	}

	var zonePath dbus.ObjectPath
	if err := config.Call(firewalldConfigInterface+".getZoneByName", 0, zoneName).Store(&zonePath); err != nil {
		return fmt.Errorf("look up permanent zone %q failed: %w", zoneName, err)
	}

	zoneObject := conn.Object(firewalldBusName, zonePath)

	var enabled bool
	if err := zoneObject.Call(firewalldConfigZoneInterface+".queryPort", 0, port, protocol).Store(&enabled); err != nil {
		return fmt.Errorf("query permanent firewall port failed: %w", err)
	}
	if enabled {
		return nil
	}

	if err := zoneObject.Call(firewalldConfigZoneInterface+".addPort", 0, port, protocol).Err; err != nil {
		return fmt.Errorf("add permanent firewall port failed: %w", err)
	}

	log.Printf("persisted firewall port %s/%s in zone %s", port, protocol, zoneName)
	return nil
}

func startPodmanSocket() error {
	ctx := context.Background()

	conn, err := systemd.NewSystemConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd failed: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)

	_, err = conn.StartUnitContext(ctx, "podman.socket", "replace", ch)
	if err != nil {
		return fmt.Errorf("start podman.socket failed: %w", err)
	}

	result := <-ch
	if result != "done" {
		return fmt.Errorf("podman.socket start result: %s", result)
	}

	return nil
}

func waitForUnixSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("socket not reachable: %s", path)
}

func recreateContainer(ctx context.Context, c ContainerConfig) error {
	spec, err := createSpec(c)
	if err != nil {
		return err
	}

	if err := pullImage(ctx, c.Image); err != nil {
		return fmt.Errorf("pull image %q failed: %w", c.Image, err)
	}

	exists, err := containerExists(ctx, c.Name)
	if err != nil {
		return fmt.Errorf("check container %q failed: %w", c.Name, err)
	}

	if exists {
		if err := removeContainer(ctx, c.Name); err != nil {
			return fmt.Errorf("remove container %q failed: %w", c.Name, err)
		}
	}

	created, err := createContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container %q failed: %w", c.Name, err)
	}

	if err := startContainer(ctx, created.ID); err != nil {
		return fmt.Errorf("start container %q failed: %w", c.Name, err)
	}

	log.Printf("started container %s", c.Name)
	return nil
}

func createSpec(c ContainerConfig) (*containerSpec, error) {
	s := &containerSpec{
		Name:    c.Name,
		Image:   c.Image,
		User:    c.User,
		Env:     c.Env,
		Command: c.Command,
	}

	for _, p := range c.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}

		s.PortMappings = append(s.PortMappings, portMapping{
			HostIP:        p.HostIP,
			HostPort:      uint16(p.HostPort),
			ContainerPort: uint16(p.ContainerPort),
			Protocol:      proto,
		})
	}

	for _, v := range c.Volumes {
		options := []string{"rbind"}

		if v.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}

		if relabel := strings.TrimSpace(v.SELinux); relabel != "" {
			options = append(options, relabel)
		}

		s.Mounts = append(s.Mounts, mount{
			Type:        "bind",
			Source:      v.HostPath,
			Destination: v.ContainerPath,
			Options:     options,
		})
	}

	if c.Restart != "" {
		s.RestartPolicy = c.Restart
	}

	return s, nil
}

func pullImage(ctx context.Context, rawImage string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("reference", rawImage)

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/images/pull", params, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	if !response.IsSuccess() {
		return response.Process(nil)
	}

	var pullErrors []error
	decoder := json.NewDecoder(response.Body)
	for {
		var report imagePullReport
		if err := decoder.Decode(&report); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			pullErrors = append(pullErrors, fmt.Errorf("failed to decode image pull response: %w", err))
			break
		}

		switch {
		case report.Stream != "":
			fmt.Fprint(os.Stderr, report.Stream)
		case report.Error != "":
			pullErrors = append(pullErrors, errors.New(report.Error))
		case len(report.Images) > 0 || report.ID != "":
		default:
			pullErrors = append(pullErrors, fmt.Errorf("unexpected image pull response: %+v", report))
		}
	}

	return errors.Join(pullErrors...)
}

func stopUnmanagedContainers(ctx context.Context, configured []ContainerConfig) error {
	configuredNames := make(map[string]struct{}, len(configured))
	for _, c := range configured {
		configuredNames[c.Name] = struct{}{}
	}

	existing, err := listContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers failed: %w", err)
	}

	for _, container := range existing {
		if containerIsConfigured(container, configuredNames) || !containerNeedsStop(container.State) {
			continue
		}

		if container.ID == "" {
			return fmt.Errorf("cannot stop unmanaged container %q: missing id", containerDisplayName(container))
		}

		if err := stopContainer(ctx, container.ID); err != nil {
			return fmt.Errorf("stop unmanaged container %q failed: %w", containerDisplayName(container), err)
		}

		log.Printf("stopped unmanaged container %s", containerDisplayName(container))
	}

	return nil
}

func listContainers(ctx context.Context) ([]listedContainer, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("all", "true")

	var containers []listedContainer
	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/json", params, nil)
	if err != nil {
		return containers, err
	}
	defer func() { _ = response.Body.Close() }()

	return containers, response.Process(&containers)
}

func inspectContainer(ctx context.Context, nameOrID string) (containerInspectResponse, error) {
	var inspect containerInspectResponse

	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return inspect, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/json", nil, nil, nameOrID)
	if err != nil {
		return inspect, err
	}
	defer func() { _ = response.Body.Close() }()

	return inspect, response.Process(&inspect)
}

func collectRunningContainerStatuses(ctx context.Context, logTail int) ([]runningContainerStatus, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	statusCtx, err := bindings.NewConnection(statusCtx, "unix:"+podmanSocketPath)
	if err != nil {
		return nil, err
	}

	existing, err := listContainers(statusCtx)
	if err != nil {
		return nil, err
	}

	var running []runningContainerStatus
	for _, container := range existing {
		if !containerIsRunning(container.State) {
			continue
		}

		status := runningContainerStatus{
			ID:    container.ID,
			Name:  containerDisplayName(container),
			Names: append([]string(nil), container.Names...),
			State: container.State,
			Image: container.Image,
			Logs:  []string{},
		}

		inspect, err := inspectContainer(statusCtx, container.ID)
		if err != nil {
			status.Error = appendStatusError(status.Error, fmt.Sprintf("inspect: %v", err))
		} else {
			if inspect.Name != "" {
				status.Name = strings.TrimPrefix(inspect.Name, "/")
			}
			if inspect.ImageName != "" {
				status.Image = inspect.ImageName
			}
			status.ImageID = inspect.Image
			status.ImageDigest = inspect.ImageDigest
			status.Version = imageReferenceVersion(status.Image)
			if status.Version == "" {
				status.Version = imageReferenceVersion(status.ImageDigest)
			}
		}

		logs, err := containerLogLines(statusCtx, container.ID, logTail)
		if err != nil {
			status.Error = appendStatusError(status.Error, fmt.Sprintf("logs: %v", err))
		} else {
			status.Logs = logs
		}

		running = append(running, status)
	}

	return running, nil
}

func containerIsRunning(state string) bool {
	return strings.EqualFold(state, "running")
}

func appendStatusError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func containerLogLines(ctx context.Context, nameOrID string, tail int) ([]string, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("stdout", "true")
	params.Set("stderr", "true")
	params.Set("tail", strconv.Itoa(tail))

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/logs", params, nil, nameOrID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if !response.IsSuccess() && !response.IsInformational() {
		return nil, response.Process(nil)
	}

	var lines []string
	buffer := make([]byte, 1024)
	for {
		fd, length, err := demuxHeader(response.Body, buffer)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return lines, err
		}

		frame, err := demuxFrame(response.Body, buffer, length)
		if err != nil {
			return lines, err
		}

		stream := "stdout"
		switch fd {
		case 1:
			stream = "stdout"
		case 2:
			stream = "stderr"
		case 3:
			return lines, fmt.Errorf("podman log stream error: %s", strings.TrimSpace(string(frame)))
		}

		for _, line := range splitLogFrame(string(frame)) {
			lines = append(lines, stream+": "+line)
		}
	}

	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	return lines, nil
}

func demuxHeader(r io.Reader, buffer []byte) (int, int, error) {
	if len(buffer) < 8 {
		buffer = make([]byte, 8)
	}
	if _, err := io.ReadFull(r, buffer[0:8]); err != nil {
		return 0, 0, err
	}

	fd := int(buffer[0])
	if fd < 0 || fd > 3 {
		return 0, 0, fmt.Errorf("container log stream lost sync: channel %d", fd)
	}

	return fd, int(binary.BigEndian.Uint32(buffer[4:8])), nil
}

func demuxFrame(r io.Reader, buffer []byte, length int) ([]byte, error) {
	if len(buffer) < length {
		buffer = make([]byte, length)
	}
	if _, err := io.ReadFull(r, buffer[0:length]); err != nil {
		return nil, err
	}
	return buffer[0:length], nil
}

func splitLogFrame(frame string) []string {
	frame = strings.TrimSuffix(frame, "\n")
	if frame == "" {
		return nil
	}
	return strings.Split(frame, "\n")
}

func imageReferenceVersion(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	name, digest, hasDigest := strings.Cut(ref, "@")
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		return name[lastColon+1:]
	}
	if hasDigest {
		return digest
	}
	return ""
}

func containerIsConfigured(container listedContainer, configuredNames map[string]struct{}) bool {
	for _, name := range container.Names {
		if _, exists := configuredNames[name]; exists {
			return true
		}
	}

	return false
}

func containerNeedsStop(state string) bool {
	switch strings.ToLower(state) {
	case "created", "configured", "dead", "exited", "removing", "stopped":
		return false
	default:
		return true
	}
}

func containerDisplayName(container listedContainer) string {
	if len(container.Names) > 0 {
		return container.Names[0]
	}
	if container.ID != "" {
		return container.ID
	}
	return "<unknown>"
}

func containerExists(ctx context.Context, nameOrID string) (bool, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return false, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/exists", nil, nil, nameOrID)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.IsSuccess() {
		return true, nil
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}

	return false, response.Process(nil)
}

func stopContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("ignore", "true")

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/containers/%s/stop", params, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}

func removeContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("force", "true")

	response, err := conn.DoRequest(ctx, nil, http.MethodDelete, "/containers/%s", params, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}

func createContainer(ctx context.Context, spec *containerSpec) (containerCreateResponse, error) {
	var created containerCreateResponse

	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return created, err
	}

	body, err := json.Marshal(spec)
	if err != nil {
		return created, err
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")

	response, err := conn.DoRequest(ctx, bytes.NewReader(body), http.MethodPost, "/containers/create", nil, headers)
	if err != nil {
		return created, err
	}
	defer func() { _ = response.Body.Close() }()

	return created, response.Process(&created)
}

func startContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/containers/%s/start", nil, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}

// nmState is the subset of the nmstate desired-state schema this tool emits.
// It is marshaled to JSON (valid YAML) and applied through NetworkManager with
// `nmstatectl apply`, which is the Red Hat Device Edge-native, declarative,
// reboot-persistent path. It replaces direct netlink calls (which fight
// NetworkManager) and `resolvectl` (which needs systemd-resolved, not enabled
// by default on RHEL).
type nmState struct {
	Interfaces  []nmInterface `json:"interfaces,omitempty"`
	Routes      *nmRoutes     `json:"routes,omitempty"`
	DNSResolver *nmDNS        `json:"dns-resolver,omitempty"`
}

type nmInterface struct {
	Name  string     `json:"name"`
	Type  string     `json:"type"`
	State string     `json:"state"`
	IPv4  *nmIPStack `json:"ipv4,omitempty"`
	IPv6  *nmIPStack `json:"ipv6,omitempty"`
}

type nmIPStack struct {
	Enabled   bool        `json:"enabled"`
	DHCP      bool        `json:"dhcp"`
	Addresses []nmAddress `json:"address,omitempty"`
}

type nmAddress struct {
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefix-length"`
}

type nmRoutes struct {
	Config []nmRoute `json:"config"`
}

type nmRoute struct {
	Destination      string `json:"destination"`
	NextHopAddress   string `json:"next-hop-address"`
	NextHopInterface string `json:"next-hop-interface"`
}

type nmDNS struct {
	Config nmDNSConfig `json:"config"`
}

type nmDNSConfig struct {
	Server []string `json:"server,omitempty"`
}

func configureHostInterfaces(interfaces []InterfaceConfig) error {
	if len(interfaces) == 0 {
		return nil
	}

	state, err := buildNMState(interfaces)
	if err != nil {
		return err
	}

	document, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nmstate document failed: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(nmstateStatePath), 0o755); err != nil {
		return fmt.Errorf("create nmstate dir %q failed: %w", filepath.Dir(nmstateStatePath), err)
	}
	if err := os.WriteFile(nmstateStatePath, document, 0o644); err != nil {
		return fmt.Errorf("write nmstate document %q failed: %w", nmstateStatePath, err)
	}

	if err := runCommand("nmstatectl", "apply", nmstateStatePath); err != nil {
		return fmt.Errorf("apply host interface configuration failed: %w", err)
	}

	return nil
}

// buildNMState translates the tool's interface config into an nmstate desired
// state. It keeps the original validation (name required, ip_address/subnet
// paired, addresses inside their subnet, parseable gateway/DNS). DNS servers
// from every interface are merged into nmstate's single global resolver list,
// de-duplicated in first-seen order.
func buildNMState(interfaces []InterfaceConfig) (*nmState, error) {
	state := &nmState{}
	var dnsServers []string
	seenDNS := make(map[string]struct{})

	for i, iface := range interfaces {
		ifaceLabel := iface.Name
		if ifaceLabel == "" {
			ifaceLabel = fmt.Sprintf("#%d", i)
		}

		if iface.Name == "" {
			return nil, fmt.Errorf("host interface %s is missing name", ifaceLabel)
		}

		if (iface.IPAddress == "") != (iface.Subnet == "") {
			return nil, fmt.Errorf("host interface %q must set both ip_address and subnet", iface.Name)
		}

		// Existing physical NICs (the documented use case, e.g. eth0). Bonds,
		// VLANs, and bridges are out of scope for this schema.
		nmIface := nmInterface{Name: iface.Name, Type: "ethernet", State: "up"}

		if iface.IPAddress != "" {
			ipNet, err := parseInterfaceAddress(iface.IPAddress, iface.Subnet)
			if err != nil {
				return nil, fmt.Errorf("invalid host interface %s address: %w", ifaceLabel, err)
			}

			prefix, _ := ipNet.Mask.Size()
			stack := &nmIPStack{
				Enabled:   true,
				DHCP:      false,
				Addresses: []nmAddress{{IP: ipNet.IP.String(), PrefixLength: prefix}},
			}
			if ipNet.IP.To4() != nil {
				nmIface.IPv4 = stack
			} else {
				nmIface.IPv6 = stack
			}
		}

		state.Interfaces = append(state.Interfaces, nmIface)

		if iface.Gateway != "" {
			gateway, err := parseAddr(iface.Gateway)
			if err != nil {
				return nil, fmt.Errorf("invalid gateway %q for host interface %s", iface.Gateway, ifaceLabel)
			}

			destination := "0.0.0.0/0"
			if gateway.To4() == nil {
				destination = "::/0"
			}

			if state.Routes == nil {
				state.Routes = &nmRoutes{}
			}
			state.Routes.Config = append(state.Routes.Config, nmRoute{
				Destination:      destination,
				NextHopAddress:   gateway.String(),
				NextHopInterface: iface.Name,
			})
		}

		for _, dns := range iface.DNS {
			dnsIP, err := parseAddr(dns)
			if err != nil {
				return nil, fmt.Errorf("invalid dns server %q for host interface %s", dns, ifaceLabel)
			}

			key := dnsIP.String()
			if _, seen := seenDNS[key]; seen {
				continue
			}
			seenDNS[key] = struct{}{}
			dnsServers = append(dnsServers, key)
		}
	}

	if len(dnsServers) > 0 {
		state.DNSResolver = &nmDNS{Config: nmDNSConfig{Server: dnsServers}}
	}

	return state, nil
}

func parseInterfaceAddress(address string, subnet string) (*net.IPNet, error) {
	ip, err := parseAddr(address)
	if err != nil {
		return nil, fmt.Errorf("invalid ip_address %q", address)
	}

	_, cidr, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet %q: %w", subnet, err)
	}

	if !cidr.Contains(ip) {
		return nil, fmt.Errorf("ip_address %q is not within subnet %q", address, subnet)
	}

	return &net.IPNet{IP: ip, Mask: cidr.Mask}, nil
}

func parseAddr(addr string) (net.IP, error) {
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	return net.IP(parsed.AsSlice()), nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
}

func runCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return output, nil
	}

	message := strings.TrimSpace(string(output))
	if ctxErr := ctx.Err(); ctxErr != nil {
		if message == "" {
			return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctxErr)
		}
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), ctxErr, message)
	}
	if message == "" {
		return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
}
