package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// edgecommanderStatusCollector gathers the /edgecommander/status response. Tests
// replace it so the handler can be exercised without requiring Podman or ostree.
//
//nolint:gochecknoglobals // injectable seam so the handler can be tested without Podman or ostree.
var edgecommanderStatusCollector = collectEdgecommanderStatus

type edgecommanderStatus struct {
	Status          string                   `json:"status"`
	GeneratedAt     string                   `json:"generated_at"`
	Hostname        string                   `json:"hostname,omitempty"`
	Uptime          uptimeStatus             `json:"uptime"`
	Ostree          ostreeStatus             `json:"ostree"`
	FreeDiskSpace   []diskStatus             `json:"free_diskspace"`
	Memory          memoryStatus             `json:"memory"`
	CPU             cpuStatus                `json:"cpu"`
	Inventory       *Inventory               `json:"inventory,omitempty"`
	Network         networkStatus            `json:"network"`
	Containers      []runningContainerStatus `json:"containers"`
	EdgecommanderLog []string                 `json:"edgecommander_log"`
	Errors          []string                 `json:"errors,omitempty"`
}

// recordError appends "prefix: msg" to the status errors when msg is non-empty,
// the shared shape for collectors that report failures through an Error field.
func (s *edgecommanderStatus) recordError(prefix, msg string) {
	if msg != "" {
		s.Errors = append(s.Errors, fmt.Sprintf("%s: %s", prefix, msg))
	}
}

func collectEdgecommanderStatus(ctx context.Context, configPath string) edgecommanderStatus {
	status := edgecommanderStatus{
		Status:      "ok",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	collectHostnameInto(&status)

	if _, err := loadConfig(configPath); err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("load config: %v", err))
	}

	ostree, err := collectOstreeStatus(ctx)
	if err != nil {
		ostree.Error = err.Error()
		status.Errors = append(status.Errors, fmt.Sprintf("ostree: %v", err))
	}
	status.Ostree = ostree

	disks, diskErrors := collectDiskStatuses()
	status.FreeDiskSpace = disks
	for _, err := range diskErrors {
		status.Errors = append(status.Errors, fmt.Sprintf("disk: %v", err))
	}

	status.Memory = collectMemoryStatus()
	status.recordError("memory", status.Memory.Error)

	status.CPU = collectCPUStatus(ctx)
	status.recordError("cpu", status.CPU.Error)

	status.Uptime = collectUptimeStatus()
	status.recordError("uptime", status.Uptime.Error)

	collectInventoryInto(&status)

	status.Network = collectNetworkStatus(ctx)
	status.recordError("network", status.Network.Error)

	containers, err := collectRunningContainerStatuses(ctx, edgecommanderLogTail)
	if err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("containers: %v", err))
	}
	status.Containers = containers
	status.Errors = append(status.Errors, containerStatusErrors(containers)...)

	// Captured after the other collectors so their log output is reflected.
	status.EdgecommanderLog = serviceLog.snapshot()

	if len(status.Errors) > 0 {
		status.Status = "degraded"
	}

	return status
}

// collectHostnameInto records the host's current hostname, or the failure to
// read it, into status.
func collectHostnameInto(status *edgecommanderStatus) {
	if hostname, err := os.Hostname(); err == nil {
		status.Hostname = hostname
	} else {
		status.Errors = append(status.Errors, fmt.Sprintf("hostname: %v", err))
	}
}

// collectInventoryInto decodes the hardware inventory into status. DMI is
// root-only and absent on some hosts, so a failure is recorded as a status error
// and the inventory is left unset.
func collectInventoryInto(status *edgecommanderStatus) {
	inventory, err := collectInventory()
	if err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("inventory: %v", err))
		return
	}
	status.Inventory = inventory
}

// containerStatusErrors returns one "container <name>: <error>" message for each
// container that reported an error during status collection.
func containerStatusErrors(containers []runningContainerStatus) []string {
	var msgs []string
	for _, container := range containers {
		if container.Error != "" {
			msgs = append(msgs, fmt.Sprintf("container %s: %s", container.Name, container.Error))
		}
	}
	return msgs
}
