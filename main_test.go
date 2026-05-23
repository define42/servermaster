package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
			{HostPath: "/data/web", ContainerPath: "/usr/share/nginx/html", ReadOnly: true},
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

	if !reflect.DeepEqual(spec.Mounts[0].Options, []string{"rbind", "ro"}) {
		t.Fatalf("read-only mount options = %v, want [rbind ro]", spec.Mounts[0].Options)
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

func TestValidateConfig(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{
			FirewallPorts: []FirewallPortConfig{{Zone: "public", Port: "8080", Protocol: "tcp"}},
			Containers: []ContainerConfig{
				{Name: "web", Image: "nginx", Ports: []PortConfig{{HostPort: 8081, ContainerPort: 80}}},
			},
		}
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	errorCases := []struct {
		name string
		cfg  *Config
		want string
	}{
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

func TestSystemdQuoteArg(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{"plain path", "/data/config/containers.json", `"/data/config/containers.json"`},
		{"escapes percent", "a%b", `"a%%b"`},
		{"escapes quotes", `a"b`, `"a\"b"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemdQuoteArg(tt.arg); got != tt.want {
				t.Fatalf("systemdQuoteArg(%q) = %q, want %q", tt.arg, got, tt.want)
			}
		})
	}
}
