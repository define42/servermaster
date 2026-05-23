package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env"`
	Ports   []PortConfig      `json:"ports"`
	Volumes []VolumeConfig    `json:"volumes"`
	Command []string          `json:"command"`
	Restart string            `json:"restart"`
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

	if c.Restart != "" {
		s.RestartPolicy = c.Restart
	}

	return s, nil
}

func pointer[T any](v T) *T {
	return &v
}
