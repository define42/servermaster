package main

import (
	"bytes"
	"context"
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
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"github.com/vishvananda/netlink"
)

type PodmanMode string

const (
	PodmanRootful  PodmanMode = "rootful"
	PodmanRootless PodmanMode = "rootless"

	defaultConfigPath       = "/data/config/containers.json"
	webServerAddress        = ":8080"
	defaultOstreeUploadPath = "/data/ostree/update.tar"

	serviceName       = "servermaster.service"
	serviceBinaryPath = "/usr/local/bin/servermaster"

	firewalldBusName       = "org.fedoraproject.FirewallD1"
	firewalldObjectPath    = "/org/fedoraproject/FirewallD1"
	firewalldZoneInterface = "org.fedoraproject.FirewallD1.zone"
)

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
	State string   `json:"State"`
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to config JSON file")
	installServiceFlag := flag.Bool("install-service", false, "install and start the systemd service")
	flag.Parse()

	if *installServiceFlag {
		if err := installService(*configPath); err != nil {
			log.Fatal(err)
		}
		log.Printf("installed and started %s", serviceName)
		return
	}

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
	// The web server stays up so the host remains observable.
	if err := run(configPath); err != nil {
		log.Printf("reconcile failed: %v", err)
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
		fmt.Fprintln(w, "servermaster running")
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
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
	defer r.Body.Close()

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
	defer os.Remove(tmpName) // best-effort; a no-op once the rename succeeds

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
	fmt.Fprintf(w, "uploaded %d bytes to %s\n", written, dest)
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
		fmt.Fprintln(w, "update applied; reboot skipped")
		return
	}

	log.Println("ostree update applied; scheduling reboot")
	fmt.Fprintln(w, "update applied; rebooting")
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

	mode := PodmanMode(cfg.PodmanMode)
	if mode == "" {
		mode = PodmanRootful
	}

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

	if err := startPodmanSocket(mode); err != nil {
		return err
	}

	socketPath, err := podmanSocketPath(mode)
	if err != nil {
		return err
	}

	if err := waitForUnixSocket(socketPath, 10*time.Second); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ctx, err = bindings.NewConnection(ctx, "unix:"+socketPath)
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
	}

	return nil
}

func validateContainerPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}

func installService(configPath string) error {
	unitPath := filepath.Join("/etc/systemd/system", serviceName)

	if err := os.WriteFile(unitPath, []byte(serviceContent(configPath)), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	ctx := context.Background()

	conn, err := systemd.NewSystemConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd: %w", err)
	}
	defer conn.Close()

	if err := conn.ReloadContext(ctx); err != nil {
		return fmt.Errorf("systemd daemon-reload failed: %w", err)
	}

	_, _, err = conn.EnableUnitFilesContext(ctx, []string{serviceName}, false, true)
	if err != nil {
		return fmt.Errorf("enable unit failed: %w", err)
	}

	ch := make(chan string, 1)
	_, err = conn.StartUnitContext(ctx, serviceName, "replace", ch)
	if err != nil {
		return fmt.Errorf("start unit failed: %w", err)
	}

	result := <-ch
	if result != "done" {
		return fmt.Errorf("start unit result: %s", result)
	}

	return nil
}

func serviceContent(configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Servermaster Node Configuration Service
After=network-online.target podman.socket firewalld.service
Wants=network-online.target
Requires=podman.socket

[Service]
Type=simple
ExecStart=%s -config %s
Restart=always
RestartSec=10s
User=root
Group=root

[Install]
WantedBy=multi-user.target
`, serviceBinaryPath, systemdQuoteArg(configPath))
}

func systemdQuoteArg(arg string) string {
	return strconv.Quote(strings.ReplaceAll(arg, "%", "%%"))
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
	defer conn.Close()

	firewalld := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldObjectPath))
	for _, port := range ports {
		zone := strings.TrimSpace(port.Zone)
		portValue := strings.TrimSpace(port.Port)
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}

		enabled, err := queryFirewallPort(firewalld, zone, portValue, protocol)
		if err != nil {
			return fmt.Errorf("query firewall port %s/%s failed: %w", portValue, protocol, err)
		}
		if enabled {
			continue
		}

		if err := addFirewallPort(firewalld, zone, portValue, protocol); err != nil {
			return fmt.Errorf("open firewall port %s/%s failed: %w", portValue, protocol, err)
		}

		log.Printf("opened firewall port %s/%s", portValue, protocol)
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

func startPodmanSocket(mode PodmanMode) error {
	ctx := context.Background()

	var conn *systemd.Conn
	var err error

	switch mode {
	case PodmanRootful:
		conn, err = systemd.NewSystemConnectionContext(ctx)
	case PodmanRootless:
		conn, err = systemd.NewUserConnectionContext(ctx)
	default:
		return fmt.Errorf("unknown podman mode: %s", mode)
	}

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

func podmanSocketPath(mode PodmanMode) (string, error) {
	switch mode {
	case PodmanRootful:
		return "/run/podman/podman.sock", nil

	case PodmanRootless:
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			return "", fmt.Errorf("XDG_RUNTIME_DIR is not set")
		}
		return runtimeDir + "/podman/podman.sock", nil

	default:
		return "", fmt.Errorf("unknown podman mode: %s", mode)
	}
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
	defer response.Body.Close()

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
	defer response.Body.Close()

	return containers, response.Process(&containers)
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
	defer response.Body.Close()

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
	defer response.Body.Close()

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
	defer response.Body.Close()

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
	defer response.Body.Close()

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
	defer response.Body.Close()

	return response.Process(nil)
}

func configureHostInterfaces(interfaces []InterfaceConfig) error {
	for i, iface := range interfaces {
		ifaceLabel := iface.Name
		if ifaceLabel == "" {
			ifaceLabel = fmt.Sprintf("#%d", i)
		}

		if iface.Name == "" {
			return fmt.Errorf("host interface %s is missing name", ifaceLabel)
		}

		link, err := netlink.LinkByName(iface.Name)
		if err != nil {
			return fmt.Errorf("host interface %q not found: %w", iface.Name, err)
		}

		if (iface.IPAddress == "") != (iface.Subnet == "") {
			return fmt.Errorf("host interface %q must set both ip_address and subnet", iface.Name)
		}

		if iface.IPAddress != "" {
			ipNet, err := parseInterfaceAddress(iface.IPAddress, iface.Subnet)
			if err != nil {
				return fmt.Errorf("invalid host interface %s address: %w", ifaceLabel, err)
			}

			if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ipNet}); err != nil {
				return fmt.Errorf("configure address for host interface %q failed: %w", iface.Name, err)
			}
		}

		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bring up host interface %q failed: %w", iface.Name, err)
		}

		if iface.Gateway != "" {
			gateway, err := parseAddr(iface.Gateway)
			if err != nil {
				return fmt.Errorf("invalid gateway %q for host interface %s", iface.Gateway, ifaceLabel)
			}

			route := netlink.Route{
				LinkIndex: link.Attrs().Index,
				Gw:        gateway,
			}
			if gateway.To4() == nil {
				route.Family = netlink.FAMILY_V6
			} else {
				route.Family = netlink.FAMILY_V4
			}

			if err := netlink.RouteReplace(&route); err != nil {
				return fmt.Errorf("configure default route for host interface %q failed: %w", iface.Name, err)
			}
		}

		if len(iface.DNS) > 0 {
			args := []string{"dns", iface.Name}
			for _, dns := range iface.DNS {
				dnsIP, err := parseAddr(dns)
				if err != nil {
					return fmt.Errorf("invalid dns server %q for host interface %s", dns, ifaceLabel)
				}
				args = append(args, dnsIP.String())
			}

			if err := runCommand("resolvectl", args...); err != nil {
				return fmt.Errorf("configure DNS for host interface %q failed: %w", iface.Name, err)
			}
		}
	}

	return nil
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
