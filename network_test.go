package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestApplyTxQueueLengths(t *testing.T) {
	prevByName, prevSet := netlinkLinkByName, netlinkLinkSetTxQLen
	defer func() { netlinkLinkByName, netlinkLinkSetTxQLen = prevByName, prevSet }()

	var gotName string
	var gotQLen int
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		gotName = name
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name}}, nil
	}
	netlinkLinkSetTxQLen = func(_ netlink.Link, qlen int) error {
		gotQLen = qlen
		return nil
	}

	if err := applyTxQueueLengths([]InterfaceConfig{
		{Name: "eth0", TxQueueLen: intPtr(5000)},
		{Name: "eth1"}, // no txqueuelen -> skipped
	}); err != nil {
		t.Fatalf("applyTxQueueLengths: %v", err)
	}
	if gotName != "eth0" || gotQLen != 5000 {
		t.Fatalf("set txqueuelen on %q to %d, want eth0/5000", gotName, gotQLen)
	}
}

func TestApplyTxQueueLengthsErrors(t *testing.T) {
	prevByName, prevSet := netlinkLinkByName, netlinkLinkSetTxQLen
	defer func() { netlinkLinkByName, netlinkLinkSetTxQLen = prevByName, prevSet }()

	ifaces := []InterfaceConfig{{Name: "eth0", TxQueueLen: intPtr(1000)}}

	netlinkLinkByName = func(string) (netlink.Link, error) { return nil, errors.New("no such link") }
	netlinkLinkSetTxQLen = func(netlink.Link, int) error { return nil }
	if err := applyTxQueueLengths(ifaces); err == nil {
		t.Fatal("expected error when the link cannot be found")
	}

	netlinkLinkByName = func(string) (netlink.Link, error) { return &netlink.Device{}, nil }
	netlinkLinkSetTxQLen = func(netlink.Link, int) error { return errors.New("set failed") }
	if err := applyTxQueueLengths(ifaces); err == nil {
		t.Fatal("expected error when setting txqueuelen fails")
	}
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

func TestConfigureHostInterfaces(t *testing.T) {
	prev := nmstateStatePath
	nmstateStatePath = filepath.Join(t.TempDir(), "nmstate", "state.yml")
	defer func() { nmstateStatePath = prev }()
	fakeCommand(t, "nmstatectl", "exit 0")

	// No declared interfaces and no existing state file: nothing to remove.
	if err := configureHostInterfaces(nil, nil); err != nil {
		t.Fatalf("empty interfaces should be a no-op: %v", err)
	}

	ifaces := []InterfaceConfig{{Name: "eth0", IPAddress: "10.0.0.2", Subnet: "10.0.0.0/24"}}
	if err := configureHostInterfaces(ifaces, nil); err != nil {
		t.Fatalf("configureHostInterfaces: %v", err)
	}
	if _, err := os.Stat(nmstateStatePath); err != nil {
		t.Fatalf("desired-state file not written: %v", err)
	}

	// Dropping all interfaces must remove the state file so nmstate.service
	// does not reapply the stale config at the next boot.
	if err := configureHostInterfaces(nil, nil); err != nil {
		t.Fatalf("removing declared interfaces should succeed: %v", err)
	}
	if _, err := os.Stat(nmstateStatePath); !os.IsNotExist(err) {
		t.Fatalf("stale desired-state file not removed: stat err = %v", err)
	}

	if err := configureHostInterfaces([]InterfaceConfig{{Name: ""}}, nil); err == nil {
		t.Fatal("expected error for an interface with no name")
	}
}

// nmStateErrorCase is an interface config buildNMState is expected to reject.
type nmStateErrorCase struct {
	name  string
	iface InterfaceConfig
}

// assertBuildNMStateErrors runs each case as a subtest asserting buildNMState
// returns an error for the given interface.
func assertBuildNMStateErrors(t *testing.T, cases []nmStateErrorCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildNMState([]InterfaceConfig{tt.iface}, nil); err == nil {
				t.Fatalf("buildNMState(%+v) expected error, got nil", tt.iface)
			}
		})
	}
}

func TestBuildNMStateStaticIPv4(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:      "eth0",
		IPAddress: "192.168.1.10",
		Subnet:    "192.168.1.0/24",
		Gateway:   "192.168.1.1",
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(state.Interfaces) != 1 {
		t.Fatalf("interfaces = %d, want 1", len(state.Interfaces))
	}
	assertStaticIPv4Interface(t, state.Interfaces[0])

	if state.Routes == nil || len(state.Routes.Config) != 1 {
		t.Fatalf("routes = %+v, want one default route", state.Routes)
	}
	assertStaticIPv4Route(t, state.Routes.Config[0])

	if state.DNSResolver == nil || !reflect.DeepEqual(state.DNSResolver.Config.Server, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("dns = %+v, want [1.1.1.1 8.8.8.8]", state.DNSResolver)
	}
}

func TestConfigureHostInterfacesApplyFails(t *testing.T) {
	prev := nmstateStatePath
	nmstateStatePath = filepath.Join(t.TempDir(), "state.yml")
	defer func() { nmstateStatePath = prev }()
	fakeCommand(t, "nmstatectl", "echo nope >&2; exit 1")

	err := configureHostInterfaces([]InterfaceConfig{{Name: "eth0"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "apply host interface configuration failed") {
		t.Fatalf("err = %v, want apply failure", err)
	}
	// A failed apply must not leave a document at the canonical path, or
	// nmstate.service would reapply the never-validated config at boot.
	if _, statErr := os.Stat(nmstateStatePath); !os.IsNotExist(statErr) {
		t.Fatalf("apply failure left a state file behind: stat err = %v", statErr)
	}
	// And it must not leave temp files littering the directory either.
	entries, readErr := os.ReadDir(filepath.Dir(nmstateStatePath))
	if readErr != nil {
		t.Fatalf("read nmstate dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("apply failure left files behind: %v", entries)
	}
}

func assertStaticIPv4Interface(t *testing.T, iface nmInterface) {
	t.Helper()
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
}

func assertStaticIPv4Route(t *testing.T, route nmRoute) {
	t.Helper()
	if route.Destination != "0.0.0.0/0" || route.NextHopAddress != "192.168.1.1" || route.NextHopInterface != "eth0" {
		t.Fatalf("route mismatch: %+v", route)
	}
}

func TestBuildNMStateInterfaceType(t *testing.T) {
	t.Run("explicit type is passed through", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{
			Name:      "dummy0",
			Type:      "dummy",
			IPAddress: "192.168.1.10",
			Subnet:    "192.168.1.0/24",
		}}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Interfaces[0].Type != "dummy" {
			t.Fatalf("type = %q, want dummy", state.Interfaces[0].Type)
		}
	})

	t.Run("empty type defaults to ethernet", func(t *testing.T) {
		state, err := buildNMState([]InterfaceConfig{{Name: "eth0"}}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Interfaces[0].Type != "ethernet" {
			t.Fatalf("type = %q, want ethernet", state.Interfaces[0].Type)
		}
	})
}

func TestBuildNMStateVLAN(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:      "eth0.100",
		Type:      "vlan",
		IPAddress: "192.168.100.10",
		Subnet:    "192.168.100.0/24",
		VLAN:      &VLANConfig{BaseInterface: "eth0", ID: 100},
	}}, nil)
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
}

func TestBuildNMStateMTU(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:       "eth0",
		MTU:        intPtr(9000),
		IPv4Method: "dhcp",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mtu := state.Interfaces[0].MTU
	if mtu == nil || *mtu != 9000 {
		t.Fatalf("mtu = %v, want 9000", mtu)
	}
}

func TestBuildNMStateVLANErrors(t *testing.T) {
	assertBuildNMStateErrors(t, []nmStateErrorCase{
		{"vlan type without settings", InterfaceConfig{Name: "eth0.100", Type: "vlan"}},
		{"vlan missing base", InterfaceConfig{Name: "eth0.100", Type: "vlan", VLAN: &VLANConfig{ID: 100}}},
		{"vlan id too low", InterfaceConfig{Name: "eth0.0", Type: "vlan", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 0}}},
		{"vlan id too high", InterfaceConfig{Name: "eth0.x", Type: "vlan", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 4095}}},
		{"vlan settings on non-vlan type", InterfaceConfig{Name: "eth0", Type: "ethernet", VLAN: &VLANConfig{BaseInterface: "eth0", ID: 100}}},
	})
}

func TestBuildNMStateBond(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:      "bond0",
		Type:      "bond",
		IPAddress: "192.168.1.50",
		Subnet:    "192.168.1.0/24",
		Bond: &BondConfig{
			Mode:    "active-backup",
			Ports:   []string{"eth1", "eth2"},
			Miimon:  intPtr(100),
			Primary: "eth1",
		},
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	iface := state.Interfaces[0]
	if iface.Type != "bond" || iface.LinkAggregation == nil {
		t.Fatalf("bond interface mismatch: %+v", iface)
	}
	agg := iface.LinkAggregation
	if agg.Mode != "active-backup" {
		t.Fatalf("bond mode = %q, want active-backup", agg.Mode)
	}
	if !reflect.DeepEqual(agg.Port, []string{"eth1", "eth2"}) {
		t.Fatalf("bond ports = %v, want [eth1 eth2]", agg.Port)
	}
	if agg.Options == nil || agg.Options.Miimon == nil || *agg.Options.Miimon != 100 {
		t.Fatalf("bond miimon mismatch: %+v", agg.Options)
	}
	if agg.Options.Primary != "eth1" {
		t.Fatalf("bond primary = %q, want eth1", agg.Options.Primary)
	}
	if iface.IPv4 == nil || iface.IPv4.Addresses[0].IP != "192.168.1.50" {
		t.Fatalf("bond ipv4 mismatch: %+v", iface.IPv4)
	}
}

func TestBuildNMStateBondNoOptions(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name: "bond0",
		Type: "bond",
		Bond: &BondConfig{Mode: "802.3ad", Ports: []string{"eth1", "eth2"}},
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	agg := state.Interfaces[0].LinkAggregation
	if agg == nil || agg.Mode != "802.3ad" {
		t.Fatalf("bond mismatch: %+v", agg)
	}
	if agg.Options != nil {
		t.Fatalf("bond options = %+v, want nil when no options declared", agg.Options)
	}
}

func TestBuildNMStateBondErrors(t *testing.T) {
	assertBuildNMStateErrors(t, []nmStateErrorCase{
		{"bond type without settings", InterfaceConfig{Name: "bond0", Type: "bond"}},
		{"bond missing mode", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Ports: []string{"eth1"}}}},
		{"bond invalid mode", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "bogus", Ports: []string{"eth1"}}}},
		{"bond no ports", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup"}}},
		{"bond empty port", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"eth1", ""}}}},
		{"bond duplicate port", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"eth1", "eth1"}}}},
		{"bond self port", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"bond0"}}}},
		{"bond miimon negative", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"eth1"}, Miimon: intPtr(-1)}}},
		{"bond primary not a port", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"eth1"}, Primary: "eth9"}}},
		{"bond primary on unsupported mode", InterfaceConfig{Name: "bond0", Type: "bond", Bond: &BondConfig{Mode: "balance-rr", Ports: []string{"eth1", "eth2"}, Primary: "eth1"}}},
		{"bond settings on non-bond type", InterfaceConfig{Name: "eth0", Type: "ethernet", Bond: &BondConfig{Mode: "active-backup", Ports: []string{"eth1"}}}},
	})
}

func TestBuildNMStateIPv6Gateway(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:      "eth0",
		IPAddress: "2001:db8::10",
		Subnet:    "2001:db8::/64",
		Gateway:   "2001:db8::1",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Interfaces[0].IPv4 != nil || state.Interfaces[0].IPv6 == nil {
		t.Fatalf("ipv6 address should populate ipv6 stack only: %+v", state.Interfaces[0])
	}
	if got := state.Routes.Config[0].Destination; got != "::/0" {
		t.Fatalf("route destination = %q, want ::/0", got)
	}
}

func TestBuildNMStateIPv4DHCP(t *testing.T) {
	for _, method := range []string{"dhcp", "auto"} {
		t.Run(method, func(t *testing.T) {
			state, err := buildNMState([]InterfaceConfig{{Name: "eth0", IPv4Method: method}}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			iface := state.Interfaces[0]
			if iface.IPv6 != nil {
				t.Fatalf("ipv4_method should leave ipv6 untouched: %+v", iface.IPv6)
			}
			if iface.IPv4 == nil || !iface.IPv4.Enabled || !iface.IPv4.DHCP {
				t.Fatalf("ipv4 stack should be enabled with dhcp on: %+v", iface.IPv4)
			}
			if len(iface.IPv4.Addresses) != 0 {
				t.Fatalf("dhcp stack should carry no static addresses: %+v", iface.IPv4.Addresses)
			}
		})
	}
}

func TestBuildNMStateIPv4Disabled(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{Name: "eth0", IPv4Method: "disabled"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ipv4 := state.Interfaces[0].IPv4
	if ipv4 == nil || ipv4.Enabled || ipv4.DHCP {
		t.Fatalf("ipv4 disabled stack mismatch: %+v", ipv4)
	}
}

func TestBuildNMStateClearsDNSWhenNoneDeclared(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{{
		Name:            "dummy0",
		Type:            "dummy",
		TxQueueLen:      intPtr(20000),
		IPv4Method:      "disabled",
		IPv6Method:      "link-local",
		IPv6AddrGenMode: "eui64",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.DNSResolver == nil {
		t.Fatalf("dns-resolver should be emitted to clear stale DNS")
	}
	if len(state.DNSResolver.Config.Server) != 0 {
		t.Fatalf("dns servers = %v, want none", state.DNSResolver.Config.Server)
	}

	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	resolver, ok := encoded["dns-resolver"].(map[string]any)
	if !ok {
		t.Fatalf("encoded state missing dns-resolver: %s", raw)
	}
	config, ok := resolver["config"].(map[string]any)
	if !ok || len(config) != 0 {
		t.Fatalf("dns-resolver config = %#v, want empty object; json=%s", resolver["config"], raw)
	}
}

func TestBuildNMStateIPv4DHCPWithIPv6Static(t *testing.T) {
	// IPv4 DHCP coexists with a static IPv6 address (different families).
	state, err := buildNMState([]InterfaceConfig{{
		Name:       "eth0",
		IPv4Method: "dhcp",
		IPAddress:  "2001:db8::10",
		Subnet:     "2001:db8::/64",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	iface := state.Interfaces[0]
	if iface.IPv4 == nil || !iface.IPv4.DHCP {
		t.Fatalf("ipv4 dhcp stack mismatch: %+v", iface.IPv4)
	}
	if iface.IPv6 == nil || iface.IPv6.Addresses[0].IP != "2001:db8::10" {
		t.Fatalf("static ipv6 address should survive: %+v", iface.IPv6)
	}
}

func TestBuildNMStateIPv4Errors(t *testing.T) {
	assertBuildNMStateErrors(t, []nmStateErrorCase{
		{"unknown method", InterfaceConfig{Name: "eth0", IPv4Method: "bogus"}},
		{"method conflicts with static v4", InterfaceConfig{Name: "eth0", IPAddress: "192.168.1.10", Subnet: "192.168.1.0/24", IPv4Method: "dhcp"}},
	})
}

func TestBuildNMStateIPv6LinkLocal(t *testing.T) {
	// The nmcli equivalent: ipv6.method link-local, ipv6.addr-gen-mode eui64.
	state, err := buildNMState([]InterfaceConfig{{
		Name:            "ens1f1np1",
		IPv6Method:      "link-local",
		IPv6AddrGenMode: "eui64",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	iface := state.Interfaces[0]
	if iface.IPv4 != nil {
		t.Fatalf("ipv6_method should leave ipv4 untouched: %+v", iface.IPv4)
	}
	if iface.IPv6 == nil {
		t.Fatalf("ipv6_method link-local should build an ipv6 stack")
	}
	if !iface.IPv6.Enabled || iface.IPv6.DHCP {
		t.Fatalf("link-local stack should be enabled with dhcp off: %+v", iface.IPv6)
	}
	if iface.IPv6.Autoconf == nil || *iface.IPv6.Autoconf {
		t.Fatalf("link-local stack should set autoconf false: %+v", iface.IPv6)
	}
	if len(iface.IPv6.Addresses) != 0 {
		t.Fatalf("link-local stack should carry no addresses: %+v", iface.IPv6.Addresses)
	}
	if iface.IPv6.AddrGenMode != "eui64" {
		t.Fatalf("addr-gen-mode = %q, want eui64", iface.IPv6.AddrGenMode)
	}
}

func TestBuildNMStateIPv6Methods(t *testing.T) {
	cases := []struct {
		method   string
		enabled  bool
		dhcp     bool
		autoconf *bool
	}{
		{"auto", true, true, boolPtr(true)},
		{"dhcp", true, true, boolPtr(false)},
		{"disabled", false, false, nil},
	}
	for _, tt := range cases {
		t.Run(tt.method, func(t *testing.T) {
			state, err := buildNMState([]InterfaceConfig{{Name: "eth0", IPv6Method: tt.method}}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ipv6 := state.Interfaces[0].IPv6
			if ipv6 == nil {
				t.Fatalf("ipv6_method %q should build an ipv6 stack", tt.method)
			}
			if ipv6.Enabled != tt.enabled || ipv6.DHCP != tt.dhcp {
				t.Fatalf("stack = %+v, want enabled=%v dhcp=%v", ipv6, tt.enabled, tt.dhcp)
			}
			if !reflect.DeepEqual(ipv6.Autoconf, tt.autoconf) {
				t.Fatalf("autoconf = %v, want %v", ipv6.Autoconf, tt.autoconf)
			}
		})
	}
}

func TestBuildNMStateIPv6AddrGenModeOnStatic(t *testing.T) {
	// addr-gen-mode attaches to the stack a static IPv6 ip_address builds.
	state, err := buildNMState([]InterfaceConfig{{
		Name:            "eth0",
		IPAddress:       "2001:db8::10",
		Subnet:          "2001:db8::/64",
		IPv6AddrGenMode: "stable-privacy",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ipv6 := state.Interfaces[0].IPv6
	if ipv6 == nil || ipv6.AddrGenMode != "stable-privacy" {
		t.Fatalf("addr-gen-mode should attach to the static ipv6 stack: %+v", ipv6)
	}
	if len(ipv6.Addresses) != 1 || ipv6.Addresses[0].IP != "2001:db8::10" {
		t.Fatalf("static address should survive: %+v", ipv6.Addresses)
	}
}

func TestBuildNMStateIPv4StaticWithIPv6LinkLocal(t *testing.T) {
	// A static IPv4 address coexists with an IPv6 link-local method.
	state, err := buildNMState([]InterfaceConfig{{
		Name:       "eth0",
		IPAddress:  "192.168.1.10",
		Subnet:     "192.168.1.0/24",
		IPv6Method: "link-local",
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	iface := state.Interfaces[0]
	if iface.IPv4 == nil || iface.IPv4.Addresses[0].IP != "192.168.1.10" {
		t.Fatalf("ipv4 static address mismatch: %+v", iface.IPv4)
	}
	if iface.IPv6 == nil || !iface.IPv6.Enabled || iface.IPv6.DHCP {
		t.Fatalf("ipv6 link-local stack mismatch: %+v", iface.IPv6)
	}
}

func TestBuildNMStateIPv6Errors(t *testing.T) {
	assertBuildNMStateErrors(t, []nmStateErrorCase{
		{"unknown method", InterfaceConfig{Name: "eth0", IPv6Method: "bogus"}},
		{"unknown addr-gen-mode", InterfaceConfig{Name: "eth0", IPv6Method: "link-local", IPv6AddrGenMode: "random"}},
		{"method conflicts with static v6", InterfaceConfig{Name: "eth0", IPAddress: "2001:db8::10", Subnet: "2001:db8::/64", IPv6Method: "link-local"}},
		{"addr-gen-mode without ipv6", InterfaceConfig{Name: "eth0", IPv6AddrGenMode: "eui64"}},
		{"addr-gen-mode with disabled ipv6", InterfaceConfig{Name: "eth0", IPv6Method: "disabled", IPv6AddrGenMode: "eui64"}},
	})
}

func TestBuildNMStateDNSMerge(t *testing.T) {
	state, err := buildNMState([]InterfaceConfig{
		{Name: "eth0", DNS: []string{"1.1.1.1", "8.8.8.8"}},
		{Name: "eth1", DNS: []string{"8.8.8.8", "9.9.9.9"}},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(state.DNSResolver.Config.Server, []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}) {
		t.Fatalf("dns = %v, want deduped [1.1.1.1 8.8.8.8 9.9.9.9]", state.DNSResolver.Config.Server)
	}
}

func TestBuildNMStateErrors(t *testing.T) {
	assertBuildNMStateErrors(t, []nmStateErrorCase{
		{"missing name", InterfaceConfig{IPAddress: "10.0.0.1", Subnet: "10.0.0.0/24"}},
		{"ip without subnet", InterfaceConfig{Name: "eth0", IPAddress: "10.0.0.1"}},
		{"subnet without ip", InterfaceConfig{Name: "eth0", Subnet: "10.0.0.0/24"}},
		{"address outside subnet", InterfaceConfig{Name: "eth0", IPAddress: "10.1.0.1", Subnet: "10.0.0.0/24"}},
		{"bad gateway", InterfaceConfig{Name: "eth0", Gateway: "not-an-ip"}},
		{"bad dns", InterfaceConfig{Name: "eth0", DNS: []string{"not-an-ip"}}},
		{"zero mtu", InterfaceConfig{Name: "eth0", MTU: intPtr(0)}},
		{"negative mtu", InterfaceConfig{Name: "eth0", MTU: intPtr(-1)}},
		{"negative txqueuelen", InterfaceConfig{Name: "eth0", TxQueueLen: intPtr(-1)}},
	})
}

func TestBuildNMStateRoutes(t *testing.T) {
	state, err := buildNMState(nil, []RouteConfig{
		{
			Destination:      "0.0.0.0/0",
			NextHopInterface: "eth0",
			NextHopAddress:   "192.168.1.1",
			TableID:          intPtr(100),
			Metric:           intPtr(50),
		},
		{
			// On-link route: no gateway, and a host bit set in the destination
			// that must be normalized away to the network address.
			Destination:      "10.20.0.5/16",
			NextHopInterface: "eth1",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Routes == nil || len(state.Routes.Config) != 2 {
		t.Fatalf("routes = %+v, want two", state.Routes)
	}

	def := state.Routes.Config[0]
	if def.Destination != "0.0.0.0/0" || def.NextHopInterface != "eth0" || def.NextHopAddress != "192.168.1.1" {
		t.Fatalf("default route mismatch: %+v", def)
	}
	if def.TableID == nil || *def.TableID != 100 || def.Metric == nil || *def.Metric != 50 {
		t.Fatalf("default route table/metric mismatch: %+v", def)
	}

	onlink := state.Routes.Config[1]
	if onlink.Destination != "10.20.0.0/16" {
		t.Fatalf("on-link destination not normalized to network: %q", onlink.Destination)
	}
	if onlink.NextHopAddress != "" || onlink.TableID != nil || onlink.Metric != nil {
		t.Fatalf("on-link route should leave next hop/table/metric unset: %+v", onlink)
	}
}

func TestBuildNMStateIPv6Route(t *testing.T) {
	state, err := buildNMState(nil, []RouteConfig{{
		Destination:      "::/0",
		NextHopInterface: "eth0",
		NextHopAddress:   "2001:db8::1",
		TableID:          intPtr(254),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	route := state.Routes.Config[0]
	if route.Destination != "::/0" || route.NextHopAddress != "2001:db8::1" {
		t.Fatalf("ipv6 route mismatch: %+v", route)
	}
}

func TestBuildNMStateRoutesAppendAfterGateway(t *testing.T) {
	// Explicitly declared routes are additive to the per-interface gateway
	// route, and follow it in the emitted document.
	state, err := buildNMState(
		[]InterfaceConfig{{Name: "eth0", IPAddress: "192.168.1.10", Subnet: "192.168.1.0/24", Gateway: "192.168.1.1"}},
		[]RouteConfig{{Destination: "10.0.0.0/8", NextHopInterface: "eth0", NextHopAddress: "192.168.1.254"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Routes == nil || len(state.Routes.Config) != 2 {
		t.Fatalf("routes = %+v, want gateway route plus explicit route", state.Routes)
	}
	if state.Routes.Config[0].Destination != "0.0.0.0/0" {
		t.Fatalf("gateway route should come first: %+v", state.Routes.Config)
	}
	if state.Routes.Config[1].Destination != "10.0.0.0/8" {
		t.Fatalf("explicit route should follow gateway route: %+v", state.Routes.Config)
	}
}

func TestBuildNMStateRouteJSONOmitsUnsetFields(t *testing.T) {
	state, err := buildNMState(nil, []RouteConfig{{Destination: "10.0.0.0/8", NextHopInterface: "eth0"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"next-hop-address", "table-id", "metric"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("unset route field %q should be omitted; json=%s", key, raw)
		}
	}

	state, err = buildNMState(nil, []RouteConfig{{
		Destination:      "10.0.0.0/8",
		NextHopInterface: "eth0",
		NextHopAddress:   "10.0.0.1",
		TableID:          intPtr(200),
		Metric:           intPtr(10),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"next-hop-address":"10.0.0.1"`, `"table-id":200`, `"metric":10`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("set route field %q should be present; json=%s", want, raw)
		}
	}
}

func TestBuildNMStateRouteErrors(t *testing.T) {
	cases := []struct {
		name  string
		route RouteConfig
	}{
		{"missing destination", RouteConfig{NextHopInterface: "eth0"}},
		{"bad destination", RouteConfig{Destination: "not-a-cidr", NextHopInterface: "eth0"}},
		{"bare ip destination", RouteConfig{Destination: "10.0.0.1", NextHopInterface: "eth0"}},
		{"missing next hop interface", RouteConfig{Destination: "0.0.0.0/0", NextHopAddress: "192.168.1.1"}},
		{"bad next hop address", RouteConfig{Destination: "0.0.0.0/0", NextHopInterface: "eth0", NextHopAddress: "nope"}},
		{"family mismatch", RouteConfig{Destination: "0.0.0.0/0", NextHopInterface: "eth0", NextHopAddress: "2001:db8::1"}},
		{"negative table id", RouteConfig{Destination: "0.0.0.0/0", NextHopInterface: "eth0", TableID: intPtr(-1)}},
		{"table id too large", RouteConfig{Destination: "0.0.0.0/0", NextHopInterface: "eth0", TableID: intPtr(maxRouteU32 + 1)}},
		{"negative metric", RouteConfig{Destination: "0.0.0.0/0", NextHopInterface: "eth0", Metric: intPtr(-1)}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildNMState(nil, []RouteConfig{tt.route}); err == nil {
				t.Fatalf("buildNMState(route %+v) expected error, got nil", tt.route)
			}
		})
	}
}

func TestBuildNMStateRouteNameInError(t *testing.T) {
	_, err := buildNMState(nil, []RouteConfig{{
		Name:             "uplink",
		Destination:      "0.0.0.0/0",
		NextHopInterface: "eth0",
		NextHopAddress:   "not-an-ip",
	}})
	if err == nil {
		t.Fatal("expected error for bad next_hop_address")
	}
	if !strings.Contains(err.Error(), "uplink") {
		t.Fatalf("error %q should identify the route by its name %q", err, "uplink")
	}
}

func TestBuildNMStateRouteNameNotApplied(t *testing.T) {
	// name is documentation only: the route still builds, but the name does not
	// reach the nmstate document applied to the host.
	state, err := buildNMState(nil, []RouteConfig{{
		Name:             "corp-net",
		Destination:      "10.0.0.0/8",
		NextHopInterface: "eth0",
		NextHopAddress:   "192.168.1.1",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Routes == nil || len(state.Routes.Config) != 1 {
		t.Fatalf("route not built: %+v", state.Routes)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "corp-net") {
		t.Fatalf("route name leaked into nmstate document: %s", raw)
	}
}

func TestConfigureHostInterfacesRoutesOnly(t *testing.T) {
	prev := nmstateStatePath
	nmstateStatePath = filepath.Join(t.TempDir(), "nmstate", "state.yml")
	defer func() { nmstateStatePath = prev }()
	fakeCommand(t, "nmstatectl", "exit 0")

	// Routes but no interfaces must still write a state file so the routes are
	// applied (and reapplied at boot by nmstate.service).
	routes := []RouteConfig{{Destination: "10.0.0.0/8", NextHopInterface: "eth0", NextHopAddress: "192.168.1.1"}}
	if err := configureHostInterfaces(nil, routes); err != nil {
		t.Fatalf("configureHostInterfaces with routes only: %v", err)
	}
	if _, err := os.Stat(nmstateStatePath); err != nil {
		t.Fatalf("desired-state file not written for routes-only config: %v", err)
	}

	// Dropping both interfaces and routes removes the state file.
	if err := configureHostInterfaces(nil, nil); err != nil {
		t.Fatalf("clearing interfaces and routes should succeed: %v", err)
	}
	if _, err := os.Stat(nmstateStatePath); !os.IsNotExist(err) {
		t.Fatalf("stale desired-state file not removed: stat err = %v", err)
	}
}

func intPtr(n int) *int { return &n }

func boolPtr(b bool) *bool { return &b }
