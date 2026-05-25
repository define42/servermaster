package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestConvertRouteIPv6Default(t *testing.T) {
	// No Dst and an IPv6 family yields the IPv6 default route destination.
	got := convertRoute(netlink.Route{Family: netlink.FAMILY_V6, LinkIndex: 2}, map[int]string{2: "eth0"})
	if got.Destination != "::/0" || got.Interface != "eth0" {
		t.Fatalf("route = %+v, want ::/0 on eth0", got)
	}
	// IPv4 family with no Dst yields the IPv4 default route.
	if got := convertRoute(netlink.Route{Family: netlink.FAMILY_V4}, nil); got.Destination != "0.0.0.0/0" {
		t.Fatalf("route dest = %q, want 0.0.0.0/0", got.Destination)
	}
}

func TestInterfaceSpeed(t *testing.T) {
	prev := sysClassNetPath
	defer func() { sysClassNetPath = prev }()
	root := t.TempDir()
	sysClassNetPath = root

	writeSpeedFile(t, root, "eth0", "1000\n")
	writeSpeedFile(t, root, "dummy0", "-1\n") // interfaces without a speed report -1

	if got := interfaceSpeed("eth0"); got != 1000 {
		t.Fatalf("interfaceSpeed(eth0) = %d, want 1000", got)
	}
	if got := interfaceSpeed("dummy0"); got != 0 {
		t.Fatalf("interfaceSpeed(dummy0) = %d, want 0 (negative normalized)", got)
	}
	if got := interfaceSpeed("eth1"); got != 0 {
		t.Fatalf("interfaceSpeed(eth1) = %d, want 0 (no speed file)", got)
	}
}

func writeSpeedFile(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speed"), []byte(content), 0o600); err != nil {
		t.Fatalf("write speed: %v", err)
	}
}

func TestInterfaceStatistics(t *testing.T) {
	if interfaceStatistics(nil) != nil {
		t.Fatal("nil kernel statistics must map to nil")
	}

	got := interfaceStatistics(&netlink.LinkStatistics{
		RxPackets: 56436, RxBytes: 188213261, RxErrors: 1, RxDropped: 2, RxFifoErrors: 3, RxFrameErrors: 4,
		TxPackets: 38747, TxBytes: 8678923, TxErrors: 5, TxDropped: 6, TxFifoErrors: 7, TxCarrierErrors: 8,
		Collisions: 9,
	})
	want := &interfaceStats{
		RXPackets: 56436, RXBytes: 188213261, RXErrors: 1, RXDropped: 2, RXOverruns: 3, RXFrame: 4,
		TXPackets: 38747, TXBytes: 8678923, TXErrors: 5, TXDropped: 6, TXOverruns: 7, TXCarrier: 8,
		Collisions: 9,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statistics = %+v, want %+v", got, want)
	}
}

func TestInterfaceAddressesSkipsNilIPNet(t *testing.T) {
	prev := netlinkAddrList
	netlinkAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
		return []netlink.Addr{
			{IPNet: nil},
			{IPNet: cidr("10.0.0.5/24"), Flags: unix.IFA_F_PERMANENT}, // static
			{IPNet: cidr("10.0.0.6/24")},                              // lease-based
		}, nil
	}
	defer func() { netlinkAddrList = prev }()

	got, err := interfaceAddresses(&netlink.Device{})
	if err != nil {
		t.Fatalf("interfaceAddresses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("addresses = %+v, want the two with an IPNet", got)
	}
	if got[0].IP != "10.0.0.5" || got[0].Dynamic {
		t.Fatalf("permanent address mismapped: %+v", got[0])
	}
	if !got[1].Dynamic {
		t.Fatalf("non-permanent address should be dynamic: %+v", got[1])
	}
}

func TestAddressingMethod(t *testing.T) {
	cases := []struct {
		name string
		addr []networkAddress
		want string
	}{
		{"none", nil, ""},
		{"link-local only ignored", []networkAddress{{IP: "fe80::1"}, {IP: "127.0.0.1"}}, ""},
		{"static global", []networkAddress{{IP: "192.168.1.10"}}, "static"},
		{"dynamic global", []networkAddress{{IP: "192.168.1.20", Dynamic: true}}, "dhcp"},
		{"mixed prefers dhcp", []networkAddress{{IP: "192.168.1.10"}, {IP: "2001:db8::5", Dynamic: true}}, "dhcp"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := addressingMethod(tt.addr); got != tt.want {
				t.Fatalf("addressingMethod = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRouteFamily(t *testing.T) {
	cases := []struct {
		name  string
		route netlink.Route
		want  string
	}{
		{"explicit v4", netlink.Route{Family: netlink.FAMILY_V4}, "ipv4"},
		{"explicit v6", netlink.Route{Family: netlink.FAMILY_V6}, "ipv6"},
		{"infer from dst", netlink.Route{Dst: cidr("2001:db8::/64")}, "ipv6"},
		{"infer from gw", netlink.Route{Gw: net.ParseIP("192.168.0.1")}, "ipv4"},
		{"default", netlink.Route{}, "ipv4"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeFamily(tt.route); got != tt.want {
				t.Fatalf("routeFamily = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterfaceFlags(t *testing.T) {
	if got := interfaceFlags(0); got != nil {
		t.Fatalf("flags(0) = %v, want nil", got)
	}
	got := interfaceFlags(net.FlagUp | net.FlagBroadcast)
	if len(got) != 2 || got[0] != "up" {
		t.Fatalf("flags = %v, want [up broadcast]", got)
	}
}

// device builds a fake netlink link for tests. netlink.Device.Type() reports
// "device", matching what LinkList returns for a physical interface.
func device(index int, name, mac string, mtu int, state netlink.LinkOperState, flags net.Flags) *netlink.Device {
	attrs := netlink.LinkAttrs{
		Index:     index,
		Name:      name,
		MTU:       mtu,
		TxQLen:    1000,
		OperState: state,
		Flags:     flags,
	}
	if mac != "" {
		hw, err := net.ParseMAC(mac)
		if err != nil {
			panic(err)
		}
		attrs.HardwareAddr = hw
	}
	return &netlink.Device{LinkAttrs: attrs}
}

func cidr(s string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	ipNet.IP = ip
	return ipNet
}

func stubNetlink(links []netlink.Link, addrs map[int][]netlink.Addr, routes []netlink.Route, routesErr error) func() {
	prevLink, prevAddr, prevRoute := netlinkLinkList, netlinkAddrList, netlinkRouteList

	netlinkLinkList = func() ([]netlink.Link, error) { return links, nil }
	netlinkAddrList = func(link netlink.Link, _ int) ([]netlink.Addr, error) {
		return addrs[link.Attrs().Index], nil
	}
	netlinkRouteList = func(netlink.Link, int) ([]netlink.Route, error) { return routes, routesErr }

	return func() {
		netlinkLinkList, netlinkAddrList, netlinkRouteList = prevLink, prevAddr, prevRoute
	}
}

// testNetworkLinks returns the eth0/lo fixture links and addresses shared by the
// network-status tests.
func testNetworkLinks() ([]netlink.Link, map[int][]netlink.Addr) {
	links := []netlink.Link{
		device(2, "eth0", "52:54:00:12:34:56", 1500, netlink.OperUp, net.FlagUp|net.FlagBroadcast),
		device(1, "lo", "", 65536, netlink.OperUnknown, net.FlagUp|net.FlagLoopback),
	}
	addrs := map[int][]netlink.Addr{
		2: {
			// IFA_F_PERMANENT marks the global address as statically configured.
			{IPNet: cidr("192.168.1.10/24"), Flags: unix.IFA_F_PERMANENT},
			{IPNet: cidr("fe80::1/64")},
		},
	}
	return links, addrs
}

func TestCollectNetworkStatus(t *testing.T) {
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("# comment\nnameserver 1.1.1.1\nnameserver 8.8.8.8\nsearch example.com\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}
	prevResolv := resolvConfPath
	resolvConfPath = resolv
	defer func() { resolvConfPath = prevResolv }()

	// Point sysfs at an empty tree so interface speeds are read as unavailable
	// rather than picking up the test host's real NICs.
	prevSysNet := sysClassNetPath
	sysClassNetPath = t.TempDir()
	defer func() { sysClassNetPath = prevSysNet }()

	links, addrs := testNetworkLinks()
	routes := []netlink.Route{
		{LinkIndex: 2, Gw: net.ParseIP("192.168.1.1"), Family: netlink.FAMILY_V4},
		{LinkIndex: 2, Dst: cidr("192.168.1.0/24"), Family: netlink.FAMILY_V4},
	}
	defer stubNetlink(links, addrs, routes, nil)()

	status := collectNetworkStatus(context.Background())

	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if status.Source != "netlink" {
		t.Fatalf("source = %q, want netlink", status.Source)
	}
	assertNetworkInterfaces(t, status.Interfaces)
	assertNetworkRoutes(t, status.Routes)
	if !reflect.DeepEqual(status.DNS, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("dns = %v, want [1.1.1.1 8.8.8.8]", status.DNS)
	}
}

func assertNetworkInterfaces(t *testing.T, interfaces []networkInterface) {
	t.Helper()
	if len(interfaces) != 2 {
		t.Fatalf("interfaces = %d, want 2", len(interfaces))
	}
	assertEth0(t, interfaces[0])

	lo := interfaces[1]
	if lo.Name != "lo" || len(lo.Addresses) != 0 || lo.MAC != "" {
		t.Fatalf("lo should have no addresses or mac: %+v", lo)
	}
}

func assertEth0(t *testing.T, eth0 networkInterface) {
	t.Helper()
	if eth0.Name != "eth0" || eth0.Index != 2 || eth0.Type != "device" || eth0.State != "up" {
		t.Fatalf("eth0 metadata mismatch: %+v", eth0)
	}
	if eth0.MAC != "52:54:00:12:34:56" || eth0.MTU != 1500 || eth0.TxQueueLen != 1000 {
		t.Fatalf("eth0 mac/mtu/txqueuelen mismatch: %+v", eth0)
	}
	if eth0.Addressing != "static" {
		t.Fatalf("eth0 addressing = %q, want static (permanent global address)", eth0.Addressing)
	}
	if len(eth0.Addresses) != 2 {
		t.Fatalf("eth0 addresses = %d, want 2", len(eth0.Addresses))
	}
	if eth0.Addresses[0] != (networkAddress{IP: "192.168.1.10", PrefixLength: 24, Family: "ipv4"}) {
		t.Fatalf("eth0 ipv4 address mismatch: %+v", eth0.Addresses[0])
	}
	if eth0.Addresses[1].Family != "ipv6" || eth0.Addresses[1].PrefixLength != 64 {
		t.Fatalf("eth0 ipv6 address mismatch: %+v", eth0.Addresses[1])
	}
}

func assertNetworkRoutes(t *testing.T, routes []networkRoute) {
	t.Helper()
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	if routes[0] != (networkRoute{Destination: "0.0.0.0/0", Gateway: "192.168.1.1", Interface: "eth0", Family: "ipv4"}) {
		t.Fatalf("default route mismatch: %+v", routes[0])
	}
	if routes[1].Destination != "192.168.1.0/24" || routes[1].Interface != "eth0" {
		t.Fatalf("subnet route mismatch: %+v", routes[1])
	}
}

func TestCollectNetworkStatusLinkListingFailure(t *testing.T) {
	prev := netlinkLinkList
	netlinkLinkList = func() ([]netlink.Link, error) { return nil, fmt.Errorf("boom") }
	defer func() { netlinkLinkList = prev }()

	status := collectNetworkStatus(context.Background())
	if status.Error == "" || !strings.Contains(status.Error, "boom") {
		t.Fatalf("expected link listing error, got %+v", status)
	}
}

func TestCollectNetworkStatusRouteListingFailure(t *testing.T) {
	links, addrs := testNetworkLinks()
	defer stubNetlink(links, addrs, nil, fmt.Errorf("route boom"))()

	status := collectNetworkStatus(context.Background())
	if len(status.Interfaces) != 2 {
		t.Fatalf("interfaces = %d, want 2 despite route error", len(status.Interfaces))
	}
	if !strings.Contains(status.Error, "route boom") {
		t.Fatalf("expected route error recorded, got %q", status.Error)
	}
}

func TestResolvConfNameservers(t *testing.T) {
	if servers := resolvConfNameservers(filepath.Join(t.TempDir(), "missing")); servers != nil {
		t.Fatalf("missing file should yield no servers, got %v", servers)
	}

	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 9.9.9.9\n; comment\noptions edns0\nnameserver 2606:4700:4700::1111\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}
	if got := resolvConfNameservers(path); !reflect.DeepEqual(got, []string{"9.9.9.9", "2606:4700:4700::1111"}) {
		t.Fatalf("nameservers = %v", got)
	}
}
