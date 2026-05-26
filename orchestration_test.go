package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func resetServiceSeams(t *testing.T) {
	t.Helper()
	prevRunService := runServiceFunc
	prevStartWebServer := startWebServerFunc
	t.Cleanup(func() {
		runServiceFunc = prevRunService
		startWebServerFunc = prevStartWebServer
	})
}

func resetApplyConfigSeams(t *testing.T) {
	t.Helper()
	prevValidateConfig := validateConfigFunc
	prevEnsureHostname := ensureHostnameFunc
	prevEnsureFolders := ensureFoldersFunc
	prevEnsureFiles := ensureFilesFunc
	prevConfigureHostInterfaces := configureHostInterfacesFunc
	prevConfigureFirewallPorts := configureFirewallPortsFunc
	prevStartPodmanSocket := startPodmanSocketFunc
	prevWaitForUnixSocket := waitForUnixSocketFunc
	prevNewPodmanConnection := newPodmanConnectionFunc
	prevStopUnmanagedContainers := stopUnmanagedContainersFunc
	prevReconcileContainer := reconcileContainerFunc
	t.Cleanup(func() {
		validateConfigFunc = prevValidateConfig
		ensureHostnameFunc = prevEnsureHostname
		ensureFoldersFunc = prevEnsureFolders
		ensureFilesFunc = prevEnsureFiles
		configureHostInterfacesFunc = prevConfigureHostInterfaces
		configureFirewallPortsFunc = prevConfigureFirewallPorts
		startPodmanSocketFunc = prevStartPodmanSocket
		waitForUnixSocketFunc = prevWaitForUnixSocket
		newPodmanConnectionFunc = prevNewPodmanConnection
		stopUnmanagedContainersFunc = prevStopUnmanagedContainers
		reconcileContainerFunc = prevReconcileContainer
	})
}

func installSuccessfulApplyConfigSeams(t *testing.T) *[]string {
	t.Helper()
	resetApplyConfigSeams(t)

	var calls []string
	installHostApplySeams(&calls)
	installPodmanApplySeams(t, &calls)

	return &calls
}

func installHostApplySeams(calls *[]string) {
	validateConfigFunc = func(*Config) error {
		*calls = append(*calls, "validate")
		return nil
	}
	ensureHostnameFunc = func(hostname string) error {
		*calls = append(*calls, "hostname:"+hostname)
		return nil
	}
	ensureFoldersFunc = func([]FolderConfig) error {
		*calls = append(*calls, "folders")
		return nil
	}
	ensureFilesFunc = func([]FileConfig) error {
		*calls = append(*calls, "files")
		return nil
	}
	configureHostInterfacesFunc = func([]InterfaceConfig, []RouteConfig) error {
		*calls = append(*calls, "interfaces")
		return nil
	}
	configureFirewallPortsFunc = func([]FirewallPortConfig) error {
		*calls = append(*calls, "firewall")
		return nil
	}
}

func installPodmanApplySeams(t *testing.T, calls *[]string) {
	t.Helper()
	startPodmanSocketFunc = func() error {
		*calls = append(*calls, "podman-socket")
		return nil
	}
	waitForUnixSocketFunc = func(path string, timeout time.Duration) error {
		*calls = append(*calls, "wait:"+path)
		if timeout != 10*time.Second {
			t.Fatalf("wait timeout = %s, want 10s", timeout)
		}
		return nil
	}
	newPodmanConnectionFunc = func(ctx context.Context, uri string) (context.Context, error) {
		*calls = append(*calls, "connect:"+uri)
		return ctx, nil
	}
	stopUnmanagedContainersFunc = func(_ context.Context, containers []ContainerConfig) error {
		*calls = append(*calls, "stop-unmanaged")
		if len(containers) != 2 {
			t.Fatalf("containers = %d, want 2", len(containers))
		}
		return nil
	}
	reconcileContainerFunc = func(_ context.Context, c ContainerConfig) error {
		*calls = append(*calls, "reconcile:"+c.Name)
		return nil
	}
}

func TestMainParsesFlagsAndRunsService(t *testing.T) {
	resetServiceSeams(t)
	prevArgs := os.Args
	prevFlags := flag.CommandLine
	prevLogOutput := log.Writer()
	t.Cleanup(func() {
		os.Args = prevArgs
		flag.CommandLine = prevFlags
		log.SetOutput(prevLogOutput)
	})

	var gotListen, gotConfig string
	runServiceFunc = func(listenAddress, configPath string) error {
		gotListen = listenAddress
		gotConfig = configPath
		return nil
	}
	flag.CommandLine = flag.NewFlagSet("servermaster", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{"servermaster", "-listen", "unix:///tmp/servermaster.sock", "-config", "/tmp/servermaster.json"}

	main()

	if gotListen != "unix:///tmp/servermaster.sock" || gotConfig != "/tmp/servermaster.json" {
		t.Fatalf("runServiceFunc called with (%q, %q)", gotListen, gotConfig)
	}
}

func TestRunServiceOrchestration(t *testing.T) {
	resetServiceSeams(t)
	applied := false
	defer stubConfigApplier(func(*Config) error {
		applied = true
		return nil
	})()

	errCh := make(chan error, 1)
	errCh <- nil
	var gotListen, gotConfig string
	startWebServerFunc = func(listenAddress, configPath string) (*http.Server, <-chan error, error) {
		gotListen = listenAddress
		gotConfig = configPath
		return nil, errCh, nil
	}

	cfgPath := writeTempConfig(t, `{}`)
	if err := runService(":0", cfgPath); err != nil {
		t.Fatalf("runService: %v", err)
	}
	if gotListen != ":0" || gotConfig != cfgPath {
		t.Fatalf("startWebServerFunc called with (%q, %q)", gotListen, gotConfig)
	}
	if !applied {
		t.Fatal("expected startup config to be applied")
	}
}

func TestRunServiceReturnsStartAndServeErrors(t *testing.T) {
	t.Run("start error", func(t *testing.T) {
		resetServiceSeams(t)
		startErr := errors.New("bind failed")
		startWebServerFunc = func(string, string) (*http.Server, <-chan error, error) {
			return nil, nil, startErr
		}
		if err := runService(":0", "config.json"); !errors.Is(err, startErr) {
			t.Fatalf("runService err = %v, want %v", err, startErr)
		}
	})

	t.Run("serve error after reconcile", func(t *testing.T) {
		resetServiceSeams(t)
		defer stubConfigApplier(func(*Config) error { return nil })()
		serveErr := errors.New("serve failed")
		errCh := make(chan error, 1)
		errCh <- serveErr
		startWebServerFunc = func(string, string) (*http.Server, <-chan error, error) {
			return nil, errCh, nil
		}
		if err := runService(":0", writeTempConfig(t, `{}`)); !errors.Is(err, serveErr) {
			t.Fatalf("runService err = %v, want %v", err, serveErr)
		}
	})

	t.Run("reconcile error keeps server up", func(t *testing.T) {
		resetServiceSeams(t)
		defer stubConfigApplier(func(*Config) error { return errors.New("apply failed") })()
		errCh := make(chan error, 1)
		errCh <- nil
		startWebServerFunc = func(string, string) (*http.Server, <-chan error, error) {
			return nil, errCh, nil
		}
		if err := runService(":0", writeTempConfig(t, `{}`)); err != nil {
			t.Fatalf("runService should keep serving after reconcile failure: %v", err)
		}
	})
}

func TestRunUsesConfigApplier(t *testing.T) {
	var applied *Config
	defer stubConfigApplier(func(c *Config) error {
		applied = c
		return nil
	})()

	if err := run(writeTempConfig(t, `{"hostname":"edge-one"}`)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if applied == nil || applied.Hostname != "edge-one" {
		t.Fatalf("applied config = %+v", applied)
	}
	if err := run("/no/such/config.json"); err == nil {
		t.Fatal("expected load error for missing config")
	}
}

func TestApplyConfigOrchestratesHostSteps(t *testing.T) {
	calls := installSuccessfulApplyConfigSeams(t)
	prevSocketPath := podmanSocketPath
	podmanSocketPath = "/tmp/podman.sock"
	t.Cleanup(func() { podmanSocketPath = prevSocketPath })

	cfg := &Config{
		Hostname:   "edge-one",
		Containers: []ContainerConfig{{Name: "web", Image: "nginx"}, {Name: "db", Image: "postgres"}},
	}
	if err := applyConfig(cfg); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	want := []string{
		"validate",
		"hostname:edge-one",
		"folders",
		"files",
		"interfaces",
		"firewall",
		"podman-socket",
		"wait:/tmp/podman.sock",
		"connect:unix:/tmp/podman.sock",
		"stop-unmanaged",
		"reconcile:web",
		"reconcile:db",
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
}

func TestApplyConfigStopsOnStepErrors(t *testing.T) {
	sentinel := errors.New("boom")
	cfg := &Config{Containers: []ContainerConfig{{Name: "web", Image: "nginx"}, {Name: "db", Image: "postgres"}}}

	tests := []struct {
		name   string
		inject func()
	}{
		{"validate", func() { validateConfigFunc = func(*Config) error { return sentinel } }},
		{"hostname", func() { ensureHostnameFunc = func(string) error { return sentinel } }},
		{"folders", func() { ensureFoldersFunc = func([]FolderConfig) error { return sentinel } }},
		{"files", func() { ensureFilesFunc = func([]FileConfig) error { return sentinel } }},
		{"interfaces", func() { configureHostInterfacesFunc = func([]InterfaceConfig, []RouteConfig) error { return sentinel } }},
		{"firewall", func() { configureFirewallPortsFunc = func([]FirewallPortConfig) error { return sentinel } }},
		{"podman socket", func() { startPodmanSocketFunc = func() error { return sentinel } }},
		{"socket wait", func() { waitForUnixSocketFunc = func(string, time.Duration) error { return sentinel } }},
		{"podman connection", func() {
			newPodmanConnectionFunc = func(context.Context, string) (context.Context, error) {
				return nil, sentinel
			}
		}},
		{"stop unmanaged", func() {
			stopUnmanagedContainersFunc = func(context.Context, []ContainerConfig) error { return sentinel }
		}},
		{"reconcile", func() {
			reconcileContainerFunc = func(_ context.Context, c ContainerConfig) error {
				if c.Name == "web" {
					return sentinel
				}
				return nil
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installSuccessfulApplyConfigSeams(t)
			tt.inject()
			err := applyConfig(cfg)
			if !errors.Is(err, sentinel) {
				t.Fatalf("applyConfig err = %v, want %v", err, sentinel)
			}
			if tt.name == "reconcile" && !strings.Contains(err.Error(), "1 of 2 containers failed") {
				t.Fatalf("reconcile err = %v, want aggregate count", err)
			}
		})
	}
}
