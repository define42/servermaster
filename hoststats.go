package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procMountsPath is the kernel's mounted-filesystem table. It is a variable so
// tests can supply a fixture instead of the live /proc/mounts.
//
//nolint:gochecknoglobals // injectable seam so disk enumeration can be tested with a fixture.
var procMountsPath = "/proc/mounts"

// procMeminfoPath, procStatPath, and procUptimePath are the kernel's memory,
// CPU, and uptime statistics files. They are variables so tests can supply
// fixtures instead of live /proc.
//
//nolint:gochecknoglobals // injectable seams so memory/CPU/uptime collection can be tested with fixtures.
var (
	procMeminfoPath = "/proc/meminfo"
	procStatPath    = "/proc/stat"
	procUptimePath  = "/proc/uptime"
)

// memoryStatus is the host's RAM usage, read from /proc/meminfo. AvailableBytes
// is the kernel's estimate of memory obtainable without swapping (the meaningful
// "free" figure); UsedBytes/UsedPercent are derived from it.
type memoryStatus struct {
	TotalBytes     uint64  `json:"total_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	Error          string  `json:"error,omitempty"`
}

// cpuStatus is the host's CPU count and current utilization. UsedPercent is the
// busy fraction sampled over a short interval from /proc/stat.
type cpuStatus struct {
	Cores       int     `json:"cores"`
	UsedPercent float64 `json:"used_percent"`
	Error       string  `json:"error,omitempty"`
}

// uptimeStatus is how long the host has been running, read from /proc/uptime.
// Seconds is the machine-readable figure; Human is a "1d 2h 3m 4s" rendering of
// the same value for convenience.
type uptimeStatus struct {
	Seconds uint64 `json:"seconds"`
	Human   string `json:"human"`
	Error   string `json:"error,omitempty"`
}

type diskStatus struct {
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"total_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// collectDiskStatuses reports free space for each real, disk-backed filesystem
// mounted on the host. It reads the mount table, keeps only block-device mounts
// (which excludes tmpfs and the other virtual filesystems), and de-duplicates by
// the underlying device so a filesystem mounted at several paths — or directories
// that merely live on the same partition — are reported once.
func collectDiskStatuses() ([]diskStatus, []error) {
	mounts, err := readDiskMounts(procMountsPath)
	if err != nil {
		return nil, []error{err}
	}

	var statuses []diskStatus
	var errs []error
	seenDevice := make(map[uint64]struct{})

	for _, mountPoint := range mounts {
		var info syscall.Stat_t
		if err := syscall.Stat(mountPoint, &info); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mountPoint, err))
			continue
		}
		if _, ok := seenDevice[info.Dev]; ok {
			continue
		}
		seenDevice[info.Dev] = struct{}{}

		status, err := diskStatusForPath(mountPoint)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		statuses = append(statuses, status)
	}

	return statuses, errs
}

// readDiskMounts returns the mount points of real, disk-backed filesystems from
// a /proc/mounts-formatted file. Only mounts whose source is a /dev device are
// kept, which skips tmpfs, devtmpfs, proc, sysfs, cgroup, overlay, and the other
// virtual filesystems that are not physical disks.
func readDiskMounts(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed procMountsPath (overridable only by tests), not request input.
	if err != nil {
		return nil, fmt.Errorf("read mounts %q: %w", path, err)
	}

	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		mounts = append(mounts, unescapeMountPoint(fields[1]))
	}
	return mounts, nil
}

// unescapeMountPoint decodes the octal escapes /proc/mounts uses for a space,
// tab, newline, or backslash in a mount-point path.
func unescapeMountPoint(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(field)
}

func diskStatusForPath(path string) (diskStatus, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskStatus{}, fmt.Errorf("%s: %w", path, err)
	}

	blockSize := uint64(stat.Bsize) //nolint:gosec // Statfs block size is a kernel-reported positive value.
	total := stat.Blocks * blockSize
	free := stat.Bfree * blockSize
	available := stat.Bavail * blockSize
	used := uint64(0)
	if total > free {
		used = total - free
	}

	usedPercent := 0.0
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}

	return diskStatus{
		Path:           path,
		TotalBytes:     total,
		FreeBytes:      free,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    usedPercent,
	}, nil
}

// collectMemoryStatus reports host RAM usage from /proc/meminfo. UsedBytes is
// derived from MemAvailable (memory obtainable without swapping), falling back
// to MemFree on kernels too old to report MemAvailable.
func collectMemoryStatus() memoryStatus {
	values, err := readMeminfo(procMeminfoPath)
	if err != nil {
		return memoryStatus{Error: err.Error()}
	}

	total := values["MemTotal"]
	if total == 0 {
		return memoryStatus{Error: "meminfo reported no MemTotal"}
	}
	free := values["MemFree"]
	available, ok := values["MemAvailable"]
	if !ok {
		available = free
	}

	used := uint64(0)
	if total > available {
		used = total - available
	}

	return memoryStatus{
		TotalBytes:     total,
		FreeBytes:      free,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    float64(used) / float64(total) * 100,
	}
}

// readMeminfo parses a /proc/meminfo-formatted file into a map of field name to
// bytes (the file reports kibibytes, which are scaled up here).
func readMeminfo(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed procMeminfoPath (overridable only by tests), not request input.
	if err != nil {
		return nil, fmt.Errorf("read meminfo %q: %w", path, err)
	}

	values := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) >= 2 && fields[1] == "kB" {
			value *= 1024
		}
		values[key] = value
	}
	return values, nil
}

// collectUptimeStatus reports how long the host has been running. A read or
// parse failure is recorded in the Error field rather than failing the whole
// status response.
func collectUptimeStatus() uptimeStatus {
	seconds, err := readUptimeSeconds(procUptimePath)
	if err != nil {
		return uptimeStatus{Error: err.Error()}
	}
	return uptimeStatus{
		Seconds: seconds,
		Human:   formatUptime(seconds),
	}
}

// readUptimeSeconds parses the whole-seconds host uptime from a /proc/uptime-
// formatted file, whose first field is the uptime in seconds (the second field,
// idle time, is ignored).
func readUptimeSeconds(path string) (uint64, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed procUptimePath (overridable only by tests), not request input.
	if err != nil {
		return 0, fmt.Errorf("read uptime %q: %w", path, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("uptime %q was empty", path)
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime %q: %w", path, err)
	}
	if seconds < 0 {
		seconds = 0
	}
	return uint64(seconds), nil
}

// formatUptime renders a second count as "1d 2h 3m 4s", dropping leading units
// that are zero but always keeping seconds so the result is never empty.
func formatUptime(seconds uint64) string {
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", secs))
	return strings.Join(parts, " ")
}

// cpuSampleInterval is how long collectCPUStatus waits between /proc/stat reads
// so UsedPercent reflects current load rather than the average since boot.
const cpuSampleInterval = 100 * time.Millisecond

// cpuTimes holds the idle (idle + iowait) and total jiffies from /proc/stat.
type cpuTimes struct {
	idle  uint64
	total uint64
}

// collectCPUStatus reports the CPU count and current utilization, sampling
// /proc/stat across cpuSampleInterval.
func collectCPUStatus(ctx context.Context) cpuStatus {
	status := cpuStatus{Cores: runtime.NumCPU()}

	before, err := readCPUTimes(procStatPath)
	if err != nil {
		status.Error = err.Error()
		return status
	}

	select {
	case <-ctx.Done():
	case <-time.After(cpuSampleInterval):
	}

	after, err := readCPUTimes(procStatPath)
	if err != nil {
		status.Error = err.Error()
		return status
	}

	status.UsedPercent = cpuUsedPercent(before, after)
	return status
}

// readCPUTimes parses the aggregate "cpu" line of a /proc/stat-formatted file.
// Idle is the idle plus iowait columns; total is the sum of every column.
func readCPUTimes(path string) (cpuTimes, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed procStatPath (overridable only by tests), not request input.
	if err != nil {
		return cpuTimes{}, fmt.Errorf("read cpu stat %q: %w", path, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var times cpuTimes
		for i, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse cpu stat field %q: %w", field, err)
			}
			times.total += value
			if i == 3 || i == 4 { // the idle and iowait columns
				times.idle += value
			}
		}
		return times, nil
	}
	return cpuTimes{}, fmt.Errorf("no aggregate cpu line in %q", path)
}

// cpuUsedPercent is the busy fraction between two /proc/stat samples.
func cpuUsedPercent(before, after cpuTimes) float64 {
	totalDelta := after.total - before.total
	idleDelta := after.idle - before.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}
