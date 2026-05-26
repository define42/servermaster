package main

import (
	"context"
	"errors"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

// fakeBusObject implements dbus.BusObject for the firewalld functions that take
// the interface directly. Only Call is exercised; it returns the configured Body
// (decoded by *dbus.Call.Store) and records the methods invoked. The functions
// under test never touch a real system bus.
type fakeBusObject struct {
	bodies map[string][]any
	errs   map[string]error
	calls  []string
}

func newFakeBus() *fakeBusObject {
	return &fakeBusObject{bodies: map[string][]any{}, errs: map[string]error{}}
}

func (f *fakeBusObject) respond(method string, values ...any) {
	f.bodies[method] = values
}

func (f *fakeBusObject) Call(method string, _ dbus.Flags, _ ...any) *dbus.Call {
	f.calls = append(f.calls, method)
	return &dbus.Call{Err: f.errs[method], Body: f.bodies[method]}
}

func (f *fakeBusObject) callCount(method string) int {
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// The remaining BusObject methods are unused by the code under test.
func (f *fakeBusObject) CallWithContext(_ context.Context, _ string, _ dbus.Flags, _ ...any) *dbus.Call {
	return &dbus.Call{}
}

func (f *fakeBusObject) Go(_ string, _ dbus.Flags, _ chan *dbus.Call, _ ...any) *dbus.Call {
	return &dbus.Call{}
}

func (f *fakeBusObject) GoWithContext(_ context.Context, _ string, _ dbus.Flags, _ chan *dbus.Call, _ ...any) *dbus.Call {
	return &dbus.Call{}
}

func (f *fakeBusObject) AddMatchSignal(_, _ string, _ ...dbus.MatchOption) *dbus.Call {
	return &dbus.Call{}
}

func (f *fakeBusObject) RemoveMatchSignal(_, _ string, _ ...dbus.MatchOption) *dbus.Call {
	return &dbus.Call{}
}

func (f *fakeBusObject) GetProperty(string) (dbus.Variant, error) { return dbus.Variant{}, nil }

func (f *fakeBusObject) StoreProperty(string, any) error { return nil }

func (f *fakeBusObject) SetProperty(string, any) error { return nil }

func (f *fakeBusObject) Destination() string { return "" }

func (f *fakeBusObject) Path() dbus.ObjectPath { return "" }

type fakeDBusConn struct {
	objects map[dbus.ObjectPath]dbus.BusObject
	closed  bool
}

func (f *fakeDBusConn) Object(_ string, path dbus.ObjectPath) dbus.BusObject {
	return f.objects[path]
}

func (f *fakeDBusConn) Close() error {
	f.closed = true
	return nil
}

func resetFirewalldSeams(t *testing.T) {
	t.Helper()
	prevEnsure := ensureFirewalldRunningFunc
	prevConnect := connectSystemBusFunc
	t.Cleanup(func() {
		ensureFirewalldRunningFunc = prevEnsure
		connectSystemBusFunc = prevConnect
	})
}

func TestQueryAndAddFirewallPort(t *testing.T) {
	fb := newFakeBus()
	fb.respond(firewalldZoneInterface+".queryPort", true)
	fb.respond(firewalldZoneInterface+".addPort", "public")

	enabled, err := queryFirewallPort(fb, "public", "8080", "tcp")
	if err != nil || !enabled {
		t.Fatalf("queryFirewallPort = %v, %v; want true, nil", enabled, err)
	}
	if err := addFirewallPort(fb, "public", "8080", "tcp"); err != nil {
		t.Fatalf("addFirewallPort: %v", err)
	}
}

func TestPruneRuntimeZone(t *testing.T) {
	fb := newFakeBus()
	fb.respond(firewalldZoneInterface+".getPorts", [][]string{{"8080", "tcp"}, {"22", "tcp"}, {"bad"}})
	fb.respond(firewalldZoneInterface+".getRichRules", []string{
		`rule family="ipv4" source address="10.0.0.0/24" port port="8080" protocol="tcp" accept`,
		`rule family="ipv4" source address="10.0.1.0/24" port port="8080" protocol="tcp" accept`,
	})
	fb.respond(firewalldZoneInterface+".getServices", []string{"ssh", "cockpit"})
	fb.respond(firewalldZoneInterface+".removePort", "public")
	fb.respond(firewalldZoneInterface+".removeRichRule", "public")
	fb.respond(firewalldZoneInterface+".removeService", "public")

	// 22/tcp is declared and must be kept; 8080/tcp is closed; the malformed
	// ["bad"] tuple is skipped; both services are removed wholesale.
	declared := declaredFirewallZone{
		ports: map[string]struct{}{firewallPortKey("22", "tcp"): {}},
		richRules: map[string]struct{}{
			`rule family="ipv4" source address="10.0.0.0/24" port port="8080" protocol="tcp" accept`: {},
		},
	}
	if err := pruneRuntimeZone(fb, "public", declared); err != nil {
		t.Fatalf("pruneRuntimeZone: %v", err)
	}
	if got := fb.callCount(firewalldZoneInterface + ".removePort"); got != 1 {
		t.Fatalf("removePort called %d times, want 1 (only 8080)", got)
	}
	if got := fb.callCount(firewalldZoneInterface + ".removeRichRule"); got != 1 {
		t.Fatalf("removeRichRule called %d times, want 1", got)
	}
	if got := fb.callCount(firewalldZoneInterface + ".removeService"); got != 2 {
		t.Fatalf("removeService called %d times, want 2", got)
	}
}

func TestRemoveUnmanagedFirewallRules(t *testing.T) {
	firewalld := newFakeBus()
	firewalld.respond(firewalldZoneInterface+".getZones", []string{"public"})
	firewalld.respond(firewalldZoneInterface+".getPorts", [][]string{{"9999", "udp"}})
	firewalld.respond(firewalldZoneInterface+".getRichRules", []string{})
	firewalld.respond(firewalldZoneInterface+".getServices", []string{"dhcp"})
	firewalld.respond(firewalldZoneInterface+".removePort", "public")
	firewalld.respond(firewalldZoneInterface+".removeRichRule", "public")
	firewalld.respond(firewalldZoneInterface+".removeService", "public")

	config := newFakeBus()
	config.respond(firewalldConfigInterface+".getZoneNames", []string{}) // no permanent zones -> conn unused

	declared := map[string]declaredFirewallZone{} // nothing declared -> close everything

	// conn is only dereferenced for permanent zones, of which there are none.
	if err := removeUnmanagedFirewallRules(nil, firewalld, config, declared); err != nil {
		t.Fatalf("removeUnmanagedFirewallRules: %v", err)
	}
	if firewalld.callCount(firewalldZoneInterface+".removePort") != 1 {
		t.Fatalf("expected the unmanaged runtime port to be closed")
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
		{Zone: "internal", Port: "8080", Protocol: "tcp", Source: "10.0.0.0/24"},
		{Port: "8080", Protocol: "tcp"}, // duplicate of the first
	}

	declared, err := declaredFirewallPorts(ports, "public")
	if err != nil {
		t.Fatalf("declaredFirewallPorts: %v", err)
	}

	// The empty-zone 8080/tcp and the explicit public 443/tcp both land in public.
	public := declared["public"].ports
	if len(public) != 2 {
		t.Fatalf("public zone keys = %v, want 2 entries", public)
	}
	if _, ok := public["8080/tcp"]; !ok {
		t.Fatalf("public zone missing 8080/tcp: %v", public)
	}
	if _, ok := public["443/tcp"]; !ok {
		t.Fatalf("public zone missing 443/tcp: %v", public)
	}

	internal := declared["internal"].ports
	if _, ok := internal["53/udp"]; !ok || len(internal) != 1 {
		t.Fatalf("internal zone = %v, want only 53/udp", internal)
	}
	internalRich := declared["internal"].richRules
	if _, ok := internalRich[`rule family="ipv4" source address="10.0.0.0/24" port port="8080" protocol="tcp" accept`]; !ok || len(internalRich) != 1 {
		t.Fatalf("internal rich rules = %v, want source-restricted rule", internalRich)
	}

	if _, ok := declared["dmz"]; ok {
		t.Fatalf("undeclared zone dmz should be absent: %v", declared)
	}
}

func TestConfigureFirewallPorts(t *testing.T) {
	resetFirewalldSeams(t)

	firewalld := newFakeBus()
	firewalld.respond(firewalldBusName+".getDefaultZone", "public")
	firewalld.respond(firewalldZoneInterface+".queryPort", false)
	firewalld.respond(firewalldZoneInterface+".addPort", "public")
	firewalld.respond(firewalldZoneInterface+".getZones", []string{"public"})
	firewalld.respond(firewalldZoneInterface+".getPorts", [][]string{{"8080", "tcp"}, {"22", "tcp"}})
	firewalld.respond(firewalldZoneInterface+".getRichRules", []string{})
	firewalld.respond(firewalldZoneInterface+".getServices", []string{})
	firewalld.respond(firewalldZoneInterface+".removePort", "public")

	config := newFakeBus()
	config.respond(firewalldConfigInterface+".getZoneByName", dbus.ObjectPath("/firewalld/public"))
	config.respond(firewalldConfigInterface+".getZoneNames", []string{"public"})

	permanentZone := newFakeBus()
	permanentZone.respond(firewalldConfigZoneInterface+".queryPort", false)
	permanentZone.respond(firewalldConfigZoneInterface+".getPorts", []firewallPortTuple{
		{Port: "8080", Protocol: "tcp"},
		{Port: "22", Protocol: "tcp"},
	})
	permanentZone.respond(firewalldConfigZoneInterface+".getRichRules", []string{})
	permanentZone.respond(firewalldConfigZoneInterface+".getServices", []string{"ssh"})

	conn := &fakeDBusConn{objects: map[dbus.ObjectPath]dbus.BusObject{
		dbus.ObjectPath(firewalldObjectPath): firewalld,
		dbus.ObjectPath(firewalldConfigPath): config,
		dbus.ObjectPath("/firewalld/public"): permanentZone,
	}}
	ensureFirewalldRunningFunc = func() error { return nil }
	connectSystemBusFunc = func() (dbusConnection, error) { return conn, nil }

	if err := configureFirewallPorts([]FirewallPortConfig{{Port: "8080"}}); err != nil {
		t.Fatalf("configureFirewallPorts: %v", err)
	}
	if !conn.closed {
		t.Fatal("expected system bus connection to be closed")
	}
	if firewalld.callCount(firewalldZoneInterface+".addPort") != 1 {
		t.Fatalf("expected runtime port to be opened, calls=%v", firewalld.calls)
	}
	if firewalld.callCount(firewalldZoneInterface+".removePort") != 1 {
		t.Fatalf("expected unmanaged runtime port to be closed, calls=%v", firewalld.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".addPort") != 1 {
		t.Fatalf("expected permanent port to be added, calls=%v", permanentZone.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".removePort") != 1 {
		t.Fatalf("expected unmanaged permanent port to be removed, calls=%v", permanentZone.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".removeService") != 1 {
		t.Fatalf("expected permanent service to be removed, calls=%v", permanentZone.calls)
	}
}

func TestConfigureFirewallPortsWithSource(t *testing.T) {
	resetFirewalldSeams(t)

	firewalld := newFakeBus()
	firewalld.respond(firewalldBusName+".getDefaultZone", "public")
	firewalld.respond(firewalldZoneInterface+".queryRichRule", false)
	firewalld.respond(firewalldZoneInterface+".addRichRule", "public")
	firewalld.respond(firewalldZoneInterface+".getZones", []string{"public"})
	firewalld.respond(firewalldZoneInterface+".getPorts", [][]string{})
	firewalld.respond(firewalldZoneInterface+".getRichRules", []string{
		`rule family="ipv4" source address="10.0.0.0/24" port port="8080" protocol="tcp" accept`,
		`rule family="ipv4" source address="10.0.1.0/24" port port="8080" protocol="tcp" accept`,
	})
	firewalld.respond(firewalldZoneInterface+".removeRichRule", "public")
	firewalld.respond(firewalldZoneInterface+".getServices", []string{})

	config := newFakeBus()
	config.respond(firewalldConfigInterface+".getZoneByName", dbus.ObjectPath("/firewalld/public"))
	config.respond(firewalldConfigInterface+".getZoneNames", []string{"public"})

	permanentZone := newFakeBus()
	permanentZone.respond(firewalldConfigZoneInterface+".queryRichRule", false)
	permanentZone.respond(firewalldConfigZoneInterface+".getPorts", []firewallPortTuple{})
	permanentZone.respond(firewalldConfigZoneInterface+".getRichRules", []string{
		`rule family="ipv4" source address="10.0.0.0/24" port port="8080" protocol="tcp" accept`,
		`rule family="ipv4" source address="10.0.1.0/24" port port="8080" protocol="tcp" accept`,
	})
	permanentZone.respond(firewalldConfigZoneInterface+".getServices", []string{})

	conn := &fakeDBusConn{objects: map[dbus.ObjectPath]dbus.BusObject{
		dbus.ObjectPath(firewalldObjectPath): firewalld,
		dbus.ObjectPath(firewalldConfigPath): config,
		dbus.ObjectPath("/firewalld/public"): permanentZone,
	}}
	ensureFirewalldRunningFunc = func() error { return nil }
	connectSystemBusFunc = func() (dbusConnection, error) { return conn, nil }

	if err := configureFirewallPorts([]FirewallPortConfig{{Port: "8080", Source: "10.0.0.0/24"}}); err != nil {
		t.Fatalf("configureFirewallPorts: %v", err)
	}
	if firewalld.callCount(firewalldZoneInterface+".addPort") != 0 {
		t.Fatalf("source-restricted rule should not call addPort, calls=%v", firewalld.calls)
	}
	if firewalld.callCount(firewalldZoneInterface+".addRichRule") != 1 {
		t.Fatalf("source-restricted rule should call addRichRule, calls=%v", firewalld.calls)
	}
	if firewalld.callCount(firewalldZoneInterface+".removeRichRule") != 1 {
		t.Fatalf("expected unmanaged runtime rich rule to be removed, calls=%v", firewalld.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".addPort") != 0 {
		t.Fatalf("source-restricted rule should not call permanent addPort, calls=%v", permanentZone.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".addRichRule") != 1 {
		t.Fatalf("source-restricted rule should call permanent addRichRule, calls=%v", permanentZone.calls)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".removeRichRule") != 1 {
		t.Fatalf("expected unmanaged permanent rich rule to be removed, calls=%v", permanentZone.calls)
	}
}

func TestConfigureFirewallPortsUnavailable(t *testing.T) {
	resetFirewalldSeams(t)
	unavailable := errors.New("firewalld missing")
	ensureFirewalldRunningFunc = func() error { return unavailable }

	if err := configureFirewallPorts(nil); err != nil {
		t.Fatalf("empty firewall config should tolerate unavailable firewalld: %v", err)
	}
	if err := configureFirewallPorts([]FirewallPortConfig{{Port: "22"}}); !errors.Is(err, unavailable) {
		t.Fatalf("configureFirewallPorts err = %v, want %v", err, unavailable)
	}
}

func TestConfigureFirewallPortsConnectError(t *testing.T) {
	resetFirewalldSeams(t)
	connectErr := errors.New("no bus")
	ensureFirewalldRunningFunc = func() error { return nil }
	connectSystemBusFunc = func() (dbusConnection, error) { return nil, connectErr }

	if err := configureFirewallPorts(nil); !errors.Is(err, connectErr) {
		t.Fatalf("configureFirewallPorts err = %v, want %v", err, connectErr)
	}
}

func TestEnsurePermanentFirewallPortAlreadyPresent(t *testing.T) {
	firewalld := newFakeBus()
	config := newFakeBus()
	config.respond(firewalldConfigInterface+".getZoneByName", dbus.ObjectPath("/firewalld/public"))
	permanentZone := newFakeBus()
	permanentZone.respond(firewalldConfigZoneInterface+".queryPort", true)
	conn := &fakeDBusConn{objects: map[dbus.ObjectPath]dbus.BusObject{
		dbus.ObjectPath("/firewalld/public"): permanentZone,
	}}

	if err := ensurePermanentFirewallPort(conn, firewalld, config, "public", "443", "tcp"); err != nil {
		t.Fatalf("ensurePermanentFirewallPort: %v", err)
	}
	if permanentZone.callCount(firewalldConfigZoneInterface+".addPort") != 0 {
		t.Fatalf("already-present port should not be added again, calls=%v", permanentZone.calls)
	}
}
