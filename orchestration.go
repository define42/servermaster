package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
)

// applyMu serializes host convergence so the startup reconcile and concurrent
// /edgecommander/config uploads cannot interleave changes to folders, interfaces,
// firewall, or containers. Callers hold it across the whole apply.
//
//nolint:gochecknoglobals // process-wide lock guarding the single host apply.
var applyMu sync.Mutex

// configApplier converges the host to a parsed config. It is a package variable
// so tests can substitute the host-mutating apply; production uses applyConfig.
//
//nolint:gochecknoglobals // injectable seam so handlers can be tested without mutating the host.
var configApplier = applyConfig

// runServiceFunc and startWebServerFunc are the top-level service seams. They
// let tests exercise command-line and run-loop behavior without binding long
// lived listeners or mutating the host.
//
//nolint:gochecknoglobals // injectable seams for service orchestration tests.
var (
	runServiceFunc     = runService
	startWebServerFunc = startWebServer
)

// apply step seams let tests cover the apply orchestration without touching the
// host. Production keeps the concrete host-mutating functions.
//
//nolint:gochecknoglobals // injectable seams for applyConfig orchestration tests.
var (
	validateConfigFunc          = validateConfig
	ensureHostnameFunc          = ensureHostname
	ensureFoldersFunc           = ensureFolders
	ensureFilesFunc             = ensureFiles
	configureHostInterfacesFunc = configureHostInterfaces
	configureFirewallPortsFunc  = configureFirewallPorts
	startPodmanSocketFunc       = startPodmanSocket
	waitForUnixSocketFunc       = waitForUnixSocket
	newPodmanConnectionFunc     = bindings.NewConnection
	stopUnmanagedContainersFunc = stopUnmanagedContainers
	reconcileContainerFunc      = reconcileContainer
)

func runService(listenAddress, configPath string) error {
	_, webServerErrors, err := startWebServerFunc(listenAddress, configPath)
	if err != nil {
		return err
	}

	// A reconcile failure is logged but does not exit the process: the unit
	// is configured with Restart=always/RestartSec=10s, so returning here
	// would tear down and recreate every container on a tight crash loop.
	// The web server stays up so the host remains observable. applyMu is held
	// so an early /edgecommander/config upload cannot race the startup convergence.
	applyMu.Lock()
	runErr := run(configPath)
	applyMu.Unlock()
	if runErr != nil {
		log.Printf("reconcile failed: %v", runErr)
	}

	if err := <-webServerErrors; err != nil {
		return err
	}

	return nil
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	return configApplier(cfg)
}

// applyConfig validates the desired node configuration and converges the host
// to it: host folders, host interfaces, firewall ports, the Podman socket, and
// the declared containers. Callers must hold applyMu so two convergence runs
// (the startup reconcile and a concurrent /edgecommander/config upload) cannot
// interleave host changes.
func applyConfig(cfg *Config) error {
	if err := validateConfigFunc(cfg); err != nil {
		return err
	}

	if err := ensureHostnameFunc(cfg.Hostname); err != nil {
		return err
	}

	if err := ensureFoldersFunc(cfg.Folders); err != nil {
		return err
	}

	if err := ensureFilesFunc(cfg.Files); err != nil {
		return err
	}

	if err := configureHostInterfacesFunc(cfg.Interfaces, cfg.Routes); err != nil {
		return err
	}

	if err := configureFirewallPortsFunc(cfg.FirewallPorts); err != nil {
		return err
	}

	if err := startPodmanSocketFunc(); err != nil {
		return err
	}

	if err := waitForUnixSocketFunc(podmanSocketPath, 10*time.Second); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ctx, err := newPodmanConnectionFunc(ctx, "unix:"+podmanSocketPath)
	if err != nil {
		return err
	}

	if err := stopUnmanagedContainersFunc(ctx, cfg.Containers); err != nil {
		return err
	}

	var reconcileErrors []error
	for _, c := range cfg.Containers {
		if err := reconcileContainerFunc(ctx, c); err != nil {
			log.Printf("reconcile container %q failed: %v", c.Name, err)
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	if len(reconcileErrors) > 0 {
		return fmt.Errorf("%d of %d containers failed to reconcile: %w", len(reconcileErrors), len(cfg.Containers), errors.Join(reconcileErrors...))
	}

	log.Println("all containers started")
	return nil
}
