package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// sysClassNetPath is the sysfs network-device tree, used to read per-interface
// link speed. It is a variable so tests can supply a fixture tree.
//
//nolint:gochecknoglobals // injectable seam so interface speed can be tested with a fixture sysfs tree.
var sysClassNetPath = "/sys/class/net"

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
	Name       string `json:"name"`
	Index      int    `json:"index"`
	Type       string `json:"type,omitempty"`
	State      string `json:"state,omitempty"`
	MAC        string `json:"mac_address,omitempty"`
	MTU        int    `json:"mtu,omitempty"`
	SpeedMbps  int    `json:"speed_mbps,omitempty"`
	TxQueueLen int    `json:"txqueuelen"`
	// Addressing summarizes how the interface's global addresses were assigned:
	// "static", "dhcp", or "" when it has none (loopback, link-local only). It is
	// inferred from the kernel's permanent-address flag, not the configured intent.
	Addressing string           `json:"addressing,omitempty"`
	Flags      []string         `json:"flags,omitempty"`
	Addresses  []networkAddress `json:"addresses,omitempty"`
	Statistics *interfaceStats  `json:"statistics,omitempty"`
}

type networkAddress struct {
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefix_length"`
	Family       string `json:"family"`
	// Dynamic is true when the address is lease-based (DHCP or router
	// advertisement) rather than a permanent, statically configured address —
	// the same distinction `ip addr` shows as "dynamic".
	Dynamic bool `json:"dynamic"`
}

// interfaceStats are the kernel's cumulative per-interface counters — the same
// figures `ifconfig`/`ip -s link` report. overruns are FIFO errors, frame is RX
// frame errors, and carrier is TX carrier errors.
type interfaceStats struct {
	RXPackets  uint64 `json:"rx_packets"`
	RXBytes    uint64 `json:"rx_bytes"`
	RXErrors   uint64 `json:"rx_errors"`
	RXDropped  uint64 `json:"rx_dropped"`
	RXOverruns uint64 `json:"rx_overruns"`
	RXFrame    uint64 `json:"rx_frame"`
	TXPackets  uint64 `json:"tx_packets"`
	TXBytes    uint64 `json:"tx_bytes"`
	TXErrors   uint64 `json:"tx_errors"`
	TXDropped  uint64 `json:"tx_dropped"`
	TXOverruns uint64 `json:"tx_overruns"`
	TXCarrier  uint64 `json:"tx_carrier"`
	Collisions uint64 `json:"collisions"`
}

type networkRoute struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface,omitempty"`
	Family      string `json:"family,omitempty"`
}

// netlink access points are indirected through variables so tests can supply
// fixtures without a live kernel/network namespace. resolvConfPath is the file
// the resolver nameservers are read from.
//
//nolint:gochecknoglobals // injectable seams so network status can be tested without a live kernel.
var (
	netlinkLinkList      = netlink.LinkList
	netlinkAddrList      = netlink.AddrList
	netlinkRouteList     = netlink.RouteList
	netlinkLinkByName    = netlink.LinkByName
	netlinkLinkSetTxQLen = netlink.LinkSetTxQLen
	resolvConfPath       = "/etc/resolv.conf"
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
			Name:       attrs.Name,
			Index:      attrs.Index,
			Type:       link.Type(),
			State:      attrs.OperState.String(),
			MTU:        attrs.MTU,
			SpeedMbps:  interfaceSpeed(attrs.Name),
			TxQueueLen: attrs.TxQLen,
			Flags:      interfaceFlags(attrs.Flags),
			Statistics: interfaceStatistics(attrs.Statistics),
		}
		if len(attrs.HardwareAddr) > 0 {
			iface.MAC = attrs.HardwareAddr.String()
		}

		addrs, addrErr := interfaceAddresses(link)
		if addrErr != nil {
			status.Error = appendStatusError(status.Error, fmt.Sprintf("addresses for %s: %v", attrs.Name, addrErr))
		}
		iface.Addresses = addrs
		iface.Addressing = addressingMethod(addrs)

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
			// A permanent address is statically configured; anything else is
			// lease-based (DHCP or a router advertisement).
			Dynamic: addr.Flags&unix.IFA_F_PERMANENT == 0,
		})
	}
	return result, nil
}

// addressingMethod summarizes how an interface's global addresses were assigned:
// "dhcp" when any is dynamic (lease-based), "static" when it has only permanent
// global addresses, and "" when it has none (such as loopback or a link-local
// only interface). Link-local and loopback addresses are ignored. This is a
// heuristic from the kernel's permanent flag, not the configured intent.
func addressingMethod(addresses []networkAddress) string {
	method := ""
	for _, addr := range addresses {
		ip := net.ParseIP(addr.IP)
		if ip == nil || !ip.IsGlobalUnicast() {
			continue
		}
		if addr.Dynamic {
			return "dhcp"
		}
		method = "static"
	}
	return method
}

// interfaceSpeed returns an interface's link speed in Mbit/s, read from sysfs.
// Interfaces without a meaningful speed — loopback, dummy, or a down link —
// report an error or a negative value there, which is normalized to 0 (and so
// omitted from the JSON).
func interfaceSpeed(name string) int {
	data, err := os.ReadFile(filepath.Join(sysClassNetPath, name, "speed")) //nolint:gosec // name comes from the kernel link list, not request input.
	if err != nil {
		return 0
	}
	speed, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || speed < 0 {
		return 0
	}
	return speed
}

// interfaceStatistics maps the kernel's per-interface counters into the reported
// statistics, following ifconfig's naming (overruns are FIFO errors, frame is RX
// frame errors, carrier is TX carrier errors). It returns nil when the kernel
// provided no statistics for the link.
func interfaceStatistics(stats *netlink.LinkStatistics) *interfaceStats {
	if stats == nil {
		return nil
	}
	return &interfaceStats{
		RXPackets:  stats.RxPackets,
		RXBytes:    stats.RxBytes,
		RXErrors:   stats.RxErrors,
		RXDropped:  stats.RxDropped,
		RXOverruns: stats.RxFifoErrors,
		RXFrame:    stats.RxFrameErrors,
		TXPackets:  stats.TxPackets,
		TXBytes:    stats.TxBytes,
		TXErrors:   stats.TxErrors,
		TXDropped:  stats.TxDropped,
		TXOverruns: stats.TxFifoErrors,
		TXCarrier:  stats.TxCarrierErrors,
		Collisions: stats.Collisions,
	}
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
