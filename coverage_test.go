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

// --- memory ----------------------------------------------------------------

func TestReadMeminfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	content := "MemTotal:       2048 kB\nMemFree:         512 kB\nHugePages_Total:       0\nweird line\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}

	values, err := readMeminfo(path)
	if err != nil {
		t.Fatalf("readMeminfo: %v", err)
	}
	if values["MemTotal"] != 2048*1024 || values["MemFree"] != 512*1024 {
		t.Fatalf("scaled values = %v", values)
	}
	if values["HugePages_Total"] != 0 { // no kB suffix -> not scaled
		t.Fatalf("HugePages_Total = %d, want 0", values["HugePages_Total"])
	}
	if _, err := readMeminfo(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error reading a missing meminfo file")
	}
}

func TestCollectMemoryStatus(t *testing.T) {
	prev := procMeminfoPath
	defer func() { procMeminfoPath = prev }()

	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemTotal: 1000 kB\nMemFree: 200 kB\nMemAvailable: 400 kB\n"), 0o600); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	procMeminfoPath = path

	m := collectMemoryStatus()
	if m.Error != "" {
		t.Fatalf("unexpected error: %s", m.Error)
	}
	if m.TotalBytes != 1000*1024 || m.FreeBytes != 200*1024 || m.AvailableBytes != 400*1024 {
		t.Fatalf("memory = %+v", m)
	}
	if m.UsedBytes != 600*1024 || m.UsedPercent != 60 {
		t.Fatalf("used = %d (%v%%), want 600 kB (60%%)", m.UsedBytes, m.UsedPercent)
	}

	// Without MemAvailable, used is derived from MemFree.
	noAvail := filepath.Join(t.TempDir(), "meminfo2")
	if err := os.WriteFile(noAvail, []byte("MemTotal: 1000 kB\nMemFree: 300 kB\n"), 0o600); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	procMeminfoPath = noAvail
	if m := collectMemoryStatus(); m.AvailableBytes != 300*1024 || m.UsedBytes != 700*1024 {
		t.Fatalf("fallback memory = %+v", m)
	}
}

func TestCollectMemoryStatusErrors(t *testing.T) {
	prev := procMeminfoPath
	defer func() { procMeminfoPath = prev }()

	procMeminfoPath = filepath.Join(t.TempDir(), "missing")
	if m := collectMemoryStatus(); m.Error == "" {
		t.Fatal("expected error when meminfo is unreadable")
	}

	noTotal := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(noTotal, []byte("MemFree: 100 kB\n"), 0o600); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	procMeminfoPath = noTotal
	if m := collectMemoryStatus(); m.Error == "" {
		t.Fatal("expected error when MemTotal is absent")
	}
}

// --- uptime ----------------------------------------------------------------

func TestReadUptimeSeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(path, []byte("350735.47 234388.90\n"), 0o600); err != nil {
		t.Fatalf("write uptime: %v", err)
	}
	seconds, err := readUptimeSeconds(path)
	if err != nil {
		t.Fatalf("readUptimeSeconds: %v", err)
	}
	if seconds != 350735 { // truncated to whole seconds, idle field ignored
		t.Fatalf("seconds = %d, want 350735", seconds)
	}

	if _, err := readUptimeSeconds(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error reading a missing uptime file")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write empty uptime: %v", err)
	}
	if _, err := readUptimeSeconds(empty); err == nil {
		t.Fatal("expected error for empty uptime file")
	}

	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("notanumber 1.0\n"), 0o600); err != nil {
		t.Fatalf("write bad uptime: %v", err)
	}
	if _, err := readUptimeSeconds(bad); err == nil {
		t.Fatal("expected error for unparseable uptime")
	}
}

func TestFormatUptime(t *testing.T) {
	cases := map[uint64]string{
		0:                           "0s",
		45:                          "45s",
		90:                          "1m 30s",
		3661:                        "1h 1m 1s",
		90061:                       "1d 1h 1m 1s",
		2*86400 + 3*3600 + 4*60 + 5: "2d 3h 4m 5s",
	}
	for seconds, want := range cases {
		if got := formatUptime(seconds); got != want {
			t.Errorf("formatUptime(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestCollectUptimeStatus(t *testing.T) {
	prev := procUptimePath
	defer func() { procUptimePath = prev }()

	path := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(path, []byte("3661.12 100.0\n"), 0o600); err != nil {
		t.Fatalf("write uptime: %v", err)
	}
	procUptimePath = path
	u := collectUptimeStatus()
	if u.Error != "" {
		t.Fatalf("unexpected error: %s", u.Error)
	}
	if u.Seconds != 3661 || u.Human != "1h 1m 1s" {
		t.Fatalf("uptime = %+v", u)
	}

	procUptimePath = filepath.Join(t.TempDir(), "missing")
	if u := collectUptimeStatus(); u.Error == "" {
		t.Fatal("expected error when uptime is unreadable")
	}
}

// --- cpu -------------------------------------------------------------------

func TestReadCPUTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	// total = 100+20+30+800+50 = 1000; idle = idle(800) + iowait(50) = 850
	content := "cpu  100 20 30 800 50 0 0 0 0 0\ncpu0 50 10 15 400 25 0 0\nintr 12345\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write stat: %v", err)
	}

	ct, err := readCPUTimes(path)
	if err != nil {
		t.Fatalf("readCPUTimes: %v", err)
	}
	if ct.total != 1000 || ct.idle != 850 {
		t.Fatalf("cpuTimes = %+v, want {idle:850 total:1000}", ct)
	}

	if _, err := readCPUTimes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error reading a missing stat file")
	}

	noAgg := filepath.Join(t.TempDir(), "stat-noagg")
	if err := os.WriteFile(noAgg, []byte("intr 1 2 3\nctxt 99\n"), 0o600); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if _, err := readCPUTimes(noAgg); err == nil {
		t.Fatal("expected error when there is no aggregate cpu line")
	}

	badField := filepath.Join(t.TempDir(), "stat-bad")
	if err := os.WriteFile(badField, []byte("cpu  100 20 30 xx 50\n"), 0o600); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if _, err := readCPUTimes(badField); err == nil {
		t.Fatal("expected error for a non-numeric cpu field")
	}
}

func TestCPUUsedPercent(t *testing.T) {
	// totalDelta = 100, idleDelta = 50 -> busy 50 -> 50%
	if got := cpuUsedPercent(cpuTimes{idle: 850, total: 1000}, cpuTimes{idle: 900, total: 1100}); got != 50 {
		t.Fatalf("usedPercent = %v, want 50", got)
	}
	if got := cpuUsedPercent(cpuTimes{total: 1000}, cpuTimes{total: 1000}); got != 0 {
		t.Fatalf("no delta usedPercent = %v, want 0", got)
	}
	if got := cpuUsedPercent(cpuTimes{idle: 0, total: 100}, cpuTimes{idle: 200, total: 150}); got != 0 {
		t.Fatalf("idle>total guard usedPercent = %v, want 0", got)
	}
}

func TestCollectCPUStatus(t *testing.T) {
	prev := procStatPath
	defer func() { procStatPath = prev }()

	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 50 0 25 400 25\n"), 0o600); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	procStatPath = path

	// A cancelled context skips the sample sleep, keeping the test fast; with a
	// static fixture the two reads match, so utilization is 0.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := collectCPUStatus(ctx)
	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}
	if st.Cores < 1 {
		t.Fatalf("cores = %d, want >= 1", st.Cores)
	}
	if st.UsedPercent != 0 {
		t.Fatalf("usedPercent = %v, want 0 for a static sample", st.UsedPercent)
	}
}

func TestCollectCPUStatusReadError(t *testing.T) {
	prev := procStatPath
	defer func() { procStatPath = prev }()
	procStatPath = filepath.Join(t.TempDir(), "missing")
	if st := collectCPUStatus(context.Background()); st.Error == "" {
		t.Fatal("expected error when /proc/stat is unreadable")
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
