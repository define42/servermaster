package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
