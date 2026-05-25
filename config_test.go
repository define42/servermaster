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
			cfg:  &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx", Volumes: []VolumeConfig{{HostPath: "/data", ContainerPath: "/data", SELinux: "x"}}}}},
			want: "selinux",
		},
	})
}

func TestValidateConfigResourceErrors(t *testing.T) {
	assertValidateConfigErrors(t, []validateConfigCase{
		{name: "bad firewall port", cfg: &Config{FirewallPorts: []FirewallPortConfig{{Port: "70000"}}}, want: "firewall port"},
		{name: "bad firewall protocol", cfg: &Config{FirewallPorts: []FirewallPortConfig{{Port: "8080", Protocol: "icmp"}}}, want: "protocol"},
		{name: "folder missing path", cfg: &Config{Folders: []FolderConfig{{Chmod: "0755"}}}, want: "missing path"},
		{name: "folder bad chmod", cfg: &Config{Folders: []FolderConfig{{Path: "/data", Chmod: "99999"}}}, want: "chmod"},
		{name: "file missing path", cfg: &Config{Files: []FileConfig{{Content: "hi"}}}, want: "missing path"},
		{name: "file bad chmod", cfg: &Config{Files: []FileConfig{{Path: "/data/x", Chmod: "99999"}}}, want: "chmod"},
		{name: "file bad encoding", cfg: &Config{Files: []FileConfig{{Path: "/data/x", Encoding: "rot13"}}}, want: "encoding"},
		{name: "file bad base64", cfg: &Config{Files: []FileConfig{{Path: "/data/x", Encoding: "base64", Content: "not!base64"}}}, want: "base64"},
		{name: "interface bad gateway", cfg: &Config{Interfaces: []InterfaceConfig{{Name: "eth0", Gateway: "not-an-ip"}}}, want: "gateway"},
	})
}
