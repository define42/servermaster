package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// nmstateApplyTimeout bounds `nmstatectl apply`'s verify-and-rollback cycle
	// (passed as --timeout). An interface that cannot reach its desired state —
	// for example a declared device that does not exist on the host — makes the
	// apply roll back and fail at this deadline instead of blocking forever. The
	// exec gets a slightly longer hard deadline (nmstateApplyTimeout + buffer) so
	// a wedged nmstatectl cannot hang the reconcile, and the /edgecommander/config
	// request that holds applyMu, indefinitely.
	nmstateApplyTimeout = 60 * time.Second
)

// nmstateStatePath is where the generated nmstate desired-state document is
// written before it is applied. The .yml extension (JSON is valid YAML) lets
// nmstate.service reapply it at boot in addition to the apply call below. It is
// a variable so tests can redirect it away from the real /etc/nmstate.
//
//nolint:gochecknoglobals // injectable seam so interface apply can be tested without touching /etc/nmstate.
var nmstateStatePath = "/etc/nmstate/edgecommander.yml"

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
	Name            string             `json:"name"`
	Type            string             `json:"type"`
	State           string             `json:"state"`
	MTU             *int               `json:"mtu,omitempty"`
	IPv4            *nmIPStack         `json:"ipv4,omitempty"`
	IPv6            *nmIPStack         `json:"ipv6,omitempty"`
	VLAN            *nmVLAN            `json:"vlan,omitempty"`
	LinkAggregation *nmLinkAggregation `json:"link-aggregation,omitempty"`
}

type nmVLAN struct {
	BaseIface string `json:"base-iface"`
	ID        int    `json:"id"`
}

// nmLinkAggregation is nmstate's bond / link-aggregation block. Port lists the
// member interfaces enslaved to the bond; Options carries the kernel bonding
// options nmstate forwards verbatim (only those declared in BondConfig are
// emitted).
type nmLinkAggregation struct {
	Mode    string         `json:"mode"`
	Port    []string       `json:"port"`
	Options *nmBondOptions `json:"options,omitempty"`
}

type nmBondOptions struct {
	Miimon  *int   `json:"miimon,omitempty"`
	Primary string `json:"primary,omitempty"`
}

type nmIPStack struct {
	Enabled bool `json:"enabled"`
	DHCP    bool `json:"dhcp"`
	// Autoconf is the IPv6 SLAAC toggle. It is a pointer so it is only emitted
	// when an ipv6_method explicitly sets it; nil leaves it (and IPv4, which has
	// no such concept) out of the document.
	Autoconf *bool `json:"autoconf,omitempty"`
	// AddrGenMode is the IPv6 addr-gen-mode (eui64 or stable-privacy); empty for
	// IPv4 and for IPv6 stacks that leave it at nmstate's default.
	AddrGenMode string      `json:"addr-gen-mode,omitempty"`
	Addresses   []nmAddress `json:"address,omitempty"`
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
	NextHopAddress   string `json:"next-hop-address,omitempty"`
	NextHopInterface string `json:"next-hop-interface"`
	// TableID and Metric mirror nmstate's route table-id and metric. They are
	// pointers so they are emitted only when explicitly set; the gateway-derived
	// default routes leave them out and so land in the main table at the default
	// metric, exactly as before.
	TableID *int `json:"table-id,omitempty"`
	Metric  *int `json:"metric,omitempty"`
}

type nmDNS struct {
	Config nmDNSConfig `json:"config"`
}

type nmDNSConfig struct {
	Server []string `json:"server,omitempty"`
}

// removeNMStateDocument deletes the nmstate desired-state file this tool
// previously wrote, so nmstate.service does not reapply a stale network config
// at the next boot. A missing file is not an error.
func removeNMStateDocument() error {
	if err := os.Remove(nmstateStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale nmstate document %q failed: %w", nmstateStatePath, err)
	}
	return nil
}

func configureHostInterfaces(interfaces []InterfaceConfig, routes []RouteConfig) error {
	// Neither interfaces nor routes are declared, so config.json is the source of
	// truth: remove any state file we previously wrote, otherwise nmstate.service
	// would reapply that stale network config at the next boot.
	if len(interfaces) == 0 && len(routes) == 0 {
		return removeNMStateDocument()
	}

	state, err := buildNMState(interfaces, routes)
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

	// Write to a temp file in the same directory and apply that. Only rename it
	// onto the canonical path once nmstatectl has accepted it, so a failed apply
	// never leaves a document that nmstate.service would reapply at boot. The
	// temp lives beside the target so the rename stays atomic on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(nmstateStatePath), ".edgecommander.*.yml.tmp")
	if err != nil {
		return fmt.Errorf("create temp nmstate document in %q failed: %w", filepath.Dir(nmstateStatePath), err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the successful rename has moved it away

	if _, err := tmp.Write(document); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp nmstate document %q failed: %w", tmpPath, err)
	}
	// nmstate.service reads this file to reapply network state at boot; it is
	// not secret, so widen CreateTemp's 0600 to the 0644 the unit expects.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp nmstate document %q failed: %w", tmpPath, err)
	}
	// fsync before the apply and rename so a power loss cannot leave
	// nmstate.service a zero-length or stale document to reapply at boot.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp nmstate document %q failed: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp nmstate document %q failed: %w", tmpPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), nmstateApplyTimeout+30*time.Second)
	defer cancel()

	nmTimeout := strconv.Itoa(int(nmstateApplyTimeout.Seconds()))
	if _, err := runCommandOutput(ctx, "nmstatectl", "apply", "--timeout", nmTimeout, tmpPath); err != nil {
		return fmt.Errorf("apply host interface configuration failed: %w", err)
	}

	if err := os.Rename(tmpPath, nmstateStatePath); err != nil {
		return fmt.Errorf("commit nmstate document to %q failed: %w", nmstateStatePath, err)
	}

	// fsync the directory so the rename is durable across a power loss.
	if err := syncDir(filepath.Dir(nmstateStatePath)); err != nil {
		return fmt.Errorf("sync nmstate dir failed: %w", err)
	}

	// nmstate has no transmit-queue-length field, so apply it through netlink
	// once the interfaces exist. The startup reconcile re-applies it on boot.
	return applyTxQueueLengths(interfaces)
}

// applyTxQueueLengths sets the transmit queue length on each interface that
// declares one (txqueuelen), looking the link up by name and setting it via
// netlink.
func applyTxQueueLengths(interfaces []InterfaceConfig) error {
	for _, iface := range interfaces {
		if iface.TxQueueLen == nil {
			continue
		}
		link, err := netlinkLinkByName(iface.Name)
		if err != nil {
			return fmt.Errorf("look up interface %q to set txqueuelen: %w", iface.Name, err)
		}
		if err := netlinkLinkSetTxQLen(link, *iface.TxQueueLen); err != nil {
			return fmt.Errorf("set txqueuelen for interface %q: %w", iface.Name, err)
		}
		log.Printf("set txqueuelen %d on interface %s", *iface.TxQueueLen, iface.Name)
	}
	return nil
}

// buildNMState translates the tool's interface config into an nmstate desired
// state. It keeps the original validation (name required, ip_address/subnet
// paired, addresses inside their subnet, parseable gateway/DNS). DNS servers
// from every interface are merged into nmstate's single global resolver list,
// de-duplicated in first-seen order. When no DNS servers are declared, an empty
// resolver config is emitted so nmstate clears any stale DNS left by a previous
// config instead of preserving it.
func buildNMState(interfaces []InterfaceConfig, routes []RouteConfig) (*nmState, error) {
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
			appendRoute(state, *route)
		}

		dnsServers, err = appendInterfaceDNS(iface, label, seenDNS, dnsServers)
		if err != nil {
			return nil, err
		}
	}

	// Explicitly declared routes follow the gateway-derived ones, in config
	// order. nmstate applies them to the interfaces they name (which need not be
	// declared here — they may be DHCP-managed or pre-existing).
	for i, route := range routes {
		label := routeLabel(route, i)
		nmRoute, err := buildNMRoute(route, label)
		if err != nil {
			return nil, err
		}
		appendRoute(state, nmRoute)
	}

	state.DNSResolver = &nmDNS{Config: nmDNSConfig{Server: dnsServers}}

	return state, nil
}

// appendRoute adds a route to the state's route config, allocating the routes
// container on first use.
func appendRoute(state *nmState, route nmRoute) {
	if state.Routes == nil {
		state.Routes = &nmRoutes{}
	}
	state.Routes.Config = append(state.Routes.Config, route)
}

// buildNMInterface translates one InterfaceConfig into an nmstate interface,
// validating its name, paired ip_address/subnet, optional MTU/VLAN settings, and
// (when set) its static address.
// maxMTU is the largest MTU value representable by the kernel's 32-bit
// interface MTU attribute. Device-specific min/max limits are enforced by
// nmstate/NetworkManager when the desired state is applied.
const maxMTU = 0xFFFFFFFF

// validateMTU checks a declared MTU is in the generic kernel range. A nil value
// (unset) is valid and leaves the MTU untouched.
func validateMTU(iface InterfaceConfig, label string) error {
	if iface.MTU == nil {
		return nil
	}
	if *iface.MTU < 1 || uint64(*iface.MTU) > maxMTU {
		return fmt.Errorf("host interface %s mtu %d must be between 1 and %d", label, *iface.MTU, uint64(maxMTU))
	}
	return nil
}

// maxTxQueueLen is the largest accepted transmit queue length: the kernel's
// tx_queue_len is a 32-bit unsigned value.
const maxTxQueueLen = 0xFFFFFFFF

// validateTxQueueLen checks a declared txqueuelen is within the kernel's range.
// A nil value (unset) is valid and leaves the queue length untouched.
func validateTxQueueLen(iface InterfaceConfig, label string) error {
	if iface.TxQueueLen == nil {
		return nil
	}
	if *iface.TxQueueLen < 0 || *iface.TxQueueLen > maxTxQueueLen {
		return fmt.Errorf("host interface %s txqueuelen %d must be between 0 and %d", label, *iface.TxQueueLen, maxTxQueueLen)
	}
	return nil
}

func buildNMInterface(iface InterfaceConfig, label string) (nmInterface, error) {
	if iface.Name == "" {
		return nmInterface{}, fmt.Errorf("host interface %s is missing name", label)
	}

	if err := validateMTU(iface, label); err != nil {
		return nmInterface{}, err
	}

	if err := validateTxQueueLen(iface, label); err != nil {
		return nmInterface{}, err
	}

	if (iface.IPAddress == "") != (iface.Subnet == "") {
		return nmInterface{}, fmt.Errorf("host interface %q must set both ip_address and subnet", iface.Name)
	}

	// Defaults to a physical NIC (the documented use case, e.g. eth0). An
	// explicit type lets nmstate manage other kinds it supports — "dummy" for
	// a software test interface, "vlan" for an 802.1Q tagged interface, or
	// "bond" for a Linux link-aggregation interface. nmstate validates the
	// value at apply time. Bridges need extra params and remain out of scope.
	ifaceType := strings.TrimSpace(iface.Type)
	if ifaceType == "" {
		ifaceType = "ethernet"
	}
	nmIface := nmInterface{Name: iface.Name, Type: ifaceType, State: "up", MTU: iface.MTU}

	vlan, err := interfaceVLAN(iface, label, ifaceType)
	if err != nil {
		return nmInterface{}, err
	}
	nmIface.VLAN = vlan

	bond, err := interfaceBond(iface, label, ifaceType)
	if err != nil {
		return nmInterface{}, err
	}
	nmIface.LinkAggregation = bond

	if err := applyStaticAddress(&nmIface, iface, label); err != nil {
		return nmInterface{}, err
	}

	if err := applyIPv4Settings(&nmIface, iface, label); err != nil {
		return nmInterface{}, err
	}

	if err := applyIPv6Settings(&nmIface, iface, label); err != nil {
		return nmInterface{}, err
	}

	return nmIface, nil
}

// applyStaticAddress sets nmIface's IPv4 or IPv6 static address (whichever
// family ip_address belongs to) when one is declared. The interface-config
// check that ip_address and subnet are declared together has already run.
func applyStaticAddress(nmIface *nmInterface, iface InterfaceConfig, label string) error {
	if iface.IPAddress == "" {
		return nil
	}
	ipNet, err := parseInterfaceAddress(iface.IPAddress, iface.Subnet)
	if err != nil {
		return fmt.Errorf("invalid host interface %s address: %w", label, err)
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
	return nil
}

// ipv4MethodStack builds the nmstate IPv4 stack for a supported ipv4_method,
// mirroring a NetworkManager ipv4.method. It reports false for an unrecognized
// method so the caller can reject it. IPv4 has no SLAAC, so "auto" is DHCP.
func ipv4MethodStack(method string) (*nmIPStack, bool) {
	switch method {
	case "dhcp", "auto":
		return &nmIPStack{Enabled: true, DHCP: true}, true
	case "disabled":
		return &nmIPStack{Enabled: false, DHCP: false}, true
	default:
		return nil, false
	}
}

// applyIPv4Settings layers the optional ipv4_method onto nmIface. The
// static-address path in buildNMInterface has already run, so nmIface.IPv4 is
// non-nil exactly when a static IPv4 ip_address was declared (the "manual"
// method), which ipv4_method must not contradict.
func applyIPv4Settings(nmIface *nmInterface, iface InterfaceConfig, label string) error {
	method := strings.TrimSpace(iface.IPv4Method)
	if method == "" {
		return nil
	}
	if nmIface.IPv4 != nil {
		return fmt.Errorf("host interface %s sets ipv4_method %q together with a static IPv4 ip_address; they are mutually exclusive", label, method)
	}
	stack, ok := ipv4MethodStack(method)
	if !ok {
		return fmt.Errorf("host interface %s has invalid ipv4_method %q (want dhcp, auto, or disabled)", label, method)
	}
	nmIface.IPv4 = stack
	return nil
}

// ipv6MethodStack builds the nmstate IPv6 stack for a supported ipv6_method,
// each mirroring a NetworkManager ipv6.method. It reports false for an
// unrecognized method so the caller can reject it.
func ipv6MethodStack(method string) (*nmIPStack, bool) {
	on, off := true, false
	switch method {
	case "link-local":
		// IPv6 up with only the auto-generated link-local address.
		return &nmIPStack{Enabled: true, DHCP: false, Autoconf: &off}, true
	case "auto":
		// SLAAC plus DHCPv6, the router-advertisement-driven default.
		return &nmIPStack{Enabled: true, DHCP: true, Autoconf: &on}, true
	case "dhcp":
		// DHCPv6 only, without SLAAC.
		return &nmIPStack{Enabled: true, DHCP: true, Autoconf: &off}, true
	case "disabled":
		return &nmIPStack{Enabled: false, DHCP: false}, true
	default:
		return nil, false
	}
}

// validIPv6AddrGenMode reports whether mode is an addr-gen-mode nmstate accepts.
func validIPv6AddrGenMode(mode string) bool {
	switch mode {
	case "eui64", "stable-privacy":
		return true
	default:
		return false
	}
}

// applyIPv6Settings layers the optional ipv6_method and ipv6_addr_gen_mode onto
// nmIface. The static-address path in buildNMInterface has already run, so
// nmIface.IPv6 is non-nil exactly when a static IPv6 ip_address was declared
// (the "manual" method), which ipv6_method must not contradict.
func applyIPv6Settings(nmIface *nmInterface, iface InterfaceConfig, label string) error {
	method := strings.TrimSpace(iface.IPv6Method)
	genMode := strings.TrimSpace(iface.IPv6AddrGenMode)

	if method != "" {
		if nmIface.IPv6 != nil {
			return fmt.Errorf("host interface %s sets ipv6_method %q together with a static IPv6 ip_address; they are mutually exclusive", label, method)
		}
		stack, ok := ipv6MethodStack(method)
		if !ok {
			return fmt.Errorf("host interface %s has invalid ipv6_method %q (want link-local, auto, dhcp, or disabled)", label, method)
		}
		nmIface.IPv6 = stack
	}

	if genMode != "" {
		if !validIPv6AddrGenMode(genMode) {
			return fmt.Errorf("host interface %s has invalid ipv6_addr_gen_mode %q (want eui64 or stable-privacy)", label, genMode)
		}
		if nmIface.IPv6 == nil || !nmIface.IPv6.Enabled {
			return fmt.Errorf("host interface %s sets ipv6_addr_gen_mode %q but IPv6 is not enabled; set ipv6_method or a static IPv6 ip_address", label, genMode)
		}
		nmIface.IPv6.AddrGenMode = genMode
	}

	return nil
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

// validBondModes lists the kernel bonding modes nmstate accepts. Mode names
// match the kernel/nmstate identifiers; the legacy numeric aliases are not
// supported.
//
//nolint:gochecknoglobals // immutable lookup set shared by validation.
var validBondModes = map[string]struct{}{
	"balance-rr":    {},
	"active-backup": {},
	"balance-xor":   {},
	"broadcast":     {},
	"802.3ad":       {},
	"balance-tlb":   {},
	"balance-alb":   {},
}

// primarySupportingBondModes lists the bonding modes where a single member is
// preferred as the active link and the `primary` option therefore applies.
//
//nolint:gochecknoglobals // immutable lookup set shared by validation.
var primarySupportingBondModes = map[string]struct{}{
	"active-backup": {},
	"balance-tlb":   {},
	"balance-alb":   {},
}

// maxBondMiimon is the largest accepted value for the bond `miimon` option:
// the kernel stores it in an unsigned 32-bit field.
const maxBondMiimon = 0xFFFFFFFF

// interfaceBond validates and builds an interface's bond settings. It returns
// nil when the interface is not a bond, and an error when bond settings are
// missing for a "bond" interface or present on a non-bond one.
func interfaceBond(iface InterfaceConfig, label, ifaceType string) (*nmLinkAggregation, error) {
	switch {
	case ifaceType == "bond":
		if iface.Bond == nil {
			return nil, fmt.Errorf("host interface %s is type bond but has no bond settings", label)
		}
		return buildBond(iface, label)
	case iface.Bond != nil:
		return nil, fmt.Errorf("host interface %s sets bond settings but type is %q, not \"bond\"", label, ifaceType)
	default:
		return nil, nil
	}
}

// buildBond validates a bond's mode, members, and options and returns the
// nmstate link-aggregation block. The caller has already confirmed iface.Bond
// is non-nil.
func buildBond(iface InterfaceConfig, label string) (*nmLinkAggregation, error) {
	bond := iface.Bond
	mode := strings.TrimSpace(bond.Mode)
	if mode == "" {
		return nil, fmt.Errorf("host interface %s bond is missing mode", label)
	}
	if _, ok := validBondModes[mode]; !ok {
		return nil, fmt.Errorf("host interface %s bond has invalid mode %q (want balance-rr, active-backup, balance-xor, broadcast, 802.3ad, balance-tlb, or balance-alb)", label, bond.Mode)
	}

	if len(bond.Ports) == 0 {
		return nil, fmt.Errorf("host interface %s bond must list at least one port", label)
	}

	ports := make([]string, 0, len(bond.Ports))
	seen := make(map[string]struct{}, len(bond.Ports))
	for i, raw := range bond.Ports {
		port := strings.TrimSpace(raw)
		if port == "" {
			return nil, fmt.Errorf("host interface %s bond port #%d is empty", label, i)
		}
		if port == iface.Name {
			return nil, fmt.Errorf("host interface %s bond cannot list itself %q as a port", label, port)
		}
		if _, dup := seen[port]; dup {
			return nil, fmt.Errorf("host interface %s bond lists port %q more than once", label, port)
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}

	agg := &nmLinkAggregation{Mode: mode, Port: ports}

	options, err := buildBondOptions(bond, mode, label, seen)
	if err != nil {
		return nil, err
	}
	agg.Options = options

	return agg, nil
}

// buildBondOptions validates and assembles the optional bond options (miimon,
// primary). It returns nil when no options are declared so the nmstate document
// stays minimal. ports is the set of declared port names, used to confirm
// primary names a real member.
func buildBondOptions(bond *BondConfig, mode, label string, ports map[string]struct{}) (*nmBondOptions, error) {
	var opts *nmBondOptions

	if bond.Miimon != nil {
		if *bond.Miimon < 0 || uint64(*bond.Miimon) > maxBondMiimon {
			return nil, fmt.Errorf("host interface %s bond miimon %d must be between 0 and %d", label, *bond.Miimon, uint64(maxBondMiimon))
		}
		opts = &nmBondOptions{Miimon: bond.Miimon}
	}

	if primary := strings.TrimSpace(bond.Primary); primary != "" {
		if _, ok := primarySupportingBondModes[mode]; !ok {
			return nil, fmt.Errorf("host interface %s bond primary %q is only valid for modes active-backup, balance-tlb, or balance-alb (got %q)", label, primary, mode)
		}
		if _, ok := ports[primary]; !ok {
			return nil, fmt.Errorf("host interface %s bond primary %q is not listed in ports", label, primary)
		}
		if opts == nil {
			opts = &nmBondOptions{}
		}
		opts.Primary = primary
	}

	return opts, nil
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

// maxRouteU32 is the largest value the kernel's 32-bit route table-id and metric
// fields accept.
const maxRouteU32 = 0xFFFFFFFF

// routeLabel names a route for validation messages, preferring its declared
// name, then its destination, then a positional index.
func routeLabel(route RouteConfig, i int) string {
	if name := strings.TrimSpace(route.Name); name != "" {
		return name
	}
	return labelOrIndex(route.Destination, i)
}

// buildNMRoute validates a declared static route and translates it into an
// nmstate route entry. It accepts a default route (destination "0.0.0.0/0" or
// "::/0") or any network destination, an optional next-hop gateway (which must
// match the destination's IP family), an optional routing table-id, and an
// optional metric. The destination is normalized to its network address so
// nmstate, which keys routes by destination, sees a canonical value.
func buildNMRoute(route RouteConfig, label string) (nmRoute, error) {
	destination := strings.TrimSpace(route.Destination)
	if destination == "" {
		return nmRoute{}, fmt.Errorf("route %s is missing destination", label)
	}
	prefix, err := netip.ParsePrefix(destination)
	if err != nil {
		return nmRoute{}, fmt.Errorf("route %s has invalid destination %q: want a CIDR network such as 0.0.0.0/0 or 10.0.0.0/8", label, route.Destination)
	}

	iface := strings.TrimSpace(route.NextHopInterface)
	if iface == "" {
		return nmRoute{}, fmt.Errorf("route %s is missing next_hop_interface", label)
	}

	nmRoute := nmRoute{
		Destination:      prefix.Masked().String(),
		NextHopInterface: iface,
	}

	if nextHop := strings.TrimSpace(route.NextHopAddress); nextHop != "" {
		gateway, err := netip.ParseAddr(nextHop)
		if err != nil {
			return nmRoute, fmt.Errorf("route %s has invalid next_hop_address %q", label, route.NextHopAddress)
		}
		if gateway.Is4() != prefix.Addr().Is4() {
			return nmRoute, fmt.Errorf("route %s next_hop_address %q and destination %q are different IP families", label, route.NextHopAddress, route.Destination)
		}
		nmRoute.NextHopAddress = gateway.String()
	}

	if err := validateRouteUint32(route.TableID, "table_id", label); err != nil {
		return nmRoute, err
	}
	nmRoute.TableID = route.TableID

	if err := validateRouteUint32(route.Metric, "metric", label); err != nil {
		return nmRoute, err
	}
	nmRoute.Metric = route.Metric

	return nmRoute, nil
}

// validateRouteUint32 checks an optional route attribute (table_id or metric)
// fits the kernel's 32-bit unsigned range. A nil value (unset) is valid.
func validateRouteUint32(value *int, field, label string) error {
	if value == nil {
		return nil
	}
	if *value < 0 || uint64(*value) > maxRouteU32 {
		return fmt.Errorf("route %s %s %d must be between 0 and %d", label, field, *value, uint64(maxRouteU32))
	}
	return nil
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
