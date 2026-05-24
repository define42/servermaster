package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestValidateContainerPort(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"min valid", 1, false},
		{"typical", 8080, false},
		{"max valid", 65535, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"just above max", 65536, true},
		{"would truncate to valid", 70000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContainerPort(tt.port)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateContainerPort(%d) error = %v, wantErr %v", tt.port, err, tt.wantErr)
			}
		})
	}
}

func TestParsePortNumber(t *testing.T) {
	tests := []struct {
		name    string
		port    string
		want    int
		wantErr bool
	}{
		{"min", "1", 1, false},
		{"max", "65535", 65535, false},
		{"typical", "8080", 8080, false},
		{"trims whitespace", " 80 ", 80, false},
		{"zero", "0", 0, true},
		{"too large", "65536", 0, true},
		{"non-numeric", "abc", 0, true},
		{"empty", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortNumber(tt.port)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePortNumber(%q) error = %v, wantErr %v", tt.port, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parsePortNumber(%q) = %d, want %d", tt.port, got, tt.want)
			}
		})
	}
}

func TestValidateFirewallPort(t *testing.T) {
	tests := []struct {
		name    string
		port    string
		wantErr bool
	}{
		{"single", "8080", false},
		{"range", "8000-8010", false},
		{"range single boundary", "80-80", false},
		{"trims whitespace", " 8080 ", false},
		{"empty", "", true},
		{"reversed range", "8010-8000", true},
		{"bad range start", "abc-10", true},
		{"bad range end", "10-abc", true},
		{"out of range", "70000", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFirewallPort(tt.port)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFirewallPort(%q) error = %v, wantErr %v", tt.port, err, tt.wantErr)
			}
		})
	}
}

func TestValidateFirewallProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		wantErr  bool
	}{
		{"empty defaults ok", "", false},
		{"tcp", "tcp", false},
		{"udp", "udp", false},
		{"sctp", "sctp", false},
		{"dccp", "dccp", false},
		{"uppercase", "TCP", false},
		{"trims whitespace", " udp ", false},
		{"invalid", "icmp", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFirewallProtocol(tt.protocol)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFirewallProtocol(%q) error = %v, wantErr %v", tt.protocol, err, tt.wantErr)
			}
		})
	}
}

func TestFirewallPortKey(t *testing.T) {
	tests := []struct {
		name     string
		port     string
		protocol string
		want     string
	}{
		{"tcp", "8080", "tcp", "8080/tcp"},
		{"empty protocol defaults tcp", "8080", "", "8080/tcp"},
		{"uppercase protocol", "53", "UDP", "53/udp"},
		{"trims whitespace", " 8000-8010 ", " tcp ", "8000-8010/tcp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firewallPortKey(tt.port, tt.protocol); got != tt.want {
				t.Fatalf("firewallPortKey(%q, %q) = %q, want %q", tt.port, tt.protocol, got, tt.want)
			}
		})
	}
}

func TestDeclaredFirewallPorts(t *testing.T) {
	ports := []FirewallPortConfig{
		{Port: "8080", Protocol: "tcp"}, // empty zone -> default
		{Zone: "public", Port: "443"},   // explicit zone, default proto
		{Zone: "internal", Port: "53", Protocol: "udp"},
		{Port: "8080", Protocol: "tcp"}, // duplicate of the first
	}

	declared := declaredFirewallPorts(ports, "public")

	// The empty-zone 8080/tcp and the explicit public 443/tcp both land in public.
	public := declared["public"]
	if len(public) != 2 {
		t.Fatalf("public zone keys = %v, want 2 entries", public)
	}
	if _, ok := public["8080/tcp"]; !ok {
		t.Fatalf("public zone missing 8080/tcp: %v", public)
	}
	if _, ok := public["443/tcp"]; !ok {
		t.Fatalf("public zone missing 443/tcp: %v", public)
	}

	internal := declared["internal"]
	if _, ok := internal["53/udp"]; !ok || len(internal) != 1 {
		t.Fatalf("internal zone = %v, want only 53/udp", internal)
	}

	if _, ok := declared["dmz"]; ok {
		t.Fatalf("undeclared zone dmz should be absent: %v", declared)
	}
}

func TestValidateSELinuxRelabel(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"shared", "z", false},
		{"private", "Z", false},
		{"trims whitespace", " Z ", false},
		{"case sensitive", "zZ", true},
		{"not a relabel option", "ro", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSELinuxRelabel(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSELinuxRelabel(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestParseFileMode(t *testing.T) {
	tests := []struct {
		name    string
		chmod   string
		want    os.FileMode
		wantErr bool
	}{
		{"leading zero", "0755", 0o755, false},
		{"no leading zero", "755", 0o755, false},
		{"setuid bits", "4755", 0o4755, false},
		{"trims whitespace", " 0644 ", 0o644, false},
		{"empty", "", 0, true},
		{"exceeds 07777", "10000", 0, true},
		{"non-octal digit", "8", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFileMode(tt.chmod)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFileMode(%q) error = %v, wantErr %v", tt.chmod, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parseFileMode(%q) = %o, want %o", tt.chmod, got, tt.want)
			}
		})
	}
}

func TestParseOwner(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		wantUID int
		wantGID int
		wantErr bool
	}{
		{"uid only", "1000", 1000, -1, false},
		{"uid and gid", "1000:2000", 1000, 2000, false},
		{"root", "0:0", 0, 0, false},
		{"trims whitespace", " 1000:2000 ", 1000, 2000, false},
		{"empty", "", -1, -1, true},
		{"missing user", ":2000", -1, -1, true},
		{"empty group", "1000:", -1, -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid, err := parseOwner(tt.owner)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOwner(%q) error = %v, wantErr %v", tt.owner, err, tt.wantErr)
			}
			if err == nil && (uid != tt.wantUID || gid != tt.wantGID) {
				t.Fatalf("parseOwner(%q) = (%d, %d), want (%d, %d)", tt.owner, uid, gid, tt.wantUID, tt.wantGID)
			}
		})
	}
}

func TestDecodeFileContent(t *testing.T) {
	tests := []struct {
		name    string
		file    FileConfig
		want    string
		wantErr bool
	}{
		{"empty encoding is plain", FileConfig{Content: "Hello, world!\n"}, "Hello, world!\n", false},
		{"explicit plain", FileConfig{Content: "abc", Encoding: "plain"}, "abc", false},
		{"trims encoding whitespace", FileConfig{Content: "abc", Encoding: " plain "}, "abc", false},
		{"base64", FileConfig{Content: "SGVsbG8=", Encoding: "base64"}, "Hello", false},
		{"empty plain content", FileConfig{}, "", false},
		{"bad base64", FileConfig{Content: "not!base64", Encoding: "base64"}, "", true},
		{"unknown encoding", FileConfig{Content: "abc", Encoding: "rot13"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeFileContent(tt.file)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeFileContent error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && string(got) != tt.want {
				t.Fatalf("decodeFileContent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureFiles(t *testing.T) {
	dir := t.TempDir()

	plainPath := filepath.Join(dir, "nested", "hello")
	files := []FileConfig{
		{Path: plainPath, Chmod: "0640", Content: "Hello, world!\n"},
		{Path: filepath.Join(dir, "raw"), Encoding: "base64", Content: "SGk="},
	}

	if err := ensureFiles(files); err != nil {
		t.Fatalf("ensureFiles: %v", err)
	}

	// Parent directories are created, content is written, and mode is exact.
	got, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "Hello, world!\n" {
		t.Fatalf("content = %q, want %q", got, "Hello, world!\n")
	}
	info, err := os.Stat(plainPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "raw"))
	if err != nil {
		t.Fatalf("read base64 file: %v", err)
	}
	if string(raw) != "Hi" {
		t.Fatalf("base64 content = %q, want %q", raw, "Hi")
	}

	// Rewriting is idempotent and overwrites prior content.
	files[0].Content = "changed\n"
	if err := ensureFiles(files[:1]); err != nil {
		t.Fatalf("ensureFiles rewrite: %v", err)
	}
	if got, _ := os.ReadFile(plainPath); string(got) != "changed\n" {
		t.Fatalf("rewrite content = %q, want %q", got, "changed\n")
	}

	t.Run("missing path", func(t *testing.T) {
		if err := ensureFiles([]FileConfig{{Content: "x"}}); err == nil {
			t.Fatal("expected error for missing path")
		}
	})
}

func TestParseInterfaceAddress(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		ipNet, err := parseInterfaceAddress("192.168.1.10", "192.168.1.0/24")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := ipNet.IP.String(); got != "192.168.1.10" {
			t.Fatalf("IP = %q, want 192.168.1.10", got)
		}
		if ones, _ := ipNet.Mask.Size(); ones != 24 {
			t.Fatalf("mask = /%d, want /24", ones)
		}
	})

	errorCases := []struct {
		name    string
		address string
		subnet  string
	}{
		{"address outside subnet", "192.168.2.10", "192.168.1.0/24"},
		{"invalid address", "not-an-ip", "192.168.1.0/24"},
		{"invalid subnet", "192.168.1.10", "garbage"},
	}

	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseInterfaceAddress(tt.address, tt.subnet); err == nil {
				t.Fatalf("parseInterfaceAddress(%q, %q) expected error, got nil", tt.address, tt.subnet)
			}
		})
	}
}

func TestBuildNMState(t *testing.T) {
	t.Run("static ipv4 with gateway and dns", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{
			Name:      "eth0",
			IPAddress: "192.168.1.10",
			Subnet:    "192.168.1.0/24",
			Gateway:   "192.168.1.1",
			DNS:       []string{"1.1.1.1", "8.8.8.8"},
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(state.Interfaces) != 1 {
			t.Fatalf("interfaces = %d, want 1", len(state.Interfaces))
		}
		iface := state.Interfaces[0]
		if iface.Name != "eth0" || iface.Type != "ethernet" || iface.State != "up" {
			t.Fatalf("interface metadata mismatch: %+v", iface)
		}
		if iface.IPv6 != nil {
			t.Fatalf("ipv4 address should not populate ipv6 stack: %+v", iface.IPv6)
		}
		if iface.IPv4 == nil || !iface.IPv4.Enabled || iface.IPv4.DHCP {
			t.Fatalf("ipv4 stack mismatch: %+v", iface.IPv4)
		}
		if got := iface.IPv4.Addresses[0]; got.IP != "192.168.1.10" || got.PrefixLength != 24 {
			t.Fatalf("address = %+v, want 192.168.1.10/24", got)
		}

		if state.Routes == nil || len(state.Routes.Config) != 1 {
			t.Fatalf("routes = %+v, want one default route", state.Routes)
		}
		route := state.Routes.Config[0]
		if route.Destination != "0.0.0.0/0" || route.NextHopAddress != "192.168.1.1" || route.NextHopInterface != "eth0" {
			t.Fatalf("route mismatch: %+v", route)
		}

		if state.DNSResolver == nil || !reflect.DeepEqual(state.DNSResolver.Config.Server, []string{"1.1.1.1", "8.8.8.8"}) {
			t.Fatalf("dns = %+v, want [1.1.1.1 8.8.8.8]", state.DNSResolver)
		}
	})

	t.Run("explicit type is passed through", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{
			Name:      "dummy0",
			Type:      "dummy",
			IPAddress: "192.168.1.10",
			Subnet:    "192.168.1.0/24",
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Interfaces[0].Type != "dummy" {
			t.Fatalf("type = %q, want dummy", state.Interfaces[0].Type)
		}
	})

	t.Run("empty type defaults to ethernet", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{Name: "eth0"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Interfaces[0].Type != "ethernet" {
			t.Fatalf("type = %q, want ethernet", state.Interfaces[0].Type)
		}
	})

	t.Run("vlan interface", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{
			Name:      "eth0.100",
			Type:      "vlan",
			IPAddress: "192.168.100.10",
			Subnet:    "192.168.100.0/24",
			VLAN:      &VLANConfig{BaseInterface: "eth0", ID: 100},
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		iface := state.Interfaces[0]
		if iface.Type != "vlan" || iface.VLAN == nil {
			t.Fatalf("vlan interface mismatch: %+v", iface)
		}
		if iface.VLAN.BaseIface != "eth0" || iface.VLAN.ID != 100 {
			t.Fatalf("vlan settings = %+v, want base eth0 id 100", iface.VLAN)
		}
		if iface.IPv4 == nil || iface.IPv4.Addresses[0].IP != "192.168.100.10" {
			t.Fatalf("vlan ipv4 mismatch: %+v", iface.IPv4)
		}
	})

	vlanErrors := []struct {
		name  string
		iface InterfaceConfig
	}{
		{"vlan type without settings", InterfaceConfig{Name: "eth0.100", Type: "vlan"}},
		{"vlan missing base", InterfaceConfig{Name: "eth0.100", Type: "vlan", VLAN: &VLANConfig{ID: 100}}},
		{"vlan id too low", InterfaceConfig{Name: "eth0.0", Type: "vlan", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 0}}},
		{"vlan id too high", InterfaceConfig{Name: "eth0.x", Type: "vlan", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 4095}}},
		{"vlan settings on non-vlan type", InterfaceConfig{Name: "eth0", Type: "ethernet", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 100}}},
	}
	for _, tt := range vlanErrors {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildNMState([]InterfaceConfig{tt.iface}); err == nil {
				t.Fatalf("buildNMState(%+v) expected error, got nil", tt.iface)
			}
		})
	}

	t.Run("ipv6 gateway yields default v6 route", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{
			Name:      "eth0",
			IPAddress: "2001:db8::10",
			Subnet:    "2001:db8::/64",
			Gateway:   "2001:db8::1",
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Interfaces[0].IPv4 != nil || state.Interfaces[0].IPv6 == nil {
			t.Fatalf("ipv6 address should populate ipv6 stack only: %+v", state.Interfaces[0])
		}
		if got := state.Routes.Config[0].Destination; got != "::/0" {
			t.Fatalf("route destination = %q, want ::/0", got)
		}
	})

	t.Run("dns merged and de-duplicated across interfaces", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{
			{Name: "eth0", DNS: []string{"1.1.1.1", "8.8.8.8"}},
			{Name: "eth1", DNS: []string{"8.8.8.8", "9.9.9.9"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(state.DNSResolver.Config.Server, []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}) {
			t.Fatalf("dns = %v, want deduped [1.1.1.1 8.8.8.8 9.9.9.9]", state.DNSResolver.Config.Server)
		}
	})

	errorCases := []struct {
		name  string
		iface InterfaceConfig
	}{
		{"missing name", InterfaceConfig{IPAddress: "10.0.0.1", Subnet: "10.0.0.0/24"}},
		{"ip without subnet", InterfaceConfig{Name: "eth0", IPAddress: "10.0.0.1"}},
		{"subnet without ip", InterfaceConfig{Name: "eth0", Subnet: "10.0.0.0/24"}},
		{"address outside subnet", InterfaceConfig{Name: "eth0", IPAddress: "10.1.0.1", Subnet: "10.0.0.0/24"}},
		{"bad gateway", InterfaceConfig{Name: "eth0", Gateway: "not-an-ip"}},
		{"bad dns", InterfaceConfig{Name: "eth0", DNS: []string{"not-an-ip"}}},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildNMState([]InterfaceConfig{tt.iface}); err == nil {
				t.Fatalf("buildNMState(%+v) expected error, got nil", tt.iface)
			}
		})
	}
}

// device builds a fake netlink link for tests. netlink.Device.Type() reports
// "device", matching what LinkList returns for a physical interface.
func device(index int, name, mac string, mtu int, state netlink.LinkOperState, flags net.Flags) *netlink.Device {
	attrs := netlink.LinkAttrs{
		Index:     index,
		Name:      name,
		MTU:       mtu,
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

func TestCollectNetworkStatus(t *testing.T) {
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("# comment\nnameserver 1.1.1.1\nnameserver 8.8.8.8\nsearch example.com\n"), 0o644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}
	prevResolv := resolvConfPath
	resolvConfPath = resolv
	defer func() { resolvConfPath = prevResolv }()

	links := []netlink.Link{
		device(2, "eth0", "52:54:00:12:34:56", 1500, netlink.OperUp, net.FlagUp|net.FlagBroadcast),
		device(1, "lo", "", 65536, netlink.OperUnknown, net.FlagUp|net.FlagLoopback),
	}
	addrs := map[int][]netlink.Addr{
		2: {
			{IPNet: cidr("192.168.1.10/24")},
			{IPNet: cidr("fe80::1/64")},
		},
	}
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
	if len(status.Interfaces) != 2 {
		t.Fatalf("interfaces = %d, want 2", len(status.Interfaces))
	}

	eth0 := status.Interfaces[0]
	if eth0.Name != "eth0" || eth0.Index != 2 || eth0.Type != "device" || eth0.State != "up" {
		t.Fatalf("eth0 metadata mismatch: %+v", eth0)
	}
	if eth0.MAC != "52:54:00:12:34:56" || eth0.MTU != 1500 {
		t.Fatalf("eth0 mac/mtu mismatch: %+v", eth0)
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

	lo := status.Interfaces[1]
	if lo.Name != "lo" || len(lo.Addresses) != 0 || lo.MAC != "" {
		t.Fatalf("lo should have no addresses or mac: %+v", lo)
	}

	if len(status.Routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(status.Routes))
	}
	if status.Routes[0] != (networkRoute{Destination: "0.0.0.0/0", Gateway: "192.168.1.1", Interface: "eth0", Family: "ipv4"}) {
		t.Fatalf("default route mismatch: %+v", status.Routes[0])
	}
	if status.Routes[1].Destination != "192.168.1.0/24" || status.Routes[1].Interface != "eth0" {
		t.Fatalf("subnet route mismatch: %+v", status.Routes[1])
	}

	if !reflect.DeepEqual(status.DNS, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("dns = %v, want [1.1.1.1 8.8.8.8]", status.DNS)
	}

	t.Run("link listing failure is reported", func(t *testing.T) {
		prev := netlinkLinkList
		netlinkLinkList = func() ([]netlink.Link, error) { return nil, fmt.Errorf("boom") }
		defer func() { netlinkLinkList = prev }()

		status := collectNetworkStatus(context.Background())
		if status.Error == "" || !strings.Contains(status.Error, "boom") {
			t.Fatalf("expected link listing error, got %+v", status)
		}
	})

	t.Run("route listing failure is recorded but interfaces still returned", func(t *testing.T) {
		defer stubNetlink(links, addrs, nil, fmt.Errorf("route boom"))()

		status := collectNetworkStatus(context.Background())
		if len(status.Interfaces) != 2 {
			t.Fatalf("interfaces = %d, want 2 despite route error", len(status.Interfaces))
		}
		if !strings.Contains(status.Error, "route boom") {
			t.Fatalf("expected route error recorded, got %q", status.Error)
		}
	})
}

func TestResolvConfNameservers(t *testing.T) {
	if servers := resolvConfNameservers(filepath.Join(t.TempDir(), "missing")); servers != nil {
		t.Fatalf("missing file should yield no servers, got %v", servers)
	}

	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 9.9.9.9\n; comment\noptions edns0\nnameserver 2606:4700:4700::1111\n"), 0o644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}
	if got := resolvConfNameservers(path); !reflect.DeepEqual(got, []string{"9.9.9.9", "2606:4700:4700::1111"}) {
		t.Fatalf("nameservers = %v", got)
	}
}

func TestContainerNeedsStop(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"Running", true},
		{"paused", true},
		{"restarting", true},
		{"unrecognized", true},
		{"created", false},
		{"configured", false},
		{"dead", false},
		{"exited", false},
		{"removing", false},
		{"stopped", false},
		{"EXITED", false},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := containerNeedsStop(tt.state); got != tt.want {
				t.Fatalf("containerNeedsStop(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestContainerIsConfigured(t *testing.T) {
	configured := map[string]struct{}{"web": {}, "db": {}}

	tests := []struct {
		name      string
		container listedContainer
		want      bool
	}{
		{"matches", listedContainer{Names: []string{"web"}}, true},
		{"matches one of many", listedContainer{Names: []string{"other", "db"}}, true},
		{"no match", listedContainer{Names: []string{"unmanaged"}}, false},
		{"no names", listedContainer{Names: nil}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerIsConfigured(tt.container, configured); got != tt.want {
				t.Fatalf("containerIsConfigured(%v) = %v, want %v", tt.container.Names, got, tt.want)
			}
		})
	}
}

func TestContainerDisplayName(t *testing.T) {
	tests := []struct {
		name      string
		container listedContainer
		want      string
	}{
		{"prefers name", listedContainer{Names: []string{"web"}, ID: "abc123"}, "web"},
		{"falls back to id", listedContainer{ID: "abc123"}, "abc123"},
		{"unknown", listedContainer{}, "<unknown>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerDisplayName(tt.container); got != tt.want {
				t.Fatalf("containerDisplayName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateSpec(t *testing.T) {
	c := ContainerConfig{
		Name:  "web",
		Image: "docker.io/library/nginx:latest",
		User:  "0:0",
		Env:   map[string]string{"TZ": "Europe/Copenhagen"},
		Ports: []PortConfig{
			{HostIP: "0.0.0.0", HostPort: 8081, ContainerPort: 80},
			{HostIP: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
		},
		Volumes: []VolumeConfig{
			{HostPath: "/data/web", ContainerPath: "/usr/share/nginx/html", ReadOnly: true, SELinux: "Z"},
			{HostPath: "/data/cache", ContainerPath: "/cache", ReadOnly: false},
		},
		Restart: "always",
	}

	spec, err := createSpec(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.Name != "web" || spec.Image != c.Image || spec.User != "0:0" {
		t.Fatalf("spec metadata mismatch: %+v", spec)
	}

	if spec.PortMappings[0].Protocol != "tcp" {
		t.Fatalf("port without protocol should default to tcp, got %q", spec.PortMappings[0].Protocol)
	}
	if spec.PortMappings[0].HostPort != 8081 || spec.PortMappings[0].ContainerPort != 80 {
		t.Fatalf("first port mapping mismatch: %+v", spec.PortMappings[0])
	}
	if spec.PortMappings[1].Protocol != "udp" {
		t.Fatalf("explicit protocol should be preserved, got %q", spec.PortMappings[1].Protocol)
	}

	if !reflect.DeepEqual(spec.Mounts[0].Options, []string{"rbind", "ro", "Z"}) {
		t.Fatalf("read-only mount options = %v, want [rbind ro Z]", spec.Mounts[0].Options)
	}
	if !reflect.DeepEqual(spec.Mounts[1].Options, []string{"rbind", "rw"}) {
		t.Fatalf("read-write mount options = %v, want [rbind rw]", spec.Mounts[1].Options)
	}
	if spec.Mounts[0].Type != "bind" || spec.Mounts[0].Source != "/data/web" || spec.Mounts[0].Destination != "/usr/share/nginx/html" {
		t.Fatalf("mount mapping mismatch: %+v", spec.Mounts[0])
	}

	if spec.RestartPolicy != "always" {
		t.Fatalf("restart policy = %q, want always", spec.RestartPolicy)
	}
}

func TestConfigHash(t *testing.T) {
	base := ContainerConfig{
		Name:  "web",
		Image: "docker.io/library/nginx:1.25",
		Env:   map[string]string{"A": "1", "B": "2"},
		Ports: []PortConfig{{HostPort: 8081, ContainerPort: 80}},
	}

	baseHash := configHash(base)

	// Map ordering must not affect the hash (Go marshals map keys sorted), and an
	// equal config must produce an equal hash.
	reordered := base
	reordered.Env = map[string]string{"B": "2", "A": "1"}
	if configHash(reordered) != baseHash {
		t.Fatal("hash changed for an equal config (map literal order should not matter)")
	}

	changes := map[string]ContainerConfig{
		"image":   {Name: "web", Image: "docker.io/library/nginx:1.26"},
		"env":     {Name: "web", Image: "docker.io/library/nginx:1.25", Env: map[string]string{"A": "1", "B": "3"}},
		"command": {Name: "web", Image: "docker.io/library/nginx:1.25", Command: []string{"sleep", "1"}},
		"restart": {Name: "web", Image: "docker.io/library/nginx:1.25", Restart: "always"},
	}
	for name, changed := range changes {
		if configHash(changed) == baseHash {
			t.Fatalf("hash did not change when %s changed", name)
		}
	}
}

func TestContainerUpToDate(t *testing.T) {
	const hash = "abc123"

	running := containerInspectResponse{
		State:  &containerInspectState{Running: true, Status: "running"},
		Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}},
	}
	if !containerUpToDate(running, hash) {
		t.Fatal("running container with matching hash should be up to date")
	}

	tests := []struct {
		name    string
		inspect containerInspectResponse
		hash    string
	}{
		{
			name:    "hash differs",
			inspect: running,
			hash:    "different",
		},
		{
			name: "not running",
			inspect: containerInspectResponse{
				State:  &containerInspectState{Running: false, Status: "exited"},
				Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}},
			},
			hash: hash,
		},
		{
			name: "missing label",
			inspect: containerInspectResponse{
				State:  &containerInspectState{Running: true},
				Config: &containerInspectConfig{Labels: map[string]string{}},
			},
			hash: hash,
		},
		{
			name:    "no state",
			inspect: containerInspectResponse{Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}}},
			hash:    hash,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if containerUpToDate(tt.inspect, tt.hash) {
				t.Fatalf("%s should not be up to date", tt.name)
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{
			PodmanMode:    "rootful",
			FirewallPorts: []FirewallPortConfig{{Zone: "public", Port: "8080", Protocol: "tcp"}},
			Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 8081, ContainerPort: 80}}},
			},
		}
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid default podman mode", func(t *testing.T) {
		if err := validateConfig(&Config{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	errorCases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{
			name: "rootless podman mode rejected",
			cfg:  &Config{PodmanMode: "rootless"},
			want: "podman_mode",
		},
		{
			name: "container declares interfaces",
			cfg: &Config{Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Interfaces: []InterfaceConfig{{Name: "eth0"}}},
			}},
			want: "interfaces",
		},
		{
			name: "bad host port",
			cfg: &Config{Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 70000, ContainerPort: 80}}},
			}},
			want: "host_port",
		},
		{
			name: "bad container port",
			cfg: &Config{Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 80, ContainerPort: 0}}},
			}},
			want: "container_port",
		},
		{
			name: "bad firewall port",
			cfg:  &Config{FirewallPorts: []FirewallPortConfig{{Port: "70000"}}},
			want: "firewall port",
		},
		{
			name: "bad firewall protocol",
			cfg:  &Config{FirewallPorts: []FirewallPortConfig{{Port: "8080", Protocol: "icmp"}}},
			want: "protocol",
		},
		{
			name: "bad selinux relabel",
			cfg: &Config{Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Volumes: []VolumeConfig{{HostPath: "/data", ContainerPath: "/data", SELinux: "x"}}},
			}},
			want: "selinux",
		},
		{
			name: "folder missing path",
			cfg:  &Config{Folders: []FolderConfig{{Chmod: "0755"}}},
			want: "missing path",
		},
		{
			name: "folder bad chmod",
			cfg:  &Config{Folders: []FolderConfig{{Path: "/data", Chmod: "99999"}}},
			want: "chmod",
		},
		{
			name: "file missing path",
			cfg:  &Config{Files: []FileConfig{{Content: "hi"}}},
			want: "missing path",
		},
		{
			name: "file bad chmod",
			cfg:  &Config{Files: []FileConfig{{Path: "/data/x", Chmod: "99999"}}},
			want: "chmod",
		},
		{
			name: "file bad encoding",
			cfg:  &Config{Files: []FileConfig{{Path: "/data/x", Encoding: "rot13"}}},
			want: "encoding",
		},
		{
			name: "file bad base64",
			cfg:  &Config{Files: []FileConfig{{Path: "/data/x", Encoding: "base64", Content: "not!base64"}}},
			want: "base64",
		},
		{
			name: "interface bad gateway",
			cfg:  &Config{Interfaces: []InterfaceConfig{{Name: "eth0", Gateway: "not-an-ip"}}},
			want: "gateway",
		},
	}

	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// stubConfigApplier replaces the host-mutating apply with fn for the duration
// of a test and returns a function that restores the original.
func stubConfigApplier(fn func(*Config) error) func() {
	prev := configApplier
	configApplier = fn
	return func() { configApplier = prev }
}

func stubServermasterStatusCollector(fn func(context.Context, string) servermasterStatus) func() {
	prev := servermasterStatusCollector
	servermasterStatusCollector = fn
	return func() { servermasterStatusCollector = prev }
}

func TestHandleServermasterStatus(t *testing.T) {
	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/servermaster", nil)
		rec := httptest.NewRecorder()
		handleServermasterStatus(rec, req, "unused")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("pretty json status", func(t *testing.T) {
		defer stubServermasterStatusCollector(func(context.Context, string) servermasterStatus {
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
		})()

		req := httptest.NewRequest(http.MethodGet, "/servermaster", nil)
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
	})
}

func TestImageReferenceVersion(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"docker.io/library/nginx:1.25", "1.25"},
		{"localhost:5000/app/backend:v2", "v2"},
		{"localhost:5000/app/backend", ""},
		{"quay.io/example/app@sha256:abc", "sha256:abc"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			if got := imageReferenceVersion(tt.ref); got != tt.want {
				t.Fatalf("imageReferenceVersion(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestParseRPMOstreeStatus(t *testing.T) {
	raw := []byte(`{
	  "deployments": [
	    {"booted": false, "version": "old", "checksum": "oldsum"},
	    {"booted": true, "version": "edge.1", "checksum": "newsum", "origin": "edge", "container-image-reference": "quay.io/example/os:edge.1"}
	  ]
	}`)

	status, err := parseRPMOstreeStatus(raw)
	if err != nil {
		t.Fatalf("parseRPMOstreeStatus: %v", err)
	}
	if !status.Booted || status.Version != "edge.1" || status.Checksum != "newsum" || status.Image != "quay.io/example/os:edge.1" {
		t.Fatalf("unexpected ostree status: %+v", status)
	}
}

func TestWriteConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "containers.json") // nested dir must be created
	body := []byte(`{"podman_mode":"rootful"}`)

	if err := writeConfigFile(path, body); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("contents = %q, want %q", got, body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("mode = %o, want 0644", perm)
	}
}

func TestHandleConfigUpload(t *testing.T) {
	validBody := `{"containers":[{"name":"web","image":"nginx","ports":[{"host_port":8081,"container_port":80}]}]}`

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		rec := httptest.NewRecorder()
		handleConfigUpload(rec, req, "unused")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("malformed json is rejected without writing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "containers.json")
		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader("{not json"))
		rec := httptest.NewRecorder()
		handleConfigUpload(rec, req, path)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("config file must not be created on parse failure")
		}
	})

	t.Run("invalid config is rejected without writing or applying", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "containers.json")
		applied := false
		defer stubConfigApplier(func(*Config) error { applied = true; return nil })()

		body := `{"firewall_ports":[{"port":"70000"}]}`
		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(body))
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
	})

	t.Run("valid config is saved and applied", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "containers.json")
		var appliedCfg *Config
		defer stubConfigApplier(func(c *Config) error { appliedCfg = c; return nil })()

		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(validBody))
		rec := httptest.NewRecorder()
		handleConfigUpload(rec, req, path)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if appliedCfg == nil || len(appliedCfg.Containers) != 1 || appliedCfg.Containers[0].Name != "web" {
			t.Fatalf("apply received unexpected config: %+v", appliedCfg)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved config: %v", err)
		}
		if string(got) != validBody {
			t.Fatalf("saved config = %q, want the uploaded body verbatim", got)
		}
	})

	t.Run("apply failure reports 500 but keeps the saved config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "containers.json")
		defer stubConfigApplier(func(*Config) error { return fmt.Errorf("firewalld down") })()

		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(validBody))
		rec := httptest.NewRecorder()
		handleConfigUpload(rec, req, path)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("config must remain saved after an apply failure: %v", err)
		}
	})
}

func TestOstreeUploadPath(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, defaultOstreeUploadPath},
		{"no ostree section", &Config{}, defaultOstreeUploadPath},
		{"blank path", &Config{Ostree: &OstreeConfig{UploadPath: "  "}}, defaultOstreeUploadPath},
		{"explicit path", &Config{Ostree: &OstreeConfig{UploadPath: "/srv/img.tar"}}, "/srv/img.tar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ostreeUploadPath(tt.cfg); got != tt.want {
				t.Fatalf("ostreeUploadPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleOstreeUpload(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "images", "update.tar") // nested dir must be created
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := fmt.Sprintf(`{"ostree":{"upload_path":%q}}`, dest)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	body := "fake-tar-contents"
	req := httptest.NewRequest(http.MethodPost, "/ostree/upload", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, cfgPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != body {
		t.Fatalf("dest contents = %q, want %q", got, body)
	}
}

func TestHandleOstreeUploadMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ostree/upload", nil)
	rec := httptest.NewRecorder()
	handleOstreeUpload(rec, req, "unused")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleOstreeUpgrade(t *testing.T) {
	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ostree/upgrade", nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, "unused")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("no apply command", func(t *testing.T) {
		cfgPath := writeTempConfig(t, `{}`)
		req := httptest.NewRequest(http.MethodPost, "/ostree/upgrade", nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, cfgPath)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	// reboot=false keeps the reboot path from firing during the test.
	t.Run("apply succeeds without reboot", func(t *testing.T) {
		cfgPath := writeTempConfig(t, `{"ostree":{"apply_command":["true"]}}`)
		req := httptest.NewRequest(http.MethodPost, "/ostree/upgrade?reboot=false", nil)
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
		req := httptest.NewRequest(http.MethodPost, "/ostree/upgrade?reboot=false", nil)
		rec := httptest.NewRecorder()
		handleOstreeUpgrade(rec, req, cfgPath)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
	})
}
