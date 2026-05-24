package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// --- ostree status parsers -------------------------------------------------

func mustParseRPM(t *testing.T, raw string) ostreeStatus {
	t.Helper()
	st, err := parseRPMOstreeStatus([]byte(raw))
	if err != nil {
		t.Fatalf("parseRPMOstreeStatus(%s): %v", raw, err)
	}
	return st
}

func mustParseBootc(t *testing.T, raw string) ostreeStatus {
	t.Helper()
	st, err := parseBootcStatus([]byte(raw))
	if err != nil {
		t.Fatalf("parseBootcStatus(%s): %v", raw, err)
	}
	return st
}

func TestParseRPMOstreeStatusBranches(t *testing.T) {
	t.Run("version from base-commit-meta and booted selection", func(t *testing.T) {
		st := mustParseRPM(t, `{"deployments":[
			{"booted":false,"checksum":"aaa"},
			{"booted":true,"base-commit-meta":{"version":"9.20260524"},"checksum":"bbb","container-image-reference":"ostree-unverified:quay.io/x@sha256:dead"}
		]}`)
		if st.Version != "9.20260524" || st.Checksum != "bbb" || !st.Booted {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("version falls back to image reference then checksum", func(t *testing.T) {
		st := mustParseRPM(t, `{"deployments":[{"booted":true,"base-commit":"cafe","container-image-reference":"docker://quay.io/x:v7"}]}`)
		if st.Version != "v7" || st.Checksum != "cafe" {
			t.Fatalf("status = %+v, want version v7 checksum cafe", st)
		}
	})

	t.Run("version is checksum when nothing else", func(t *testing.T) {
		if st := mustParseRPM(t, `{"deployments":[{"booted":true,"checksum":"only"}]}`); st.Version != "only" {
			t.Fatalf("version = %q, want only", st.Version)
		}
	})

	t.Run("no deployments and bad json error", func(t *testing.T) {
		if _, err := parseRPMOstreeStatus([]byte(`{"deployments":[]}`)); err == nil {
			t.Fatal("expected error for no deployments")
		}
		if _, err := parseRPMOstreeStatus([]byte(`not json`)); err == nil {
			t.Fatal("expected error for bad json")
		}
	})
}

func TestParseBootcStatus(t *testing.T) {
	t.Run("nested booted image", func(t *testing.T) {
		st := mustParseBootc(t, `{"status":{"booted":{"image":{"version":"42","image_digest":"sha256:dd","image":"quay.io/x:42"}}}}`)
		if st.Version != "42" || st.Checksum != "sha256:dd" || st.Image != "quay.io/x:42" || !st.Booted {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("flat fields with version from image reference", func(t *testing.T) {
		st := mustParseBootc(t, `{"image":{"reference":"quay.io/x:v9"}}`)
		if st.Image != "quay.io/x:v9" || st.Version != "v9" {
			t.Fatalf("status = %+v, want image+version v9", st)
		}
	})

	t.Run("version falls back to checksum", func(t *testing.T) {
		if st := mustParseBootc(t, `{"checksum":"sum-only"}`); st.Version != "sum-only" {
			t.Fatalf("version = %q, want sum-only", st.Version)
		}
	})

	t.Run("no fields and bad json error", func(t *testing.T) {
		if _, err := parseBootcStatus([]byte(`{"status":{"booted":{}}}`)); err == nil {
			t.Fatal("expected error for empty booted")
		}
		if _, err := parseBootcStatus([]byte(`nope`)); err == nil {
			t.Fatal("expected error for bad json")
		}
	})
}

func TestParseOstreeAdminStatus(t *testing.T) {
	booted := parseOstreeAdminStatus([]byte("  some-stream 1\n* fedora-iot abc123.0\n  other 2\n"))
	if !booted.Booted || booted.Deployment != "fedora-iot abc123.0" {
		t.Fatalf("booted parse = %+v", booted)
	}
	none := parseOstreeAdminStatus([]byte("  fedora-iot abc123.0\n"))
	if none.Booted || none.Deployment != "" {
		t.Fatalf("unbooted parse = %+v", none)
	}
}

func TestCollectOstreeStatusAllFail(t *testing.T) {
	t.Setenv("PATH", "") // rpm-ostree, bootc, ostree all unresolvable
	if _, err := collectOstreeStatus(context.Background()); err == nil {
		t.Fatal("expected error when no ostree tooling is available")
	}
}

// --- disk ------------------------------------------------------------------

func TestReadDiskMounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mounts")
	content := strings.Join([]string{
		"rootfs / rootfs rw 0 0",                   // not a /dev source -> skipped
		"/dev/vda1 / ext4 rw,relatime 0 0",         // kept
		"tmpfs /run tmpfs rw 0 0",                  // tmpfs -> skipped
		"proc /proc proc rw 0 0",                   // virtual -> skipped
		`/dev/vdc1 /mnt/with\040space ext4 rw 0 0`, // kept, with an escaped space
		"too short", // <3 fields -> skipped
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}

	mounts, err := readDiskMounts(path)
	if err != nil {
		t.Fatalf("readDiskMounts: %v", err)
	}
	if want := []string{"/", "/mnt/with space"}; !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts = %v, want %v", mounts, want)
	}

	if _, err := readDiskMounts(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error reading a missing mounts file")
	}
}

func TestCollectDiskStatuses(t *testing.T) {
	prev := procMountsPath
	defer func() { procMountsPath = prev }()

	path := filepath.Join(t.TempDir(), "mounts")
	content := strings.Join([]string{
		"/dev/vda1 / ext4 rw 0 0",
		"/dev/vda1 / ext4 rw 0 0",                  // same device -> de-duplicated
		"tmpfs /run tmpfs rw 0 0",                  // excluded
		"/dev/vdb1 /no/such/mount/xyz ext4 rw 0 0", // kept by filter but stat fails
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	procMountsPath = path

	statuses, errs := collectDiskStatuses()
	if len(statuses) != 1 || statuses[0].Path != "/" {
		t.Fatalf("statuses = %+v, want a single entry for /", statuses)
	}
	if statuses[0].TotalBytes == 0 {
		t.Fatal("root filesystem reported zero total bytes")
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one (the nonexistent mount)", errs)
	}
}

func TestCollectDiskStatusesReadError(t *testing.T) {
	prev := procMountsPath
	defer func() { procMountsPath = prev }()
	procMountsPath = filepath.Join(t.TempDir(), "missing")

	statuses, errs := collectDiskStatuses()
	if statuses != nil || len(errs) != 1 {
		t.Fatalf("want nil statuses and one error, got %v / %v", statuses, errs)
	}
}

func TestDiskStatusForPathError(t *testing.T) {
	if _, err := diskStatusForPath(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected statfs error for missing path")
	}
}

// --- folders / ownership ---------------------------------------------------

func TestEnsureFoldersAppliesModeAndOwner(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b")
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	if err := ensureFolders([]FolderConfig{{Path: target, Chmod: "0700", User: owner}}); err != nil {
		t.Fatalf("ensureFolders: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestEnsureFolderErrors(t *testing.T) {
	cases := []struct {
		name   string
		folder FolderConfig
	}{
		{"missing path", FolderConfig{Chmod: "0755"}},
		{"bad chmod", FolderConfig{Path: filepath.Join(t.TempDir(), "x"), Chmod: "99999"}},
		{"bad user", FolderConfig{Path: filepath.Join(t.TempDir(), "y"), User: "no-such-user-xyz"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := ensureFolders([]FolderConfig{tt.folder}); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestParseOwnerVariants(t *testing.T) {
	if uid, gid, err := parseOwner("0:0"); err != nil || uid != 0 || gid != 0 {
		t.Fatalf("parseOwner(0:0) = %d,%d,%v", uid, gid, err)
	}
	if uid, _, err := parseOwner("root"); err != nil || uid != 0 {
		t.Fatalf("parseOwner(root) = %d,%v", uid, err)
	}
	if _, gid, err := parseOwner("0:root"); err != nil || gid != 0 {
		t.Fatalf("parseOwner(0:root) = %d,%v", gid, err)
	}
	for _, bad := range []string{"", ":0", "0:", "nobody-xyz", "0:nogroup-xyz"} {
		if _, _, err := parseOwner(bad); err == nil {
			t.Fatalf("parseOwner(%q) expected error", bad)
		}
	}
}

// --- runCommand / runCommandOutput -----------------------------------------

func TestRunCommand(t *testing.T) {
	if err := runCommand("true"); err != nil {
		t.Fatalf("runCommand(true): %v", err)
	}
	if err := runCommand("sh", "-c", "exit 1"); err == nil {
		t.Fatal("expected error from exit 1")
	}
	err := runCommand("sh", "-c", "echo problem; exit 1")
	if err == nil || !strings.Contains(err.Error(), "problem") {
		t.Fatalf("err = %v, want output included", err)
	}
}

func TestRunCommandOutput(t *testing.T) {
	out, err := runCommandOutput(context.Background(), "sh", "-c", "echo hi")
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	if _, err := runCommandOutput(context.Background(), "sh", "-c", "echo boom >&2; exit 2"); err == nil {
		t.Fatal("expected error from exit 2")
	}
	if _, err := runCommandOutput(context.Background(), "sh", "-c", "exit 3"); err == nil {
		t.Fatal("expected error from exit 3")
	}
}

func TestRunCommandOutputContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := runCommandOutput(ctx, "sh", "-c", "sleep 2"); err == nil {
		t.Fatal("expected error when context deadline exceeded")
	}
}

// --- network helpers -------------------------------------------------------

func TestRouteFamily(t *testing.T) {
	cases := []struct {
		name  string
		route netlink.Route
		want  string
	}{
		{"explicit v4", netlink.Route{Family: netlink.FAMILY_V4}, "ipv4"},
		{"explicit v6", netlink.Route{Family: netlink.FAMILY_V6}, "ipv6"},
		{"infer from dst", netlink.Route{Dst: cidr("2001:db8::/64")}, "ipv6"},
		{"infer from gw", netlink.Route{Gw: net.ParseIP("192.168.0.1")}, "ipv4"},
		{"default", netlink.Route{}, "ipv4"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeFamily(tt.route); got != tt.want {
				t.Fatalf("routeFamily = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterfaceFlags(t *testing.T) {
	if got := interfaceFlags(0); got != nil {
		t.Fatalf("flags(0) = %v, want nil", got)
	}
	got := interfaceFlags(net.FlagUp | net.FlagBroadcast)
	if len(got) != 2 || got[0] != "up" {
		t.Fatalf("flags = %v, want [up broadcast]", got)
	}
}

// --- status orchestration --------------------------------------------------

func TestCollectServermasterStatus(t *testing.T) {
	links, addrs := testNetworkLinks()
	defer stubNetlink(links, addrs, nil, nil)()

	f := &fakePodman{
		list:    []listedContainer{{ID: "abc", Names: []string{"web"}, State: "running"}},
		inspect: map[string]containerInspectResponse{"abc": {ID: "abc", Name: "/web"}},
	}
	f.start(t)

	cfgPath := writeTempConfig(t, `{"podman_mode":"rootful"}`)
	t.Setenv("PATH", "") // ostree tooling unavailable -> recorded as an error

	status := collectServermasterStatus(context.Background(), cfgPath)
	if status.Status != "degraded" {
		t.Fatalf("status = %q, want degraded (ostree unavailable)", status.Status)
	}
	if len(status.Containers) != 1 || status.Containers[0].Name != "web" {
		t.Fatalf("containers = %+v", status.Containers)
	}
	if status.Network.Source != "netlink" {
		t.Fatalf("network source = %q, want netlink", status.Network.Source)
	}
}
