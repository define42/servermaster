package main

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestSplitLogFrame(t *testing.T) {
	if got := splitLogFrame(""); got != nil {
		t.Fatalf("splitLogFrame(empty) = %v, want nil", got)
	}
	if got := splitLogFrame("a\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLogFrame = %v, want [a b]", got)
	}
}

func TestAppendStatusError(t *testing.T) {
	if got := appendStatusError("", "first"); got != "first" {
		t.Fatalf("appendStatusError(empty) = %q", got)
	}
	if got := appendStatusError("first", "second"); got != "first; second" {
		t.Fatalf("appendStatusError = %q, want joined", got)
	}
}

func TestContainerUpToDateNilConfig(t *testing.T) {
	inspect := containerInspectResponse{State: &containerInspectState{Running: true}, Config: nil}
	if containerUpToDate(inspect, "hash") {
		t.Fatal("a running container with no config is not up to date")
	}
}

func TestDemuxShortReader(t *testing.T) {
	buf := make([]byte, 1024)
	if _, _, err := demuxHeader(strings.NewReader("123"), buf); err == nil {
		t.Fatal("demuxHeader expected error on truncated header")
	}
	if _, err := demuxFrame(strings.NewReader("12"), buf, 10); err == nil {
		t.Fatal("demuxFrame expected error on truncated payload")
	}
	// An undersized buffer is grown internally before the 8-byte header read.
	header := multiplexedLog(1, "")[:8]
	fd, length, err := demuxHeader(strings.NewReader(string(header)), make([]byte, 4))
	if err != nil || fd != 1 || length != 0 {
		t.Fatalf("demuxHeader(small buf) = %d,%d,%v", fd, length, err)
	}
}

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

func TestLoadConfigBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig expected error on malformed JSON")
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("loadConfig expected error on missing file")
	}
}

func TestNestedStringStringer(t *testing.T) {
	root := map[string]any{"addr": net.IPv4(10, 0, 0, 1)} // net.IP is a fmt.Stringer
	if got := nestedString(root, "addr"); got != "10.0.0.1" {
		t.Fatalf("nestedString = %q, want 10.0.0.1", got)
	}
	if got := nestedString(root, "missing", "deep"); got != "" {
		t.Fatalf("nestedString(missing) = %q, want empty", got)
	}
}
