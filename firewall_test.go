package main

import (
	"context"
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
func (f *fakeBusObject) StoreProperty(string, any) error          { return nil }
func (f *fakeBusObject) SetProperty(string, any) error            { return nil }
func (f *fakeBusObject) Destination() string                      { return "" }
func (f *fakeBusObject) Path() dbus.ObjectPath                    { return "" }

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
	fb.respond(firewalldZoneInterface+".getServices", []string{"ssh", "cockpit"})
	fb.respond(firewalldZoneInterface+".removePort", "public")
	fb.respond(firewalldZoneInterface+".removeService", "public")

	// 22/tcp is declared and must be kept; 8080/tcp is closed; the malformed
	// ["bad"] tuple is skipped; both services are removed wholesale.
	declared := map[string]struct{}{firewallPortKey("22", "tcp"): {}}
	if err := pruneRuntimeZone(fb, "public", declared); err != nil {
		t.Fatalf("pruneRuntimeZone: %v", err)
	}
	if got := fb.callCount(firewalldZoneInterface + ".removePort"); got != 1 {
		t.Fatalf("removePort called %d times, want 1 (only 8080)", got)
	}
	if got := fb.callCount(firewalldZoneInterface + ".removeService"); got != 2 {
		t.Fatalf("removeService called %d times, want 2", got)
	}
}

func TestRemoveUnmanagedFirewallRules(t *testing.T) {
	firewalld := newFakeBus()
	firewalld.respond(firewalldZoneInterface+".getZones", []string{"public"})
	firewalld.respond(firewalldZoneInterface+".getPorts", [][]string{{"9999", "udp"}})
	firewalld.respond(firewalldZoneInterface+".getServices", []string{"dhcp"})
	firewalld.respond(firewalldZoneInterface+".removePort", "public")
	firewalld.respond(firewalldZoneInterface+".removeService", "public")

	config := newFakeBus()
	config.respond(firewalldConfigInterface+".getZoneNames", []string{}) // no permanent zones -> conn unused

	declared := map[string]map[string]struct{}{} // nothing declared -> close everything

	// conn is only dereferenced for permanent zones, of which there are none.
	if err := removeUnmanagedFirewallRules(nil, firewalld, config, declared); err != nil {
		t.Fatalf("removeUnmanagedFirewallRules: %v", err)
	}
	if firewalld.callCount(firewalldZoneInterface+".removePort") != 1 {
		t.Fatalf("expected the unmanaged runtime port to be closed")
	}
}
