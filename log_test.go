package main

import (
	"reflect"
	"testing"
)

func TestLogRing(t *testing.T) {
	r := newLogRing(3)

	if got := r.snapshot(); got == nil || len(got) != 0 {
		t.Fatalf("empty ring snapshot = %#v, want non-nil empty slice", got)
	}

	// The standard logger writes one record per Write, with a trailing newline.
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := r.snapshot(); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("snapshot = %v, want [one two three] (trailing newline trimmed)", got)
	}

	// Writing past the cap keeps only the most recent max lines, oldest dropped.
	if _, err := r.Write([]byte("four\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := r.snapshot(); !reflect.DeepEqual(got, []string{"two", "three", "four"}) {
		t.Fatalf("snapshot = %v, want [two three four]", got)
	}

	// snapshot returns a copy: mutating it must not affect the ring.
	snap := r.snapshot()
	snap[0] = "mutated"
	if got := r.snapshot(); got[0] != "two" {
		t.Fatalf("snapshot is not a copy: ring shows %q", got[0])
	}
}
