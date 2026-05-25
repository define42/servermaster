package main

import (
	"context"
	"fmt"
	"net"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
)

const (
	// systemdJobTimeout bounds the wait for a systemd StartUnit job to finish.
	// StartUnitContext returns once the job is enqueued; its result arrives later
	// on a channel, so without this bound a unit that hangs in activation would
	// block the reconcile — which holds applyMu — and every later
	// /servermaster/config upload, forever.
	systemdJobTimeout = 60 * time.Second
)

type systemdUnitStarter interface {
	StartUnitContext(ctx context.Context, name string, mode string, ch chan<- string) (int, error)
	Close()
}

//nolint:gochecknoglobals // injectable seam so systemd unit starts can be tested with fakes.
var newSystemdConnectionContextFunc = func(ctx context.Context) (systemdUnitStarter, error) {
	return systemd.NewSystemConnectionContext(ctx)
}

func startPodmanSocket() error {
	ctx, cancel := context.WithTimeout(context.Background(), systemdJobTimeout)
	defer cancel()

	conn, err := newSystemdConnectionContextFunc(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd failed: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)

	if _, err := conn.StartUnitContext(ctx, "podman.socket", "replace", ch); err != nil {
		return fmt.Errorf("start podman.socket failed: %w", err)
	}

	// StartUnitContext returns once the job is enqueued; the completion result
	// arrives on ch. Bound the wait so a job that hangs in activation cannot
	// block the reconcile (and applyMu) forever.
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("podman.socket start result: %s", result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("start podman.socket timed out: %w", ctx.Err())
	}
}

// ensureFirewalldRunning starts firewalld.service through systemd so its D-Bus
// name is owned before ports are configured; on a host where firewalld is merely
// stopped this makes the apply self-healing instead of failing with "name is not
// activatable". firewalld.service is Type=dbus, so a "done" job result means the
// bus name has been acquired, and starting an already-active unit is a no-op. An
// error means firewalld is absent, masked, or failed to start.
func ensureFirewalldRunning() error {
	ctx, cancel := context.WithTimeout(context.Background(), systemdJobTimeout)
	defer cancel()

	conn, err := newSystemdConnectionContextFunc(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd failed: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)

	if _, err := conn.StartUnitContext(ctx, "firewalld.service", "replace", ch); err != nil {
		return fmt.Errorf("start firewalld.service failed: %w", err)
	}

	// StartUnitContext returns once the job is enqueued; the completion result
	// arrives on ch. Bound the wait so a job that hangs in activation cannot
	// block the reconcile (and applyMu) forever.
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("firewalld.service start result: %s", result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("start firewalld.service timed out: %w", ctx.Err())
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
