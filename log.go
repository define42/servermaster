package main

import (
	"io"
	"log"
	"strings"
	"sync"
)

const (
	servermasterLogTail = 100
)

// serviceLog retains the most recent log lines in memory so the
// /servermaster/status endpoint can surface them in servermaster_log.
// captureServiceLog tees the standard logger into it at startup.
//
//nolint:gochecknoglobals // process-wide log ring teed from the standard logger.
var serviceLog = newLogRing(servermasterLogTail)

// logRing is a bounded, concurrency-safe buffer of the most recent log lines. It
// is an io.Writer, so installing it as (part of) the standard logger's output
// captures every log.Print* call. The standard logger writes one full record per
// Write, so each Write is stored as one line.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(size int) *logRing {
	return &logRing{max: size}
}

func (r *logRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		// Copy the tail into a fresh slice so the dropped lines' backing array is
		// released rather than retained behind a reslice.
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

// snapshot returns a copy of the retained lines, oldest first. It is always
// non-nil so the JSON field renders as [] rather than null when empty.
func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// captureServiceLog tees the standard logger to its existing destination and the
// in-memory ring, so logs still reach stderr/journald while becoming queryable
// via /servermaster/status.
func captureServiceLog() {
	log.SetOutput(io.MultiWriter(log.Writer(), serviceLog))
}
