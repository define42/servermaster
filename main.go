package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
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
	Network   string   `json:"network"`
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

	if len(c.Interfaces) > 0 {
		s.Networks = make(map[string]nettypes.PerNetworkOptions, len(c.Interfaces))
		dnsServers := make([]net.IP, 0)

		for _, iface := range c.Interfaces {
			networkName := iface.Network
			if networkName == "" {
				networkName = "podman"
			}

			network := s.Networks[networkName]
			if iface.Name != "" {
				network.InterfaceName = iface.Name
			}

			if iface.IPAddress != "" {
				ip := net.ParseIP(iface.IPAddress)
				if ip == nil {
					return nil, fmt.Errorf("invalid ip_address %q for container %q interface %q", iface.IPAddress, c.Name, iface.Name)
				}
				network.StaticIPs = append(network.StaticIPs, ip)
			}

			if iface.Subnet != "" {
				if network.Options == nil {
					network.Options = map[string]string{}
				}
				network.Options["subnet"] = iface.Subnet
			}

			if iface.Gateway != "" {
				gateway := net.ParseIP(iface.Gateway)
				if gateway == nil {
					return nil, fmt.Errorf("invalid gateway %q for container %q interface %q", iface.Gateway, c.Name, iface.Name)
				}
				if network.Options == nil {
					network.Options = map[string]string{}
				}
				network.Options["gateway"] = gateway.String()
			}

			for _, dns := range iface.DNS {
				dnsIP := net.ParseIP(dns)
				if dnsIP == nil {
					return nil, fmt.Errorf("invalid dns server %q for container %q interface %q", dns, c.Name, iface.Name)
				}
				dnsServers = append(dnsServers, dnsIP)
			}

			s.Networks[networkName] = network
		}

		if len(dnsServers) > 0 {
			s.DNSServers = dnsServers
		}
	}

	if c.Restart != "" {
		s.RestartPolicy = c.Restart
	}

	return s, nil
}

func pointer[T any](v T) *T {
	return &v
}
