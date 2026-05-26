package main

import (
	"fmt"
	"log"
	"net/netip"
	"strings"

	dbus "github.com/godbus/dbus/v5"
)

const (
	firewalldBusName       = "org.fedoraproject.FirewallD1"
	firewalldObjectPath    = "/org/fedoraproject/FirewallD1"
	firewalldZoneInterface = "org.fedoraproject.FirewallD1.zone"

	// Permanent configuration lives behind the config interface, addressed by
	// an explicit zone name, and survives a firewalld reload and reboot.
	firewalldConfigPath          = "/org/fedoraproject/FirewallD1/config"
	firewalldConfigInterface     = "org.fedoraproject.FirewallD1.config"
	firewalldConfigZoneInterface = "org.fedoraproject.FirewallD1.config.zone"
)

type dbusConnection interface {
	Object(dest string, path dbus.ObjectPath) dbus.BusObject
	Close() error
}

//nolint:gochecknoglobals // injectable seams so firewalld D-Bus logic can be tested with fakes.
var (
	ensureFirewalldRunningFunc = ensureFirewalldRunning
	connectSystemBusFunc       = func() (dbusConnection, error) { return dbus.ConnectSystemBus() }
)

// configureFirewallPorts enforces config.json as the single source of truth for
// firewalld: it opens (and persists) every declared port, then closes any port
// not declared and removes every firewalld service. Because the config owns the
// entire firewall surface, an empty list is not a no-op — it still runs the
// cleanup so no undeclared port/rich-rule and no service is left open. Access is
// expressed as ports (optionally source-restricted), so service-provided access
// (notably the default ssh service) survives only if the corresponding port (for
// example 22/tcp) is declared.
func configureFirewallPorts(ports []FirewallPortConfig) error {
	// firewalld owns its D-Bus name only while running and is not D-Bus
	// activatable on a default install, so bring it up before talking to it.
	// firewalld is an optional (Recommends) dependency: if it cannot be started
	// and no ports are declared there is nothing to enforce, so skip; if ports
	// are declared the config cannot be satisfied, so fail.
	if err := ensureFirewalldRunningFunc(); err != nil {
		if len(ports) == 0 {
			log.Printf("skipping firewall reconcile, firewalld unavailable: %v", err)
			return nil
		}
		return err
	}

	conn, err := connectSystemBusFunc()
	if err != nil {
		return fmt.Errorf("connect to system bus failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	firewalld := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldObjectPath))
	config := conn.Object(firewalldBusName, dbus.ObjectPath(firewalldConfigPath))

	var defaultZone string
	if err := firewalld.Call(firewalldBusName+".getDefaultZone", 0).Store(&defaultZone); err != nil {
		return fmt.Errorf("get default zone failed: %w", err)
	}

	for _, port := range ports {
		if err := openDeclaredFirewallPort(conn, firewalld, config, port); err != nil {
			return err
		}
	}

	declared, err := declaredFirewallPorts(ports, defaultZone)
	if err != nil {
		return err
	}
	if err := removeUnmanagedFirewallRules(conn, firewalld, config, declared); err != nil {
		return err
	}

	return nil
}

// openDeclaredFirewallPort opens a single declared port in both the runtime and
// permanent firewalld configuration, defaulting an empty protocol to tcp.
func openDeclaredFirewallPort(conn dbusConnection, firewalld, config dbus.BusObject, port FirewallPortConfig) error {
	zone := strings.TrimSpace(port.Zone)
	portValue := strings.TrimSpace(port.Port)
	protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
	source := strings.TrimSpace(port.Source)
	if protocol == "" {
		protocol = "tcp"
	}
	if source == "" {
		return openDeclaredPlainFirewallPort(conn, firewalld, config, zone, portValue, protocol)
	}
	return openDeclaredSourceFirewallPort(conn, firewalld, config, zone, source, portValue, protocol)
}

func openDeclaredSourceFirewallPort(conn dbusConnection, firewalld, config dbus.BusObject, zone, source, portValue, protocol string) error {
	rule, err := firewallRichRule(source, portValue, protocol)
	if err != nil {
		return fmt.Errorf("build firewall rich rule for source %q failed: %w", source, err)
	}
	enabled, err := queryFirewallRichRule(firewalld, zone, rule)
	if err != nil {
		return fmt.Errorf("query firewall rich rule %q failed: %w", rule, err)
	}
	if !enabled {
		if err := addFirewallRichRule(firewalld, zone, rule); err != nil {
			return fmt.Errorf("open firewall rich rule %q failed: %w", rule, err)
		}
		log.Printf("opened firewall rich rule %q", rule)
	}
	if err := ensurePermanentFirewallRichRule(conn, firewalld, config, zone, rule); err != nil {
		return fmt.Errorf("persist firewall rich rule %q failed: %w", rule, err)
	}
	return nil
}

func openDeclaredPlainFirewallPort(conn dbusConnection, firewalld, config dbus.BusObject, zone, portValue, protocol string) error {
	// Runtime config takes effect immediately, without a firewalld reload.
	enabled, err := queryFirewallPort(firewalld, zone, portValue, protocol)
	if err != nil {
		return fmt.Errorf("query firewall port %s/%s failed: %w", portValue, protocol, err)
	}
	if !enabled {
		if err := addFirewallPort(firewalld, zone, portValue, protocol); err != nil {
			return fmt.Errorf("open firewall port %s/%s failed: %w", portValue, protocol, err)
		}
		log.Printf("opened firewall port %s/%s", portValue, protocol)
	}

	// Permanent config survives a firewalld reload and a reboot.
	if err := ensurePermanentFirewallPort(conn, firewalld, config, zone, portValue, protocol); err != nil {
		return fmt.Errorf("persist firewall port %s/%s failed: %w", portValue, protocol, err)
	}
	return nil
}

// firewallPortTuple decodes a firewalld permanent-config (port, protocol) struct
// (D-Bus signature `(ss)`).
type firewallPortTuple struct {
	Port     string
	Protocol string
}

type declaredFirewallZone struct {
	ports     map[string]struct{}
	richRules map[string]struct{}
}

func firewallRichRule(source, port, protocol string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("missing source")
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	family, normalizedSource, err := firewallSourceFamily(source)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		`rule family="%s" source address="%s" port port="%s" protocol="%s" accept`,
		family,
		normalizedSource,
		strings.TrimSpace(port),
		protocol,
	), nil
}

func firewallSourceFamily(source string) (family, normalized string, err error) {
	if addr, parseErr := netip.ParseAddr(source); parseErr == nil {
		family = "ipv4"
		if addr.Is6() {
			family = "ipv6"
		}
		return family, addr.String(), nil
	}
	prefix, parseErr := netip.ParsePrefix(source)
	if parseErr != nil {
		return "", "", fmt.Errorf("source must be a valid IP address or CIDR: %w", parseErr)
	}
	family = "ipv4"
	if prefix.Addr().Is6() {
		family = "ipv6"
	}
	return family, prefix.String(), nil
}

// firewallPortKey normalizes a port and protocol into a comparison key, applying
// the same defaulting (lowercase protocol, empty protocol means tcp) used when
// opening declared ports so declared and live ports compare equal.
func firewallPortKey(port, protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	return strings.TrimSpace(port) + "/" + protocol
}

// declaredFirewallPorts groups the config ports by their resolved zone, returning
// per-zone sets of plain "port/proto" keys and source-restricted rich rules. An
// empty zone resolves to defaultZone, the same substitution firewalld makes when
// a rule is opened without a zone.
func declaredFirewallPorts(ports []FirewallPortConfig, defaultZone string) (map[string]declaredFirewallZone, error) {
	declared := make(map[string]declaredFirewallZone)
	for _, port := range ports {
		zone := strings.TrimSpace(port.Zone)
		if zone == "" {
			zone = defaultZone
		}
		zoneDeclared := declared[zone]
		if zoneDeclared.ports == nil {
			zoneDeclared.ports = make(map[string]struct{})
		}
		if zoneDeclared.richRules == nil {
			zoneDeclared.richRules = make(map[string]struct{})
		}
		source := strings.TrimSpace(port.Source)
		if source == "" {
			zoneDeclared.ports[firewallPortKey(port.Port, port.Protocol)] = struct{}{}
		} else {
			rule, err := firewallRichRule(source, port.Port, port.Protocol)
			if err != nil {
				return nil, fmt.Errorf("build firewall rich rule for source %q failed: %w", source, err)
			}
			zoneDeclared.richRules[rule] = struct{}{}
		}
		declared[zone] = zoneDeclared
	}
	return declared, nil
}

// removeUnmanagedFirewallRules enforces config.json as the single source of
// truth for firewalld: across every zone, in both the runtime and permanent
// configuration, it closes every open port not present in declared and removes
// every service. Services are stripped wholesale because config.json expresses
// access only as explicit port/rule declarations — so any service-provided access
// (notably the default `ssh` service) survives a reconcile only if re-declared
// as a port. declared maps a zone name to the set of allowed plain ports and
// rich rules in that zone.
func removeUnmanagedFirewallRules(conn dbusConnection, firewalld, config dbus.BusObject, declared map[string]declaredFirewallZone) error {
	if err := removeUnmanagedRuntimeRules(firewalld, declared); err != nil {
		return err
	}
	return removeUnmanagedPermanentRules(conn, config, declared)
}

// removeUnmanagedRuntimeRules prunes the runtime configuration, where changes
// take effect immediately.
func removeUnmanagedRuntimeRules(firewalld dbus.BusObject, declared map[string]declaredFirewallZone) error {
	var zones []string
	if err := firewalld.Call(firewalldZoneInterface+".getZones", 0).Store(&zones); err != nil {
		return fmt.Errorf("list runtime firewall zones failed: %w", err)
	}
	for _, zone := range zones {
		if err := pruneRuntimeZone(firewalld, zone, declared[zone]); err != nil {
			return err
		}
	}
	return nil
}

func pruneRuntimeZone(firewalld dbus.BusObject, zone string, declared declaredFirewallZone) error {
	if err := pruneRuntimeZonePorts(firewalld, zone, declared); err != nil {
		return err
	}
	if err := pruneRuntimeZoneRichRules(firewalld, zone, declared); err != nil {
		return err
	}
	return pruneRuntimeZoneServices(firewalld, zone)
}

func pruneRuntimeZonePorts(firewalld dbus.BusObject, zone string, declared declaredFirewallZone) error {
	var current [][]string
	if err := firewalld.Call(firewalldZoneInterface+".getPorts", 0, zone).Store(&current); err != nil {
		return fmt.Errorf("list runtime ports for zone %q failed: %w", zone, err)
	}
	for _, pp := range current {
		if len(pp) != 2 {
			continue
		}
		port, protocol := pp[0], pp[1]
		if _, ok := declared.ports[firewallPortKey(port, protocol)]; ok {
			continue
		}
		var appliedZone string
		if err := firewalld.Call(firewalldZoneInterface+".removePort", 0, zone, port, protocol).Store(&appliedZone); err != nil {
			return fmt.Errorf("close unmanaged firewall port %s/%s in zone %q failed: %w", port, protocol, zone, err)
		}
		log.Printf("closed unmanaged firewall port %s/%s in zone %s", port, protocol, zone)
	}
	return nil
}

func pruneRuntimeZoneRichRules(firewalld dbus.BusObject, zone string, declared declaredFirewallZone) error {
	var richRules []string
	if err := firewalld.Call(firewalldZoneInterface+".getRichRules", 0, zone).Store(&richRules); err != nil {
		return fmt.Errorf("list runtime rich rules for zone %q failed: %w", zone, err)
	}
	for _, rule := range richRules {
		if _, ok := declared.richRules[rule]; ok {
			continue
		}
		var appliedZone string
		if err := firewalld.Call(firewalldZoneInterface+".removeRichRule", 0, zone, rule).Store(&appliedZone); err != nil {
			return fmt.Errorf("remove unmanaged firewall rich rule %q in zone %q failed: %w", rule, zone, err)
		}
		log.Printf("removed unmanaged firewall rich rule %q in zone %s", rule, zone)
	}
	return nil
}

func pruneRuntimeZoneServices(firewalld dbus.BusObject, zone string) error {
	var services []string
	if err := firewalld.Call(firewalldZoneInterface+".getServices", 0, zone).Store(&services); err != nil {
		return fmt.Errorf("list runtime services for zone %q failed: %w", zone, err)
	}
	for _, service := range services {
		var appliedZone string
		if err := firewalld.Call(firewalldZoneInterface+".removeService", 0, zone, service).Store(&appliedZone); err != nil {
			return fmt.Errorf("remove firewall service %q in zone %q failed: %w", service, zone, err)
		}
		log.Printf("removed firewall service %s in zone %s", service, zone)
	}
	return nil
}

// removeUnmanagedPermanentRules prunes the permanent configuration, where
// changes survive a firewalld reload and a reboot.
func removeUnmanagedPermanentRules(conn dbusConnection, config dbus.BusObject, declared map[string]declaredFirewallZone) error {
	var zones []string
	if err := config.Call(firewalldConfigInterface+".getZoneNames", 0).Store(&zones); err != nil {
		return fmt.Errorf("list permanent firewall zones failed: %w", err)
	}
	for _, zone := range zones {
		if err := prunePermanentZone(conn, config, zone, declared[zone]); err != nil {
			return err
		}
	}
	return nil
}

func prunePermanentZone(conn dbusConnection, config dbus.BusObject, zone string, declared declaredFirewallZone) error {
	var zonePath dbus.ObjectPath
	if err := config.Call(firewalldConfigInterface+".getZoneByName", 0, zone).Store(&zonePath); err != nil {
		return fmt.Errorf("look up permanent zone %q failed: %w", zone, err)
	}
	zoneObject := conn.Object(firewalldBusName, zonePath)

	if err := prunePermanentZonePorts(zoneObject, zone, declared); err != nil {
		return err
	}
	if err := prunePermanentZoneRichRules(zoneObject, zone, declared); err != nil {
		return err
	}
	return prunePermanentZoneServices(zoneObject, zone)
}

func prunePermanentZonePorts(zoneObject dbus.BusObject, zone string, declared declaredFirewallZone) error {
	var current []firewallPortTuple
	if err := zoneObject.Call(firewalldConfigZoneInterface+".getPorts", 0).Store(&current); err != nil {
		return fmt.Errorf("list permanent ports for zone %q failed: %w", zone, err)
	}
	for _, pp := range current {
		if _, ok := declared.ports[firewallPortKey(pp.Port, pp.Protocol)]; ok {
			continue
		}
		if err := zoneObject.Call(firewalldConfigZoneInterface+".removePort", 0, pp.Port, pp.Protocol).Err; err != nil {
			return fmt.Errorf("remove permanent firewall port %s/%s in zone %q failed: %w", pp.Port, pp.Protocol, zone, err)
		}
		log.Printf("removed unmanaged permanent firewall port %s/%s in zone %s", pp.Port, pp.Protocol, zone)
	}
	return nil
}

func prunePermanentZoneRichRules(zoneObject dbus.BusObject, zone string, declared declaredFirewallZone) error {
	var richRules []string
	if err := zoneObject.Call(firewalldConfigZoneInterface+".getRichRules", 0).Store(&richRules); err != nil {
		return fmt.Errorf("list permanent rich rules for zone %q failed: %w", zone, err)
	}
	for _, rule := range richRules {
		if _, ok := declared.richRules[rule]; ok {
			continue
		}
		if err := zoneObject.Call(firewalldConfigZoneInterface+".removeRichRule", 0, rule).Err; err != nil {
			return fmt.Errorf("remove permanent firewall rich rule %q in zone %q failed: %w", rule, zone, err)
		}
		log.Printf("removed unmanaged permanent firewall rich rule %q in zone %s", rule, zone)
	}
	return nil
}

func prunePermanentZoneServices(zoneObject dbus.BusObject, zone string) error {
	var services []string
	if err := zoneObject.Call(firewalldConfigZoneInterface+".getServices", 0).Store(&services); err != nil {
		return fmt.Errorf("list permanent services for zone %q failed: %w", zone, err)
	}
	for _, service := range services {
		if err := zoneObject.Call(firewalldConfigZoneInterface+".removeService", 0, service).Err; err != nil {
			return fmt.Errorf("remove permanent firewall service %q in zone %q failed: %w", service, zone, err)
		}
		log.Printf("removed permanent firewall service %s in zone %s", service, zone)
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

func queryFirewallRichRule(firewalld dbus.BusObject, zone string, rule string) (bool, error) {
	var enabled bool
	err := firewalld.Call(firewalldZoneInterface+".queryRichRule", 0, zone, rule).Store(&enabled)
	return enabled, err
}

func addFirewallRichRule(firewalld dbus.BusObject, zone string, rule string) error {
	var appliedZone string
	return firewalld.Call(firewalldZoneInterface+".addRichRule", 0, zone, rule, int32(0)).Store(&appliedZone)
}

// ensurePermanentFirewallPort writes the port into firewalld's permanent
// configuration. The runtime config opened above is reset to the permanent
// config on `firewall-cmd --reload`, so without this the port would silently
// close until the next reconcile at boot. An empty zone resolves to firewalld's
// default zone, since the permanent config is addressed by an explicit name.
func ensurePermanentFirewallPort(conn dbusConnection, firewalld, config dbus.BusObject, zone, port, protocol string) error {
	zoneName := zone
	if zoneName == "" {
		if err := firewalld.Call(firewalldBusName+".getDefaultZone", 0).Store(&zoneName); err != nil {
			return fmt.Errorf("get default zone failed: %w", err)
		}
	}

	var zonePath dbus.ObjectPath
	if err := config.Call(firewalldConfigInterface+".getZoneByName", 0, zoneName).Store(&zonePath); err != nil {
		return fmt.Errorf("look up permanent zone %q failed: %w", zoneName, err)
	}

	zoneObject := conn.Object(firewalldBusName, zonePath)

	var enabled bool
	if err := zoneObject.Call(firewalldConfigZoneInterface+".queryPort", 0, port, protocol).Store(&enabled); err != nil {
		return fmt.Errorf("query permanent firewall port failed: %w", err)
	}
	if enabled {
		return nil
	}

	if err := zoneObject.Call(firewalldConfigZoneInterface+".addPort", 0, port, protocol).Err; err != nil {
		return fmt.Errorf("add permanent firewall port failed: %w", err)
	}

	log.Printf("persisted firewall port %s/%s in zone %s", port, protocol, zoneName)
	return nil
}

func ensurePermanentFirewallRichRule(conn dbusConnection, firewalld, config dbus.BusObject, zone, rule string) error {
	zoneName := zone
	if zoneName == "" {
		if err := firewalld.Call(firewalldBusName+".getDefaultZone", 0).Store(&zoneName); err != nil {
			return fmt.Errorf("get default zone failed: %w", err)
		}
	}

	var zonePath dbus.ObjectPath
	if err := config.Call(firewalldConfigInterface+".getZoneByName", 0, zoneName).Store(&zonePath); err != nil {
		return fmt.Errorf("look up permanent zone %q failed: %w", zoneName, err)
	}

	zoneObject := conn.Object(firewalldBusName, zonePath)

	var enabled bool
	if err := zoneObject.Call(firewalldConfigZoneInterface+".queryRichRule", 0, rule).Store(&enabled); err != nil {
		return fmt.Errorf("query permanent firewall rich rule failed: %w", err)
	}
	if enabled {
		return nil
	}

	if err := zoneObject.Call(firewalldConfigZoneInterface+".addRichRule", 0, rule).Err; err != nil {
		return fmt.Errorf("add permanent firewall rich rule failed: %w", err)
	}

	log.Printf("persisted firewall rich rule %q in zone %s", rule, zoneName)
	return nil
}
