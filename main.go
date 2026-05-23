package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	nettypes "github.com/containers/common/libnetwork/types"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/specgen"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type PodmanMode string

const (
	PodmanRootful  PodmanMode = "rootful"
	PodmanRootless PodmanMode = "rootless"
)

type Config struct {
	PodmanMode string            `json:"podman_mode"`
	Interfaces []InterfaceConfig `json:"interfaces"`
	Containers []ContainerConfig `json:"containers"`
}

type ContainerConfig struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Env        map[string]string `json:"env"`
	Ports      []PortConfig      `json:"ports"`
	Volumes    []VolumeConfig    `json:"volumes"`
	Interfaces []InterfaceConfig `json:"interfaces"`
	Command    []string          `json:"command"`
	Restart    string            `json:"restart"`
}

type InterfaceConfig struct {
	Name      string   `json:"name"`
	IPAddress string   `json:"ip_address"`
	Subnet    string   `json:"subnet"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
}

type PortConfig struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type VolumeConfig struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only"`
}

func main() {
	cfg, err := loadConfig("/data/config/containers.json")
	if err != nil {
		log.Fatal(err)
	}

	mode := PodmanMode(cfg.PodmanMode)
	if mode == "" {
		mode = PodmanRootful
	}

	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}

	if err := configureHostInterfaces(cfg.Interfaces); err != nil {
		log.Fatal(err)
	}

	if err := startPodmanSocket(mode); err != nil {
		log.Fatal(err)
	}

	socketPath, err := podmanSocketPath(mode)
	if err != nil {
		log.Fatal(err)
	}

	if err := waitForUnixSocket(socketPath, 10*time.Second); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ctx, err = bindings.NewConnection(ctx, "unix:"+socketPath)
	if err != nil {
		log.Fatal(err)
	}

	for _, c := range cfg.Containers {
		if err := recreateContainer(ctx, c); err != nil {
			log.Fatal(err)
		}
	}

	log.Println("all containers started")
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	for _, c := range cfg.Containers {
		if len(c.Interfaces) > 0 {
			return fmt.Errorf("container %q defines interfaces; interfaces configure host interfaces and must be declared at the top level", c.Name)
		}
	}

	return nil
}

func startPodmanSocket(mode PodmanMode) error {
	var conn *systemd.Conn
	var err error

	switch mode {
	case PodmanRootful:
		conn, err = systemd.NewSystemConnection()
	case PodmanRootless:
		conn, err = systemd.NewUserConnection()
	default:
		return fmt.Errorf("unknown podman mode: %s", mode)
	}

	if err != nil {
		return fmt.Errorf("connect to systemd failed: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)

	_, err = conn.StartUnit("podman.socket", "replace", ch)
	if err != nil {
		return fmt.Errorf("start podman.socket failed: %w", err)
	}

	result := <-ch
	if result != "done" {
		return fmt.Errorf("podman.socket start result: %s", result)
	}

	return nil
}

func podmanSocketPath(mode PodmanMode) (string, error) {
	switch mode {
	case PodmanRootful:
		return "/run/podman/podman.sock", nil

	case PodmanRootless:
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			return "", fmt.Errorf("XDG_RUNTIME_DIR is not set")
		}
		return runtimeDir + "/podman/podman.sock", nil

	default:
		return "", fmt.Errorf("unknown podman mode: %s", mode)
	}
}

func waitForUnixSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("socket not reachable: %s", path)
}

func recreateContainer(ctx context.Context, c ContainerConfig) error {
	spec, err := createSpec(c)
	if err != nil {
		return err
	}

	_, err = images.Pull(ctx, c.Image, nil)
	if err != nil {
		return fmt.Errorf("pull image %q failed: %w", c.Image, err)
	}

	exists, err := containers.Exists(ctx, c.Name, nil)
	if err != nil {
		return fmt.Errorf("check container %q failed: %w", c.Name, err)
	}

	if exists {
		_, err = containers.Remove(ctx, c.Name, &containers.RemoveOptions{
			Force: pointer(true),
		})
		if err != nil {
			return fmt.Errorf("remove container %q failed: %w", c.Name, err)
		}
	}

	created, err := containers.CreateWithSpec(ctx, spec, nil)
	if err != nil {
		return fmt.Errorf("create container %q failed: %w", c.Name, err)
	}

	if err := containers.Start(ctx, created.ID, nil); err != nil {
		return fmt.Errorf("start container %q failed: %w", c.Name, err)
	}

	log.Printf("started container %s", c.Name)
	return nil
}

func createSpec(c ContainerConfig) (*specgen.SpecGenerator, error) {
	s := specgen.NewSpecGenerator(c.Image, false)
	s.Name = c.Name
	s.Env = c.Env
	s.Command = c.Command

	for _, p := range c.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}

		s.PortMappings = append(s.PortMappings, nettypes.PortMapping{
			HostIP:        p.HostIP,
			HostPort:      uint16(p.HostPort),
			ContainerPort: uint16(p.ContainerPort),
			Protocol:      proto,
		})
	}

	for _, v := range c.Volumes {
		options := []string{"rbind"}

		if v.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}

		s.Mounts = append(s.Mounts, specs.Mount{
			Type:        "bind",
			Source:      v.HostPath,
			Destination: v.ContainerPath,
			Options:     options,
		})
	}

	if c.Restart != "" {
		s.RestartPolicy = c.Restart
	}

	return s, nil
}

func pointer[T any](v T) *T {
	return &v
}

func configureHostInterfaces(interfaces []InterfaceConfig) error {
	for i, iface := range interfaces {
		ifaceLabel := iface.Name
		if ifaceLabel == "" {
			ifaceLabel = fmt.Sprintf("#%d", i)
		}

		if iface.Name == "" {
			return fmt.Errorf("host interface %s is missing name", ifaceLabel)
		}

		if _, err := net.InterfaceByName(iface.Name); err != nil {
			return fmt.Errorf("host interface %q not found: %w", iface.Name, err)
		}

		if (iface.IPAddress == "") != (iface.Subnet == "") {
			return fmt.Errorf("host interface %q must set both ip_address and subnet", iface.Name)
		}

		if iface.IPAddress != "" {
			ip, prefixLength, err := parseInterfaceAddress(iface.IPAddress, iface.Subnet)
			if err != nil {
				return fmt.Errorf("invalid host interface %s address: %w", ifaceLabel, err)
			}

			if err := runCommand("ip", "addr", "replace", fmt.Sprintf("%s/%d", ip.String(), prefixLength), "dev", iface.Name); err != nil {
				return fmt.Errorf("configure address for host interface %q failed: %w", iface.Name, err)
			}
		}

		if err := runCommand("ip", "link", "set", "dev", iface.Name, "up"); err != nil {
			return fmt.Errorf("bring up host interface %q failed: %w", iface.Name, err)
		}

		if iface.Gateway != "" {
			gateway, err := parseAddr(iface.Gateway)
			if err != nil {
				return fmt.Errorf("invalid gateway %q for host interface %s", iface.Gateway, ifaceLabel)
			}

			args := []string{"route", "replace", "default", "via", gateway.String(), "dev", iface.Name}
			if gateway.To4() == nil {
				args = append([]string{"-6"}, args...)
			}

			if err := runCommand("ip", args...); err != nil {
				return fmt.Errorf("configure default route for host interface %q failed: %w", iface.Name, err)
			}
		}

		if len(iface.DNS) > 0 {
			args := []string{"dns", iface.Name}
			for _, dns := range iface.DNS {
				dnsIP, err := parseAddr(dns)
				if err != nil {
					return fmt.Errorf("invalid dns server %q for host interface %s", dns, ifaceLabel)
				}
				args = append(args, dnsIP.String())
			}

			if err := runCommand("resolvectl", args...); err != nil {
				return fmt.Errorf("configure DNS for host interface %q failed: %w", iface.Name, err)
			}
		}
	}

	return nil
}

func parseInterfaceAddress(address string, subnet string) (net.IP, int, error) {
	ip, err := parseAddr(address)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid ip_address %q", address)
	}

	_, cidr, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid subnet %q: %w", subnet, err)
	}

	if !cidr.Contains(ip) {
		return nil, 0, fmt.Errorf("ip_address %q is not within subnet %q", address, subnet)
	}

	ones, _ := cidr.Mask.Size()
	return ip, ones, nil
}

func parseAddr(addr string) (net.IP, error) {
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	return net.IP(parsed.AsSlice()), nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
}
