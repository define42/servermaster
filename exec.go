package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// hostCommandTimeout bounds the short host commands run through runCommand
	// (hostnamectl, systemctl reboot) so a wedged command cannot hang the
	// reconcile or the restart handler indefinitely.
	hostCommandTimeout = 30 * time.Second
)

// runCommand runs a host command and returns its error, discarding output. It
// delegates to runCommandOutput so callers must pass a context: a bounded one
// stops a wedged command (hostnamectl, systemctl reboot, the operator-supplied
// ostree apply_command) from blocking its caller — and any applyMu it holds —
// indefinitely.
func runCommand(ctx context.Context, name string, args ...string) error {
	_, err := runCommandOutput(ctx, name, args...)
	return err
}

func runCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // runs fixed tool commands (nmstatectl, rpm-ostree, hostnamectl, systemctl) and the operator-declared ostree.apply_command; managing the host is this tool's purpose, and none of it is request input.
	output, err := cmd.CombinedOutput()
	if err == nil {
		return output, nil
	}

	message := strings.TrimSpace(string(output))
	if ctxErr := ctx.Err(); ctxErr != nil {
		if message == "" {
			return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctxErr)
		}
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), ctxErr, message)
	}
	if message == "" {
		return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
}
