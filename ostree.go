package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultOstreeUploadPath = "/data/ostree/update.tar"
	statusCommandTimeout    = 5 * time.Second
	// ostreeApplyTimeout bounds the operator-supplied ostree apply_command. An OS
	// image apply legitimately takes minutes, so the bound is deliberately
	// generous; it exists only to stop a wedged apply from blocking the upgrade
	// handler forever.
	ostreeApplyTimeout = 30 * time.Minute
)

type ostreeStatus struct {
	Source     string `json:"source,omitempty"`
	Version    string `json:"version,omitempty"`
	Checksum   string `json:"checksum,omitempty"`
	Image      string `json:"image,omitempty"`
	Booted     bool   `json:"booted"`
	Deployment string `json:"deployment,omitempty"`
	Error      string `json:"error,omitempty"`
}

type rpmOstreeStatus struct {
	Deployments []rpmOstreeDeployment `json:"deployments"`
}

type rpmOstreeDeployment struct {
	Booted                  bool           `json:"booted"`
	Version                 string         `json:"version"`
	Checksum                string         `json:"checksum"`
	BaseCommit              string         `json:"base-commit"`
	ContainerImageReference string         `json:"container-image-reference"`
	Origin                  string         `json:"origin"`
	BaseCommitMeta          map[string]any `json:"base-commit-meta"`
}

func ostreeUploadPath(cfg *Config) string {
	if cfg != nil && cfg.Ostree != nil {
		if path := strings.TrimSpace(cfg.Ostree.UploadPath); path != "" {
			return path
		}
	}
	return defaultOstreeUploadPath
}

func collectOstreeStatus(ctx context.Context) (ostreeStatus, error) {
	var attempts []error

	if output, err := runStatusCommand(ctx, "rpm-ostree", "status", "--json"); err == nil {
		status, parseErr := parseRPMOstreeStatus(output)
		if parseErr == nil {
			return status, nil
		}
		attempts = append(attempts, parseErr)
	} else {
		attempts = append(attempts, err)
	}

	if output, err := runStatusCommand(ctx, "bootc", "status", "--json"); err == nil {
		status, parseErr := parseBootcStatus(output)
		if parseErr == nil {
			return status, nil
		}
		attempts = append(attempts, parseErr)
	} else {
		attempts = append(attempts, err)
	}

	if output, err := runStatusCommand(ctx, "ostree", "admin", "status"); err == nil {
		status := parseOstreeAdminStatus(output)
		if status.Deployment != "" {
			return status, nil
		}
		attempts = append(attempts, fmt.Errorf("ostree admin status did not report a booted deployment"))
	} else {
		attempts = append(attempts, err)
	}

	return ostreeStatus{}, fmt.Errorf("ostree status unavailable: %w", errors.Join(attempts...))
}

func parseRPMOstreeStatus(raw []byte) (ostreeStatus, error) {
	var parsed rpmOstreeStatus
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ostreeStatus{}, fmt.Errorf("parse rpm-ostree status: %w", err)
	}
	if len(parsed.Deployments) == 0 {
		return ostreeStatus{}, fmt.Errorf("rpm-ostree status has no deployments")
	}

	deployment := parsed.Deployments[0]
	for _, candidate := range parsed.Deployments {
		if candidate.Booted {
			deployment = candidate
			break
		}
	}

	version := deployment.Version
	if version == "" && deployment.BaseCommitMeta != nil {
		if value, ok := deployment.BaseCommitMeta["version"].(string); ok {
			version = value
		}
	}

	checksum := deployment.Checksum
	if checksum == "" {
		checksum = deployment.BaseCommit
	}
	if version == "" {
		version = imageReferenceVersion(deployment.ContainerImageReference)
	}
	if version == "" {
		version = checksum
	}

	return ostreeStatus{
		Source:     "rpm-ostree status --json",
		Version:    version,
		Checksum:   checksum,
		Image:      deployment.ContainerImageReference,
		Booted:     deployment.Booted,
		Deployment: deployment.Origin,
	}, nil
}

func parseBootcStatus(raw []byte) (ostreeStatus, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return ostreeStatus{}, fmt.Errorf("parse bootc status: %w", err)
	}

	booted := nestedMap(root, "status", "booted")
	if booted == nil {
		booted = root
	}

	status := ostreeStatus{
		Source: "bootc status --json",
		Version: firstNestedString(booted,
			[]string{"image", "version"},
			[]string{"version"},
			[]string{"base", "version"},
		),
		Checksum: firstNestedString(booted,
			[]string{"image", "image_digest"},
			[]string{"image", "digest"},
			[]string{"checksum"},
			[]string{"base", "checksum"},
		),
		Image: firstNestedString(booted,
			[]string{"image", "image"},
			[]string{"image", "reference"},
			[]string{"image"},
		),
		Booted: true,
	}

	if status.Version == "" && status.Checksum == "" && status.Image == "" {
		return ostreeStatus{}, fmt.Errorf("bootc status has no booted image/version fields")
	}
	if status.Version == "" {
		status.Version = imageReferenceVersion(status.Image)
	}
	if status.Version == "" {
		status.Version = status.Checksum
	}

	return status, nil
}

func parseOstreeAdminStatus(raw []byte) ostreeStatus {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "*") {
			continue
		}
		return ostreeStatus{
			Source:     "ostree admin status",
			Booted:     true,
			Deployment: strings.TrimSpace(strings.TrimPrefix(trimmed, "*")),
		}
	}
	return ostreeStatus{Source: "ostree admin status"}
}

func nestedMap(root map[string]any, keys ...string) map[string]any {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func firstNestedString(root map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value := nestedString(root, path...); value != "" {
			return value
		}
	}
	return ""
}

func nestedString(root map[string]any, keys ...string) string {
	var current any = root
	for _, key := range keys {
		currentMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = currentMap[key]
	}

	switch value := current.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func runStatusCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, statusCommandTimeout)
	defer cancel()
	return runCommandOutput(commandCtx, name, args...)
}
