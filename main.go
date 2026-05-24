// Command servermaster reconciles a Red Hat Device Edge node to a JSON
// configuration: it manages host folders and files, host network interfaces
// (through nmstate), firewalld ports, and the Podman containers that should be
// present, treating config.json as the single source of truth for node state.
// It also serves a status endpoint and the ostree OS-update endpoints on :8080.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/vishvananda/netlink"
)

const (
	defaultConfigPath       = "/data/config/containers.json"
	webServerAddress        = ":8080"
	defaultOstreeUploadPath = "/data/ostree/update.tar"
	podmanRootfulMode       = "rootful"
	servermasterLogTail     = 100
	statusCommandTimeout    = 5 * time.Second

	// nmstateApplyTimeout bounds `nmstatectl apply`'s verify-and-rollback cycle
	// (passed as --timeout). An interface that cannot reach its desired state —
	// for example a declared device that does not exist on the host — makes the
	// apply roll back and fail at this deadline instead of blocking forever. The
	// exec gets a slightly longer hard deadline (nmstateApplyTimeout + buffer) so
	// a wedged nmstatectl cannot hang the reconcile, and the /config request that
	// holds applyMu, indefinitely.
	nmstateApplyTimeout = 60 * time.Second

	// maxConfigUploadBytes caps the body accepted by /config. A node config is
	// a small JSON document; the limit stops an unauthenticated caller from
	// streaming an arbitrarily large body into memory.
	maxConfigUploadBytes = 1 << 20 // 1 MiB

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
//
//nolint:gochecknoglobals // process-wide lock guarding the single host apply.
var applyMu sync.Mutex

// configApplier converges the host to a parsed config. It is a package variable
// so tests can substitute the host-mutating apply; production uses applyConfig.
//
//nolint:gochecknoglobals // injectable seam so handlers can be tested without mutating the host.
var configApplier = applyConfig

// servermasterStatusCollector gathers the /servermaster response. Tests replace
// it so the handler can be exercised without requiring Podman or ostree.
//
//nolint:gochecknoglobals // injectable seam so the handler can be tested without Podman or ostree.
var servermasterStatusCollector = collectServermasterStatus

// serviceLog retains the most recent log lines in memory so the /servermaster
// endpoint can surface them in servermaster_log. captureServiceLog tees the
// standard logger into it at startup.
//
//nolint:gochecknoglobals // process-wide log ring teed from the standard logger.
var serviceLog = newLogRing(servermasterLogTail)

// podmanSocketPath is the libpod API socket the tool talks to. It is a variable
// rather than a constant so tests can point the client at a fake socket.
//
//nolint:gochecknoglobals // injectable seam so the Podman client can be tested against a fake socket.
var podmanSocketPath = "/run/podman/podman.sock"

// nmstateStatePath is where the generated nmstate desired-state document is
// written before it is applied. The .yml extension (JSON is valid YAML) lets
// nmstate.service reapply it at boot in addition to the apply call below. It is
// a variable so tests can redirect it away from the real /etc/nmstate.
//
//nolint:gochecknoglobals // injectable seam so interface apply can be tested without touching /etc/nmstate.
var nmstateStatePath = "/etc/nmstate/servermaster.yml"

// logRing is a bounded, concurrency-safe buffer of the most recent log lines. It
// is an io.Writer, so installing it as (part of) the standard logger's output
// captures every log.Print* call. The standard logger writes one full record per
// Write, so each Write is stored as one line.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(size int) *logRing {
	return &logRing{max: size}
}

func (r *logRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		// Copy the tail into a fresh slice so the dropped lines' backing array is
		// released rather than retained behind a reslice.
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

// snapshot returns a copy of the retained lines, oldest first. It is always
// non-nil so the JSON field renders as [] rather than null when empty.
func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// captureServiceLog tees the standard logger to its existing destination and the
// in-memory ring, so logs still reach stderr/journald while becoming queryable
// via /servermaster.
func captureServiceLog() {
	log.SetOutput(io.MultiWriter(log.Writer(), serviceLog))
}

type Config struct {
	PodmanMode    string               `json:"podman_mode"`
	Folders       []FolderConfig       `json:"folders"`
	Files         []FileConfig         `json:"files"`
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

type FileConfig struct {
	Path     string `json:"path"`
	Chmod    string `json:"chmod"`
	User     string `json:"user"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
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
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	IPAddress string      `json:"ip_address"`
	Subnet    string      `json:"subnet"`
	Gateway   string      `json:"gateway"`
	DNS       []string    `json:"dns"`
	VLAN      *VLANConfig `json:"vlan,omitempty"`
}

// VLANConfig describes an 802.1Q VLAN interface (type "vlan"): the VLAN rides on
// BaseInterface and is tagged with ID. The interface Name is the VLAN device's
// own name, conventionally "<base>.<id>" such as "eth0.100".
type VLANConfig struct {
	BaseInterface string `json:"base_interface"`
	ID            int    `json:"id"`
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
	Labels        map[string]string `json:"labels,omitempty"`
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
	Status          string                   `json:"status"`
	GeneratedAt     string                   `json:"generated_at"`
	Ostree          ostreeStatus             `json:"ostree"`
	FreeDiskSpace   []diskStatus             `json:"free_diskspace"`
	Network         networkStatus            `json:"network"`
	Containers      []runningContainerStatus `json:"containers"`
	ServermasterLog []string                 `json:"servermaster_log"`
	Errors          []string                 `json:"errors,omitempty"`
}

// networkStatus is the live network configuration of every host interface, read
// from the kernel via netlink rather than from config.json: it reports actual
// node state (including interfaces this tool does not manage), not the desired
// state. DNS is not a netlink concept, so it is read from /etc/resolv.conf.
type networkStatus struct {
	Source     string             `json:"source,omitempty"`
	Interfaces []networkInterface `json:"interfaces"`
	Routes     []networkRoute     `json:"routes,omitempty"`
	DNS        []string           `json:"dns,omitempty"`
	Error      string             `json:"error,omitempty"`
}

type networkInterface struct {
	Name      string           `json:"name"`
	Index     int              `json:"index"`
	Type      string           `json:"type,omitempty"`
	State     string           `json:"state,omitempty"`
	MAC       string           `json:"mac_address,omitempty"`
	MTU       int              `json:"mtu,omitempty"`
	Flags     []string         `json:"flags,omitempty"`
	Addresses []networkAddress `json:"addresses,omitempty"`
}

type networkAddress struct {
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefix_length"`
	Family       string `json:"family"`
}

type networkRoute struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface,omitempty"`
	Family      string `json:"family,omitempty"`
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
	ID          string                  `json:"Id"`
	Name        string                  `json:"Name"`
	Image       string                  `json:"Image"`
	ImageName   string                  `json:"ImageName"`
	ImageDigest string                  `json:"ImageDigest"`
	State       *containerInspectState  `json:"State"`
	Config      *containerInspectConfig `json:"Config"`
}

type containerInspectState struct {
	Running bool   `json:"Running"`
	Status  string `json:"Status"`
}

type containerInspectConfig struct {
	Labels map[string]string `json:"Labels"`
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
	captureServiceLog()

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

	network := collectNetworkStatus(ctx)
	if network.Error != "" {
		status.Errors = append(status.Errors, fmt.Sprintf("network: %s", network.Error))
	}
	status.Network = network

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

	// Captured after the other collectors so their log output is reflected.
	status.ServermasterLog = serviceLog.snapshot()

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

	blockSize := uint64(stat.Bsize) //nolint:gosec // Statfs block size is a kernel-reported positive value.
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

// writeConfigFile writes the config body to a temp file in the destination
// directory and renames it into place, so a crash mid-write can never leave a
// truncated config where the next boot would load it.
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

	_, writeErr := tmp.Write(body)
	closeErr := tmp.Close()
	if writeErr != nil {
		return fmt.Errorf("write config: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close config: %w", closeErr)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // config is intentionally world-readable for operator inspection on the node.
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
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // operator-owned staging dir; traversable by design on this single-tenant node.
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

	if err := ensureFiles(cfg.Files); err != nil {
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
		if err := reconcileContainer(ctx, c); err != nil {
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
	raw, err := os.ReadFile(path) //nolint:gosec // path is the operator-supplied config location, not attacker input.
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
	if err := validateFolders(cfg.Folders); err != nil {
		return err
	}
	if err := validateFiles(cfg.Files); err != nil {
		return err
	}
	// buildNMState validates the interface config (names, paired ip/subnet,
	// addresses within subnet, parseable gateway/DNS) without side effects.
	if _, err := buildNMState(cfg.Interfaces); err != nil {
		return err
	}
	if err := validateFirewallPortConfigs(cfg.FirewallPorts); err != nil {
		return err
	}
	return validateContainers(cfg.Containers)
}

// labelOrIndex returns name when it is set, otherwise a positional "#i" label
// used in validation messages for an unnamed config entry.
func labelOrIndex(name string, i int) string {
	if name == "" {
		return fmt.Sprintf("#%d", i)
	}
	return name
}

func validateFolders(folders []FolderConfig) error {
	for i, folder := range folders {
		label := labelOrIndex(folder.Path, i)
		if folder.Path == "" {
			return fmt.Errorf("folder %s is missing path", label)
		}
		if folder.Chmod != "" {
			if _, err := parseFileMode(folder.Chmod); err != nil {
				return fmt.Errorf("invalid chmod %q for folder %s: %w", folder.Chmod, label, err)
			}
		}
		if folder.User != "" {
			if _, _, err := parseOwner(folder.User); err != nil {
				return fmt.Errorf("invalid user %q for folder %s: %w", folder.User, label, err)
			}
		}
	}
	return nil
}

func validateFiles(files []FileConfig) error {
	for i, file := range files {
		label := labelOrIndex(file.Path, i)
		if file.Path == "" {
			return fmt.Errorf("file %s is missing path", label)
		}
		if file.Chmod != "" {
			if _, err := parseFileMode(file.Chmod); err != nil {
				return fmt.Errorf("invalid chmod %q for file %s: %w", file.Chmod, label, err)
			}
		}
		if file.User != "" {
			if _, _, err := parseOwner(file.User); err != nil {
				return fmt.Errorf("invalid user %q for file %s: %w", file.User, label, err)
			}
		}
		// Decoding validates the encoding name and (for base64) the content
		// without writing anything, so a bad file is rejected before any apply.
		if _, err := decodeFileContent(file); err != nil {
			return fmt.Errorf("invalid content for file %s: %w", label, err)
		}
	}
	return nil
}

func validateFirewallPortConfigs(ports []FirewallPortConfig) error {
	for i, port := range ports {
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
	return nil
}

func validateContainers(containers []ContainerConfig) error {
	for _, c := range containers {
		if err := validateContainer(c); err != nil {
			return err
		}
	}
	return nil
}

func validateContainer(c ContainerConfig) error {
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

// configureFirewallPorts enforces config.json as the single source of truth for
// firewalld: it opens (and persists) every declared port, then closes any port
// not declared and removes every firewalld service. Because the config owns the
// entire firewall surface, an empty list is not a no-op — it still runs the
// cleanup so no undeclared port and no service is left open. Access is expressed
// only as ports, so service-provided access (notably the default ssh service)
// survives only if the corresponding port (for example 22/tcp) is declared.
func configureFirewallPorts(ports []FirewallPortConfig) error {
	// firewalld owns its D-Bus name only while running and is not D-Bus
	// activatable on a default install, so bring it up before talking to it.
	// firewalld is an optional (Recommends) dependency: if it cannot be started
	// and no ports are declared there is nothing to enforce, so skip; if ports
	// are declared the config cannot be satisfied, so fail.
	if err := ensureFirewalldRunning(); err != nil {
		if len(ports) == 0 {
			log.Printf("skipping firewall reconcile, firewalld unavailable: %v", err)
			return nil
		}
		return err
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect to system bus failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	firewalld := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldObjectPath))
	config := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldConfigPath))

	var defaultZone string
	if err := firewalld.Call(firewalldBusName+".getDefaultZone", 0).Store(&defaultZone); err != nil {
		return fmt.Errorf("get default zone failed: %w", err)
	}

	for _, port := range ports {
		if err := openDeclaredFirewallPort(conn, firewalld, config, port); err != nil {
			return err
		}
	}

	declared := declaredFirewallPorts(ports, defaultZone)
	if err := removeUnmanagedFirewallRules(conn, firewalld, config, declared); err != nil {
		return err
	}

	return nil
}

// openDeclaredFirewallPort opens a single declared port in both the runtime and
// permanent firewalld configuration, defaulting an empty protocol to tcp.
func openDeclaredFirewallPort(conn *dbus.Conn, firewalld, config dbus.BusObject, port FirewallPortConfig) error {
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
	return nil
}

// firewallPortTuple decodes a firewalld permanent-config (port, protocol) struct
// (D-Bus signature `(ss)`).
type firewallPortTuple struct {
	Port     string
	Protocol string
}

// firewallPortKey normalizes a port and protocol into a comparison key, applying
// the same defaulting (lowercase protocol, empty protocol means tcp) used when
// opening declared ports so declared and live ports compare equal.
func firewallPortKey(port, protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	return strings.TrimSpace(port) + "/" + protocol
}

// declaredFirewallPorts groups the config ports by their resolved zone, returning
// per-zone sets of "port/proto" keys. An empty zone resolves to defaultZone, the
// same substitution firewalld makes when a port is opened without a zone.
func declaredFirewallPorts(ports []FirewallPortConfig, defaultZone string) map[string]map[string]struct{} {
	declared := make(map[string]map[string]struct{})
	for _, port := range ports {
		zone := strings.TrimSpace(port.Zone)
		if zone == "" {
			zone = defaultZone
		}
		if declared[zone] == nil {
			declared[zone] = make(map[string]struct{})
		}
		declared[zone][firewallPortKey(port.Port, port.Protocol)] = struct{}{}
	}
	return declared
}

// removeUnmanagedFirewallRules enforces config.json as the single source of
// truth for firewalld: across every zone, in both the runtime and permanent
// configuration, it closes every open port not present in declared and removes
// every service. Services are stripped wholesale because config.json expresses
// access only as ports — so any service-provided access (notably the default
// `ssh` service) survives a reconcile only if re-declared as a port. declared
// maps a zone name to the set of "port/proto" keys allowed in that zone.
func removeUnmanagedFirewallRules(conn *dbus.Conn, firewalld, config dbus.BusObject, declared map[string]map[string]struct{}) error {
	if err := removeUnmanagedRuntimeRules(firewalld, declared); err != nil {
		return err
	}
	return removeUnmanagedPermanentRules(conn, config, declared)
}

// removeUnmanagedRuntimeRules prunes the runtime configuration, where changes
// take effect immediately.
func removeUnmanagedRuntimeRules(firewalld dbus.BusObject, declared map[string]map[string]struct{}) error {
	var zones []string
	if err := firewalld.Call(firewalldZoneInterface+".getZones", 0).Store(&zones); err != nil {
		return fmt.Errorf("list runtime firewall zones failed: %w", err)
	}
	for _, zone := range zones {
		if err := pruneRuntimeZone(firewalld, zone, declared[zone]); err != nil {
			return err
		}
	}
	return nil
}

func pruneRuntimeZone(firewalld dbus.BusObject, zone string, declared map[string]struct{}) error {
	var current [][]string
	if err := firewalld.Call(firewalldZoneInterface+".getPorts", 0, zone).Store(&current); err != nil {
		return fmt.Errorf("list runtime ports for zone %q failed: %w", zone, err)
	}
	for _, pp := range current {
		if len(pp) != 2 {
			continue
		}
		port, protocol := pp[0], pp[1]
		if _, ok := declared[firewallPortKey(port, protocol)]; ok {
			continue
		}
		var appliedZone string
		if err := firewalld.Call(firewalldZoneInterface+".removePort", 0, zone, port, protocol).Store(&appliedZone); err != nil {
			return fmt.Errorf("close unmanaged firewall port %s/%s in zone %q failed: %w", port, protocol, zone, err)
		}
		log.Printf("closed unmanaged firewall port %s/%s in zone %s", port, protocol, zone)
	}

	var services []string
	if err := firewalld.Call(firewalldZoneInterface+".getServices", 0, zone).Store(&services); err != nil {
		return fmt.Errorf("list runtime services for zone %q failed: %w", zone, err)
	}
	for _, service := range services {
		var appliedZone string
		if err := firewalld.Call(firewalldZoneInterface+".removeService", 0, zone, service).Store(&appliedZone); err != nil {
			return fmt.Errorf("remove firewall service %q in zone %q failed: %w", service, zone, err)
		}
		log.Printf("removed firewall service %s in zone %s", service, zone)
	}
	return nil
}

// removeUnmanagedPermanentRules prunes the permanent configuration, where
// changes survive a firewalld reload and a reboot.
func removeUnmanagedPermanentRules(conn *dbus.Conn, config dbus.BusObject, declared map[string]map[string]struct{}) error {
	var zones []string
	if err := config.Call(firewalldConfigInterface+".getZoneNames", 0).Store(&zones); err != nil {
		return fmt.Errorf("list permanent firewall zones failed: %w", err)
	}
	for _, zone := range zones {
		if err := prunePermanentZone(conn, config, zone, declared[zone]); err != nil {
			return err
		}
	}
	return nil
}

func prunePermanentZone(conn *dbus.Conn, config dbus.BusObject, zone string, declared map[string]struct{}) error {
	var zonePath dbus.ObjectPath
	if err := config.Call(firewalldConfigInterface+".getZoneByName", 0, zone).Store(&zonePath); err != nil {
		return fmt.Errorf("look up permanent zone %q failed: %w", zone, err)
	}
	zoneObject := conn.Object(firewalldBusName, zonePath)

	var current []firewallPortTuple
	if err := zoneObject.Call(firewalldConfigZoneInterface+".getPorts", 0).Store(&current); err != nil {
		return fmt.Errorf("list permanent ports for zone %q failed: %w", zone, err)
	}
	for _, pp := range current {
		if _, ok := declared[firewallPortKey(pp.Port, pp.Protocol)]; ok {
			continue
		}
		if err := zoneObject.Call(firewalldConfigZoneInterface+".removePort", 0, pp.Port, pp.Protocol).Err; err != nil {
			return fmt.Errorf("remove permanent firewall port %s/%s in zone %q failed: %w", pp.Port, pp.Protocol, zone, err)
		}
		log.Printf("removed unmanaged permanent firewall port %s/%s in zone %s", pp.Port, pp.Protocol, zone)
	}

	var services []string
	if err := zoneObject.Call(firewalldConfigZoneInterface+".getServices", 0).Store(&services); err != nil {
		return fmt.Errorf("list permanent services for zone %q failed: %w", zone, err)
	}
	for _, service := range services {
		if err := zoneObject.Call(firewalldConfigZoneInterface+".removeService", 0, service).Err; err != nil {
			return fmt.Errorf("remove permanent firewall service %q in zone %q failed: %w", service, zone, err)
		}
		log.Printf("removed permanent firewall service %s in zone %s", service, zone)
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

// ensureFirewalldRunning starts firewalld.service through systemd so its D-Bus
// name is owned before ports are configured; on a host where firewalld is merely
// stopped this makes the apply self-healing instead of failing with "name is not
// activatable". firewalld.service is Type=dbus, so a "done" job result means the
// bus name has been acquired, and starting an already-active unit is a no-op. An
// error means firewalld is absent, masked, or failed to start.
func ensureFirewalldRunning() error {
	ctx := context.Background()

	conn, err := systemd.NewSystemConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd failed: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)

	if _, err := conn.StartUnitContext(ctx, "firewalld.service", "replace", ch); err != nil {
		return fmt.Errorf("start firewalld.service failed: %w", err)
	}

	result := <-ch
	if result != "done" {
		return fmt.Errorf("firewalld.service start result: %s", result)
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

// configHashLabel records, on the container, a hash of the ContainerConfig it
// was created from. reconcileContainer uses it to leave a running container
// untouched when its desired config is unchanged.
const configHashLabel = "servermaster.config-hash"

// configHash is a stable fingerprint of a container's desired configuration. Go
// marshals struct fields in declaration order and map keys in sorted order, so
// the encoding — and therefore the hash — is deterministic for equal configs.
func configHash(c ContainerConfig) string {
	data, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// reconcileContainer converges a single declared container to its desired state.
// A container that is already running with a matching config hash is left as-is,
// so an unchanged container is never restarted, re-pulled, or recreated. Any
// other case (missing, stopped, or config changed) is (re)created from the spec,
// pulling the image only when it is not already present in local storage.
func reconcileContainer(ctx context.Context, c ContainerConfig) error {
	spec, err := createSpec(c)
	if err != nil {
		return err
	}

	desiredHash := configHash(c)
	if spec.Labels == nil {
		spec.Labels = make(map[string]string, 1)
	}
	spec.Labels[configHashLabel] = desiredHash

	exists, err := containerExists(ctx, c.Name)
	if err != nil {
		return fmt.Errorf("check container %q failed: %w", c.Name, err)
	}

	current, err := containerIsCurrent(ctx, c.Name, exists, desiredHash)
	if err != nil {
		return err
	}
	if current {
		log.Printf("container %s unchanged, leaving it running", c.Name)
		return nil
	}

	present, err := imageExists(ctx, c.Image)
	if err != nil {
		return fmt.Errorf("check image %q failed: %w", c.Image, err)
	}
	if !present {
		if err := pullImage(ctx, c.Image); err != nil {
			return fmt.Errorf("pull image %q failed: %w", c.Image, err)
		}
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

// containerIsCurrent reports whether an existing, declared container can be left
// running untouched: it must already exist and, on inspection, be running with a
// matching config hash. A container that does not exist is never current.
func containerIsCurrent(ctx context.Context, name string, exists bool, desiredHash string) (bool, error) {
	if !exists {
		return false, nil
	}
	inspect, err := inspectContainer(ctx, name)
	if err != nil {
		return false, fmt.Errorf("inspect container %q failed: %w", name, err)
	}
	return containerUpToDate(inspect, desiredHash), nil
}

// containerUpToDate reports whether an existing container is running and was
// created from the desired config (matching hash label). A stopped container, or
// one created before this label existed or from a different config, is not up to
// date and must be recreated.
func containerUpToDate(inspect containerInspectResponse, desiredHash string) bool {
	if inspect.State == nil || !inspect.State.Running {
		return false
	}
	if inspect.Config == nil {
		return false
	}
	return inspect.Config.Labels[configHashLabel] == desiredHash
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

		// validateConfig (run before any reconcile) guarantees both ports are in
		// 1-65535, so neither uint16 conversion can overflow.
		hostPort := uint16(p.HostPort)           //nolint:gosec // bounded to 1-65535 by validateConfig.
		containerPort := uint16(p.ContainerPort) //nolint:gosec // bounded to 1-65535 by validateConfig.
		s.PortMappings = append(s.PortMappings, portMapping{
			HostIP:        p.HostIP,
			HostPort:      hostPort,
			ContainerPort: containerPort,
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

// imageExists reports whether an image reference is already present in local
// storage, using the same 204/404 exists endpoint pattern as containerExists.
func imageExists(ctx context.Context, rawImage string) (bool, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return false, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/images/%s/exists", nil, nil, rawImage)
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
		running = append(running, buildRunningContainerStatus(statusCtx, container, logTail))
	}

	return running, nil
}

// buildRunningContainerStatus assembles the status for one running container,
// folding inspect and log failures into the status's Error field rather than
// failing the whole collection.
func buildRunningContainerStatus(ctx context.Context, container listedContainer, logTail int) runningContainerStatus {
	status := runningContainerStatus{
		ID:    container.ID,
		Name:  containerDisplayName(container),
		Names: append([]string(nil), container.Names...),
		State: container.State,
		Image: container.Image,
		Logs:  []string{},
	}

	if inspect, err := inspectContainer(ctx, container.ID); err != nil {
		status.Error = appendStatusError(status.Error, fmt.Sprintf("inspect: %v", err))
	} else {
		applyInspectToStatus(&status, inspect)
	}

	if logs, err := containerLogLines(ctx, container.ID, logTail); err != nil {
		status.Error = appendStatusError(status.Error, fmt.Sprintf("logs: %v", err))
	} else {
		status.Logs = logs
	}

	return status
}

// applyInspectToStatus overlays the richer detail from a container inspect onto
// the status built from the list entry.
func applyInspectToStatus(status *runningContainerStatus, inspect containerInspectResponse) {
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

		frameLines, err := logFrameLines(fd, frame)
		if err != nil {
			return lines, err
		}
		lines = append(lines, frameLines...)
	}

	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	return lines, nil
}

// logFrameLines turns one demuxed podman log frame into prefixed log lines, one
// per text line. Channel 1 is stdout and 2 is stderr; channel 3 carries a
// stream error, which is surfaced as an error.
func logFrameLines(fd int, frame []byte) ([]string, error) {
	stream := "stdout"
	switch fd {
	case 1:
		stream = "stdout"
	case 2:
		stream = "stderr"
	case 3:
		return nil, fmt.Errorf("podman log stream error: %s", strings.TrimSpace(string(frame)))
	}

	var lines []string
	for _, line := range splitLogFrame(string(frame)) {
		lines = append(lines, stream+": "+line)
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
	VLAN  *nmVLAN    `json:"vlan,omitempty"`
}

type nmVLAN struct {
	BaseIface string `json:"base-iface"`
	ID        int    `json:"id"`
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

// netlink access points are indirected through variables so tests can supply
// fixtures without a live kernel/network namespace. resolvConfPath is the file
// the resolver nameservers are read from.
//
//nolint:gochecknoglobals // injectable seams so network status can be tested without a live kernel.
var (
	netlinkLinkList  = netlink.LinkList
	netlinkAddrList  = netlink.AddrList
	netlinkRouteList = netlink.RouteList
	resolvConfPath   = "/etc/resolv.conf"
)

// collectNetworkStatus reads the live network configuration of every host
// interface from the kernel via netlink (links, addresses, and routes) plus the
// resolver list from /etc/resolv.conf. Partial failures are recorded in the
// returned status's Error field rather than aborting the wider collection; a
// failure to list links at all is fatal to this section only.
func collectNetworkStatus(ctx context.Context) networkStatus {
	_ = ctx // netlink calls are synchronous syscalls and do not take a context.

	links, err := netlinkLinkList()
	if err != nil {
		return networkStatus{Error: fmt.Sprintf("list links: %v", err)}
	}

	status := networkStatus{
		Source:     "netlink",
		Interfaces: make([]networkInterface, 0, len(links)),
	}

	nameByIndex := make(map[int]string, len(links))
	for _, link := range links {
		attrs := link.Attrs()
		nameByIndex[attrs.Index] = attrs.Name

		iface := networkInterface{
			Name:  attrs.Name,
			Index: attrs.Index,
			Type:  link.Type(),
			State: attrs.OperState.String(),
			MTU:   attrs.MTU,
			Flags: interfaceFlags(attrs.Flags),
		}
		if len(attrs.HardwareAddr) > 0 {
			iface.MAC = attrs.HardwareAddr.String()
		}

		addrs, addrErr := interfaceAddresses(link)
		if addrErr != nil {
			status.Error = appendStatusError(status.Error, fmt.Sprintf("addresses for %s: %v", attrs.Name, addrErr))
		}
		iface.Addresses = addrs

		status.Interfaces = append(status.Interfaces, iface)
	}

	routes, err := netlinkRouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		status.Error = appendStatusError(status.Error, fmt.Sprintf("list routes: %v", err))
	} else {
		for _, route := range routes {
			status.Routes = append(status.Routes, convertRoute(route, nameByIndex))
		}
	}

	status.DNS = resolvConfNameservers(resolvConfPath)

	return status
}

func interfaceAddresses(link netlink.Link) ([]networkAddress, error) {
	addrs, err := netlinkAddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}

	result := make([]networkAddress, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IPNet == nil {
			continue
		}
		prefix, _ := addr.Mask.Size()
		result = append(result, networkAddress{
			IP:           addr.IP.String(),
			PrefixLength: prefix,
			Family:       ipFamily(addr.IP),
		})
	}
	return result, nil
}

func convertRoute(route netlink.Route, nameByIndex map[int]string) networkRoute {
	converted := networkRoute{
		Interface: nameByIndex[route.LinkIndex],
		Family:    routeFamily(route),
	}

	switch {
	case route.Dst != nil:
		converted.Destination = route.Dst.String()
	case converted.Family == "ipv6":
		converted.Destination = "::/0"
	default:
		converted.Destination = "0.0.0.0/0"
	}

	if len(route.Gw) > 0 {
		converted.Gateway = route.Gw.String()
	}

	return converted
}

func interfaceFlags(flags net.Flags) []string {
	if flags == 0 {
		return nil
	}
	return strings.Split(flags.String(), "|")
}

func ipFamily(ip net.IP) string {
	if ip.To4() != nil {
		return "ipv4"
	}
	return "ipv6"
}

func routeFamily(route netlink.Route) string {
	switch route.Family {
	case netlink.FAMILY_V4:
		return "ipv4"
	case netlink.FAMILY_V6:
		return "ipv6"
	}
	// Family is not always populated; infer it from the route's addresses.
	if route.Dst != nil {
		return ipFamily(route.Dst.IP)
	}
	if len(route.Gw) > 0 {
		return ipFamily(route.Gw)
	}
	return "ipv4"
}

// resolvConfNameservers returns the nameserver addresses declared in a
// resolv.conf-formatted file. A missing or unreadable file yields no servers
// (DNS is best-effort context, not a hard error for the status endpoint).
func resolvConfNameservers(path string) []string {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed resolvConfPath (overridable only by tests), not request input.
	if err != nil {
		return nil
	}

	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	return servers
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

	if err := os.MkdirAll(filepath.Dir(nmstateStatePath), 0o755); err != nil { //nolint:gosec // /etc/nmstate must stay traversable so nmstate.service can read the state at boot.
		return fmt.Errorf("create nmstate dir %q failed: %w", filepath.Dir(nmstateStatePath), err)
	}
	if err := os.WriteFile(nmstateStatePath, document, 0o644); err != nil { //nolint:gosec // nmstate.service reads this file to reapply network state at boot; not secret.
		return fmt.Errorf("write nmstate document %q failed: %w", nmstateStatePath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), nmstateApplyTimeout+30*time.Second)
	defer cancel()

	nmTimeout := strconv.Itoa(int(nmstateApplyTimeout.Seconds()))
	if _, err := runCommandOutput(ctx, "nmstatectl", "apply", "--timeout", nmTimeout, nmstateStatePath); err != nil {
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
		label := labelOrIndex(iface.Name, i)

		nmIface, err := buildNMInterface(iface, label)
		if err != nil {
			return nil, err
		}
		state.Interfaces = append(state.Interfaces, nmIface)

		route, err := gatewayRoute(iface, label)
		if err != nil {
			return nil, err
		}
		if route != nil {
			if state.Routes == nil {
				state.Routes = &nmRoutes{}
			}
			state.Routes.Config = append(state.Routes.Config, *route)
		}

		dnsServers, err = appendInterfaceDNS(iface, label, seenDNS, dnsServers)
		if err != nil {
			return nil, err
		}
	}

	if len(dnsServers) > 0 {
		state.DNSResolver = &nmDNS{Config: nmDNSConfig{Server: dnsServers}}
	}

	return state, nil
}

// buildNMInterface translates one InterfaceConfig into an nmstate interface,
// validating its name, paired ip_address/subnet, optional VLAN settings, and
// (when set) its static address.
func buildNMInterface(iface InterfaceConfig, label string) (nmInterface, error) {
	if iface.Name == "" {
		return nmInterface{}, fmt.Errorf("host interface %s is missing name", label)
	}

	if (iface.IPAddress == "") != (iface.Subnet == "") {
		return nmInterface{}, fmt.Errorf("host interface %q must set both ip_address and subnet", iface.Name)
	}

	// Defaults to a physical NIC (the documented use case, e.g. eth0). An
	// explicit type lets nmstate manage other kinds it supports — "dummy" for
	// a software test interface, or "vlan" for an 802.1Q tagged interface.
	// nmstate validates the value at apply time. Bonds and bridges need extra
	// params and remain out of scope for this schema.
	ifaceType := strings.TrimSpace(iface.Type)
	if ifaceType == "" {
		ifaceType = "ethernet"
	}
	nmIface := nmInterface{Name: iface.Name, Type: ifaceType, State: "up"}

	vlan, err := interfaceVLAN(iface, label, ifaceType)
	if err != nil {
		return nmInterface{}, err
	}
	nmIface.VLAN = vlan

	if iface.IPAddress != "" {
		ipNet, err := parseInterfaceAddress(iface.IPAddress, iface.Subnet)
		if err != nil {
			return nmInterface{}, fmt.Errorf("invalid host interface %s address: %w", label, err)
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

	return nmIface, nil
}

// interfaceVLAN validates and builds an interface's VLAN settings. It returns
// nil when the interface is not a VLAN, and an error when vlan settings are
// missing for a "vlan" interface or present on a non-VLAN one.
func interfaceVLAN(iface InterfaceConfig, label, ifaceType string) (*nmVLAN, error) {
	switch {
	case ifaceType == "vlan":
		if iface.VLAN == nil {
			return nil, fmt.Errorf("host interface %s is type vlan but has no vlan settings", label)
		}
		base := strings.TrimSpace(iface.VLAN.BaseInterface)
		if base == "" {
			return nil, fmt.Errorf("host interface %s vlan is missing base_interface", label)
		}
		if iface.VLAN.ID < 1 || iface.VLAN.ID > 4094 {
			return nil, fmt.Errorf("host interface %s vlan id %d must be between 1 and 4094", label, iface.VLAN.ID)
		}
		return &nmVLAN{BaseIface: base, ID: iface.VLAN.ID}, nil
	case iface.VLAN != nil:
		return nil, fmt.Errorf("host interface %s sets vlan settings but type is %q, not \"vlan\"", label, ifaceType)
	default:
		return nil, nil
	}
}

// gatewayRoute builds the default route for an interface's gateway, returning
// nil when no gateway is declared.
func gatewayRoute(iface InterfaceConfig, label string) (*nmRoute, error) {
	if iface.Gateway == "" {
		return nil, nil
	}

	gateway, err := parseAddr(iface.Gateway)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway %q for host interface %s", iface.Gateway, label)
	}

	destination := "0.0.0.0/0"
	if gateway.To4() == nil {
		destination = "::/0"
	}

	return &nmRoute{
		Destination:      destination,
		NextHopAddress:   gateway.String(),
		NextHopInterface: iface.Name,
	}, nil
}

// appendInterfaceDNS validates an interface's DNS servers and appends the ones
// not already in seen to servers, preserving first-seen order across interfaces.
func appendInterfaceDNS(iface InterfaceConfig, label string, seen map[string]struct{}, servers []string) ([]string, error) {
	for _, dns := range iface.DNS {
		dnsIP, err := parseAddr(dns)
		if err != nil {
			return nil, fmt.Errorf("invalid dns server %q for host interface %s", dns, label)
		}

		key := dnsIP.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		servers = append(servers, key)
	}
	return servers, nil
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
	cmd := exec.Command(name, args...) //nolint:gosec // runs operator-declared host commands (e.g. ostree.apply_command); managing the host is this tool's purpose.
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
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // runs fixed status/apply commands (nmstatectl, rpm-ostree, …), not request input.
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
