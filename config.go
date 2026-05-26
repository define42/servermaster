package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	PodmanMode    string               `json:"podman_mode"`
	Hostname      string               `json:"hostname"`
	Folders       []FolderConfig       `json:"folders"`
	Files         []FileConfig         `json:"files"`
	Interfaces    []InterfaceConfig    `json:"interfaces"`
	Routes        []RouteConfig        `json:"routes,omitempty"`
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
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	IPAddress string   `json:"ip_address"`
	Subnet    string   `json:"subnet"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
	// MTU, when set, is the interface maximum transmission unit applied by
	// nmstate through NetworkManager. A nil value leaves it untouched.
	MTU *int `json:"mtu,omitempty"`
	// IPv4Method mirrors NetworkManager's ipv4.method for interfaces without a
	// static IPv4 ip_address. "" leaves IPv4 at nmstate's default; "dhcp" (or its
	// alias "auto", since IPv4 has no SLAAC) leases an address over DHCP;
	// "disabled" turns IPv4 off. It is mutually exclusive with a static IPv4
	// ip_address, which is itself the "manual" method.
	IPv4Method string `json:"ipv4_method,omitempty"`
	// IPv6Method mirrors NetworkManager's ipv6.method for interfaces without a
	// static IPv6 ip_address. "" leaves IPv6 at nmstate's default; "link-local"
	// enables IPv6 with only the auto-generated link-local address (no DHCPv6,
	// no SLAAC global address); "auto" uses SLAAC/DHCPv6; "dhcp" uses DHCPv6
	// only; "disabled" turns IPv6 off. It is mutually exclusive with a static
	// IPv6 ip_address, which is itself the "manual" method.
	IPv6Method string `json:"ipv6_method,omitempty"`
	// IPv6AddrGenMode mirrors nmcli's ipv6.addr-gen-mode, selecting how the IPv6
	// interface identifier is generated: "eui64" or "stable-privacy". "" leaves
	// it at nmstate's default. It requires IPv6 to be enabled, via IPv6Method or
	// a static IPv6 ip_address.
	IPv6AddrGenMode string `json:"ipv6_addr_gen_mode,omitempty"`
	// TxQueueLen, when set, is the transmit queue length applied to the interface
	// (the txqueuelen `ip link` reports). nmstate has no field for it, so it is
	// applied via netlink after the nmstate apply. A nil value leaves it untouched.
	TxQueueLen *int        `json:"txqueuelen,omitempty"`
	VLAN       *VLANConfig `json:"vlan,omitempty"`
}

// VLANConfig describes an 802.1Q VLAN interface (type "vlan"): the VLAN rides on
// BaseInterface and is tagged with ID. The interface Name is the VLAN device's
// own name, conventionally "<base>.<id>" such as "eth0.100".
type VLANConfig struct {
	BaseInterface string `json:"base_interface"`
	ID            int    `json:"id"`
}

// RouteConfig is a static route installed through nmstate, and so persisted
// across reboots and reapplied by nmstate.service just like the interface
// config. It covers a default route (Destination "0.0.0.0/0" or "::/0") and,
// via TableID, a route in a non-main routing table for policy routing. Routes
// declared here are additive to the per-interface default routes derived from
// each interface's gateway.
type RouteConfig struct {
	// Name is an optional human-readable label for the route. nmstate has no
	// per-route name, so it is not applied to the kernel; it documents the route
	// in the config and is used to identify it in validation error messages.
	Name string `json:"name,omitempty"`
	// Destination is the route's target network in CIDR form: "0.0.0.0/0" or
	// "::/0" for a default route, or a specific network such as "10.0.0.0/8".
	Destination string `json:"destination"`
	// NextHopInterface is the egress interface for the route. nmstate requires a
	// next-hop interface on every route, so it is mandatory here too.
	NextHopInterface string `json:"next_hop_interface"`
	// NextHopAddress is the gateway the route forwards through. Optional: omit it
	// for an on-link route reached directly over NextHopInterface. When set it
	// must share the destination's IP family.
	NextHopAddress string `json:"next_hop_address,omitempty"`
	// TableID selects the kernel routing table the route is installed in,
	// mirroring nmstate's route table-id. Omitting it (or 0) uses the main table
	// (254). A nil value leaves it at nmstate's default.
	TableID *int `json:"table_id,omitempty"`
	// Metric is the route priority/metric (lower wins among equal destinations).
	// A nil value lets nmstate and the kernel pick the default.
	Metric *int `json:"metric,omitempty"`
}

type FirewallPortConfig struct {
	Zone     string `json:"zone"`
	Port     string `json:"port"`
	Protocol string `json:"protocol"`
	Source   string `json:"source,omitempty"`
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

// validateHostname checks a declared hostname against RFC 1123: a dot-separated
// series of labels of letters, digits, and hyphens, each 1-63 characters and not
// hyphen-bounded, totalling at most 253 characters. An empty hostname is valid
// and means the host's hostname is left unmanaged.
func validateHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	if len(hostname) > 253 {
		return fmt.Errorf("hostname %q exceeds 253 characters", hostname)
	}
	for _, label := range strings.Split(hostname, ".") {
		if err := validateHostnameLabel(label); err != nil {
			return err
		}
	}
	return nil
}

func validateHostnameLabel(label string) error {
	if label == "" {
		return fmt.Errorf("hostname has an empty label")
	}
	if len(label) > 63 {
		return fmt.Errorf("hostname label %q exceeds 63 characters", label)
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return fmt.Errorf("hostname label %q must not start or end with a hyphen", label)
	}
	for _, r := range label {
		if !isHostnameChar(r) {
			return fmt.Errorf("hostname label %q contains an invalid character", label)
		}
	}
	return nil
}

func isHostnameChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
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
	if err := validateHostname(cfg.Hostname); err != nil {
		return err
	}
	if err := validateFolders(cfg.Folders); err != nil {
		return err
	}
	if err := validateFiles(cfg.Files); err != nil {
		return err
	}
	// buildNMState validates the interface and route config (names, paired
	// ip/subnet, addresses within subnet, parseable gateway/DNS, route
	// destinations/next hops/table ids) without side effects.
	if _, err := buildNMState(cfg.Interfaces, cfg.Routes); err != nil {
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
		if err := validateFirewallSource(port.Source); err != nil {
			return fmt.Errorf("invalid firewall source for port %s: %w", portLabel, err)
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

func validateFirewallSource(source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil
	}
	if _, err := netip.ParseAddr(source); err == nil {
		return nil
	}
	if _, err := netip.ParsePrefix(source); err == nil {
		return nil
	}
	return fmt.Errorf("source must be a valid IP address or CIDR")
}
