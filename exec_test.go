package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCommand(t *testing.T) {
	if err := runCommand(context.Background(), "true"); err != nil {
		t.Fatalf("runCommand(true): %v", err)
	}
	if err := runCommand(context.Background(), "sh", "-c", "exit 1"); err == nil {
		t.Fatal("expected error from exit 1")
	}
	err := runCommand(context.Background(), "sh", "-c", "echo problem; exit 1")
	if err == nil || !strings.Contains(err.Error(), "problem") {
		t.Fatalf("err = %v, want output included", err)
	}
}

func TestRunCommandContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runCommand(ctx, "sh", "-c", "sleep 2"); err == nil {
		t.Fatal("expected error when context deadline exceeded")
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
