package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeSystemdConn struct {
	result string
	err    error

	unit   string
	mode   string
	closed bool
}

func (f *fakeSystemdConn) StartUnitContext(_ context.Context, name string, mode string, ch chan<- string) (int, error) {
	f.unit = name
	f.mode = mode
	if f.err != nil {
		return 0, f.err
	}
	if f.result == "" {
		f.result = "done"
	}
	ch <- f.result
	return 1, nil
}

func (f *fakeSystemdConn) Close() {
	f.closed = true
}

func stubSystemdConnection(t *testing.T, conn systemdUnitStarter, err error) {
	t.Helper()
	prev := newSystemdConnectionContextFunc
	t.Cleanup(func() { newSystemdConnectionContextFunc = prev })
	newSystemdConnectionContextFunc = func(ctx context.Context) (systemdUnitStarter, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("systemd connection context has no deadline")
		}
		return conn, err
	}
}

func resetScheduleRebootSeams(t *testing.T) {
	t.Helper()
	prevDelay := rebootDelay
	prevCommand := rebootCommandFunc
	t.Cleanup(func() {
		rebootDelay = prevDelay
		rebootCommandFunc = prevCommand
	})
	rebootDelay = 0
}

func TestScheduleRebootRunsSystemctlReboot(t *testing.T) {
	resetScheduleRebootSeams(t)

	var gotName string
	var gotArgs []string
	rebootCommandFunc = func(ctx context.Context, name string, args ...string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("reboot command context has no deadline")
		}
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}

	scheduleReboot()

	if gotName != "systemctl" || !reflect.DeepEqual(gotArgs, []string{"reboot"}) {
		t.Fatalf("reboot command = %s %v, want systemctl [reboot]", gotName, gotArgs)
	}
}

func TestScheduleRebootLogsCommandError(t *testing.T) {
	resetScheduleRebootSeams(t)
	prevLogOutput := log.Writer()
	t.Cleanup(func() { log.SetOutput(prevLogOutput) })

	var logs bytes.Buffer
	log.SetOutput(&logs)
	rebootCommandFunc = func(context.Context, string, ...string) error {
		return errors.New("systemctl failed")
	}

	scheduleReboot()

	if !strings.Contains(logs.String(), "reboot failed: systemctl failed") {
		t.Fatalf("logs = %q, want reboot failure", logs.String())
	}
}

func TestStartPodmanSocketStartsUnit(t *testing.T) {
	conn := &fakeSystemdConn{}
	stubSystemdConnection(t, conn, nil)

	if err := startPodmanSocket(); err != nil {
		t.Fatalf("startPodmanSocket: %v", err)
	}
	if conn.unit != "podman.socket" || conn.mode != "replace" || !conn.closed {
		t.Fatalf("conn = %+v, want podman.socket replace and closed", conn)
	}
}

func TestStartPodmanSocketErrors(t *testing.T) {
	tests := []struct {
		name string
		conn *fakeSystemdConn
		err  error
		want string
	}{
		{name: "connect", err: errors.New("no systemd"), want: "connect to systemd failed"},
		{name: "start", conn: &fakeSystemdConn{err: errors.New("start denied")}, want: "start podman.socket failed"},
		{name: "result", conn: &fakeSystemdConn{result: "failed"}, want: "podman.socket start result: failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubSystemdConnection(t, tt.conn, tt.err)

			err := startPodmanSocket()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("startPodmanSocket err = %v, want %q", err, tt.want)
			}
			if tt.conn != nil && !tt.conn.closed {
				t.Fatal("expected systemd connection to be closed")
			}
		})
	}
}

func TestEnsureFirewalldRunningStartsUnit(t *testing.T) {
	conn := &fakeSystemdConn{}
	stubSystemdConnection(t, conn, nil)

	if err := ensureFirewalldRunning(); err != nil {
		t.Fatalf("ensureFirewalldRunning: %v", err)
	}
	if conn.unit != "firewalld.service" || conn.mode != "replace" || !conn.closed {
		t.Fatalf("conn = %+v, want firewalld.service replace and closed", conn)
	}
}

func TestEnsureFirewalldRunningErrors(t *testing.T) {
	tests := []struct {
		name string
		conn *fakeSystemdConn
		err  error
		want string
	}{
		{name: "connect", err: errors.New("no systemd"), want: "connect to systemd failed"},
		{name: "start", conn: &fakeSystemdConn{err: errors.New("start denied")}, want: "start firewalld.service failed"},
		{name: "result", conn: &fakeSystemdConn{result: "failed"}, want: "firewalld.service start result: failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubSystemdConnection(t, tt.conn, tt.err)

			err := ensureFirewalldRunning()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ensureFirewalldRunning err = %v, want %q", err, tt.want)
			}
			if tt.conn != nil && !tt.conn.closed {
				t.Fatal("expected systemd connection to be closed")
			}
		})
	}
}

func TestSystemdUnitStartTimeoutsRemainBounded(t *testing.T) {
	deadlineConn := &fakeSystemdConn{}
	var timeout time.Duration
	prev := newSystemdConnectionContextFunc
	t.Cleanup(func() { newSystemdConnectionContextFunc = prev })
	newSystemdConnectionContextFunc = func(ctx context.Context) (systemdUnitStarter, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("systemd connection context has no deadline")
		}
		timeout = time.Until(deadline)
		return deadlineConn, nil
	}

	if err := startPodmanSocket(); err != nil {
		t.Fatalf("startPodmanSocket: %v", err)
	}
	if timeout <= 0 || timeout > systemdJobTimeout {
		t.Fatalf("timeout = %s, want within %s", timeout, systemdJobTimeout)
	}
}

func TestWaitForUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	if err := waitForUnixSocket(sock, time.Second); err != nil {
		t.Fatalf("reachable socket: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.sock")
	if err := waitForUnixSocket(missing, 150*time.Millisecond); err == nil {
		t.Fatal("expected timeout error for absent socket")
	}
}
