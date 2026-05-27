package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestValidateFirewallSource(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"ipv4", "10.0.0.10", false},
		{"ipv6", "2001:db8::10", false},
		{"ipv4 cidr", "10.0.0.0/24", false},
		{"ipv6 cidr", "2001:db8::/64", false},
		{"trims whitespace", " 10.0.0.0/24 ", false},
		{"invalid", "not-an-ip", true},
		{"bad cidr", "10.0.0.0/99", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFirewallSource(tt.source)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFirewallSource(%q) error = %v, wantErr %v", tt.source, err, tt.wantErr)
			}
		})
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

func TestValidateConfigFile(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		path := writeTempConfig(t, `{"podman_mode":"rootful","firewall_ports":[{"zone":"public","port":"8080","protocol":"tcp"}]}`)
		if err := validateConfigFile(path); err != nil {
			t.Fatalf("validateConfigFile(%q) = %v, want nil", path, err)
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		path := writeTempConfig(t, `{"firewall_ports":[{"port":"70000"}]}`)
		err := validateConfigFile(path)
		if err == nil {
			t.Fatal("validateConfigFile expected error on invalid config")
		}
		if !strings.Contains(err.Error(), "firewall port") {
			t.Fatalf("error = %q, want it to mention the firewall port", err.Error())
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		path := writeTempConfig(t, "{not json")
		if err := validateConfigFile(path); err == nil {
			t.Fatal("validateConfigFile expected error on malformed JSON")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if err := validateConfigFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
			t.Fatal("validateConfigFile expected error on missing file")
		}
	})
}

func TestValidateConfigOwnerErrors(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "folder bad user", cfg: &Config{Folders: []FolderConfig{{Path: "/d", User: "no-user-xyz"}}}, want: "user"},
		{name: "file bad user", cfg: &Config{Files: []FileConfig{{Path: "/d/f", User: "no-user-xyz"}}}, want: "user"},
	})
}

func TestValidateHostname(t *testing.T) {
	valid := []string{"", "node1", "node-1", "edge.example.com", strings.Repeat("a", 63)}
	for _, h := range valid {
		if err := validateHostname(h); err != nil {
			t.Fatalf("validateHostname(%q) = %v, want nil", h, err)
		}
	}
	invalid := []string{"-bad", "bad-", "node_1", "a..b", strings.Repeat("a", 64), strings.Repeat("a.", 130) + "a"}
	for _, h := range invalid {
		if err := validateHostname(h); err == nil {
			t.Fatalf("validateHostname(%q) = nil, want error", h)
		}
	}
}

func TestValidateConfigRejectsBadHostname(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "bad hostname", cfg: &Config{Hostname: "bad_host"}, want: "hostname"},
	})
}

// validateConfigCase is a config expected to fail validation, with want naming
// a substring the returned error must contain.
type validateConfigCase struct {
	name string
	cfg  *Config
	want string
}

// assertValidateConfigErrors runs each case as a subtest asserting validateConfig
// fails with an error containing want.
func assertValidateConfigErrors(t *testing.T, cases []validateConfigCase) {
	t.Helper()
	for _, tt := range cases {
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

func TestValidateConfigValid(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{
			PodmanMode:    "rootful",
			Routes:        []RouteConfig{{Name: "corp-net", Destination: "10.0.0.0/8", NextHopInterface: "eth0", NextHopAddress: "192.168.1.1", TableID: intPtr(100)}},
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
}

func TestValidateConfigPodmanAndContainerErrors(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "rootless podman mode rejected", cfg: &Config{PodmanMode: "rootless"}, want: "podman_mode"},
		{
			name: "container declares interfaces",
			cfg:  &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx", Interfaces: []InterfaceConfig{{Name: "eth0"}}}}},
			want: "interfaces",
		},
		{
			name: "bad host port",
			cfg:  &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 70000, ContainerPort: 80}}}}},
			want: "host_port",
		},
		{
			name: "bad container port",
			cfg:  &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 80, ContainerPort: 0}}}}},
			want: "container_port",
		},
		{
			name: "bad selinux relabel",
			cfg:  &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx", Volumes: []VolumeConfig{{HostPath: "/var/data", ContainerPath: "/data", SELinux: "x"}}}}},
			want: "selinux",
		},
	})
}

func TestValidateConfigResourceErrors(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "bad firewall port", cfg: &Config{FirewallPorts: []FirewallPortConfig{{Port: "70000"}}}, want: "firewall port"},
		{name: "bad firewall protocol", cfg: &Config{FirewallPorts: []FirewallPortConfig{{Port: "8080", Protocol: "icmp"}}}, want: "protocol"},
		{name: "bad firewall source", cfg: &Config{FirewallPorts: []FirewallPortConfig{{Port: "8080", Source: "invalid"}}}, want: "source"},
		{name: "folder missing path", cfg: &Config{Folders: []FolderConfig{{Chmod: "0755"}}}, want: "missing path"},
		{name: "folder bad chmod", cfg: &Config{Folders: []FolderConfig{{Path: "/var/data", Chmod: "99999"}}}, want: "chmod"},
		{name: "file missing path", cfg: &Config{Files: []FileConfig{{Content: "hi"}}}, want: "missing path"},
		{name: "file bad chmod", cfg: &Config{Files: []FileConfig{{Path: "/var/data/x", Chmod: "99999"}}}, want: "chmod"},
		{name: "file bad encoding", cfg: &Config{Files: []FileConfig{{Path: "/var/data/x", Encoding: "rot13"}}}, want: "encoding"},
		{name: "file bad base64", cfg: &Config{Files: []FileConfig{{Path: "/var/data/x", Encoding: "base64", Content: "not!base64"}}}, want: "base64"},
		{name: "interface bad gateway", cfg: &Config{Interfaces: []InterfaceConfig{{Name: "eth0", Gateway: "not-an-ip"}}}, want: "gateway"},
		{name: "route bad destination", cfg: &Config{Routes: []RouteConfig{{Destination: "nope", NextHopInterface: "eth0"}}}, want: "route"},
		{name: "route missing next hop interface", cfg: &Config{Routes: []RouteConfig{{Destination: "0.0.0.0/0"}}}, want: "next_hop_interface"},
	})
}

func TestValidateNetworkMode(t *testing.T) {
	for _, mode := range []string{"", "host", "bridge", "none", "private", "slirp4netns", "pasta"} {
		if err := validateNetworkMode(mode); err != nil {
			t.Errorf("validateNetworkMode(%q) unexpected error: %v", mode, err)
		}
	}
	for _, mode := range []string{"container:abc", "ns:/proc/1/ns/net", "bogus"} {
		if err := validateNetworkMode(mode); err == nil {
			t.Errorf("validateNetworkMode(%q) expected error, got nil", mode)
		}
	}
}

func TestValidateCapability(t *testing.T) {
	for _, cap := range []string{"ALL", "CAP_NET_ADMIN", "CAP_SYS_PTRACE", "CAP_AUDIT_WRITE"} {
		if err := validateCapability(cap); err != nil {
			t.Errorf("validateCapability(%q) unexpected error: %v", cap, err)
		}
	}
	for _, cap := range []string{"", "net_admin", "CAP_", "CAP_net_admin", "CAP_NET-ADMIN"} {
		if err := validateCapability(cap); err == nil {
			t.Errorf("validateCapability(%q) expected error, got nil", cap)
		}
	}
}

func TestValidateContainerCapabilities(t *testing.T) {
	if err := validateContainer(ContainerConfig{
		Name:        "x",
		Image:       "busybox",
		NetworkMode: "host",
		ReadOnly:    true,
		CapAdd:      []string{"CAP_NET_ADMIN"},
		CapDrop:     []string{"ALL"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateContainer(ContainerConfig{Name: "x", Image: "busybox", NetworkMode: "wat"}); err == nil {
		t.Fatal("expected error for invalid network_mode")
	}
	if err := validateContainer(ContainerConfig{Name: "x", Image: "busybox", CapAdd: []string{"net_admin"}}); err == nil {
		t.Fatal("expected error for invalid cap_add")
	}
	if err := validateContainer(ContainerConfig{Name: "x", Image: "busybox", CapDrop: []string{""}}); err == nil {
		t.Fatal("expected error for empty cap_drop")
	}
}
