package main

import (
	"context"
	"net"
	"testing"
)

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

func TestNestedStringStringer(t *testing.T) {
	root := map[string]any{"addr": net.IPv4(10, 0, 0, 1)} // net.IP is a fmt.Stringer
	if got := nestedString(root, "addr"); got != "10.0.0.1" {
		t.Fatalf("nestedString = %q, want 10.0.0.1", got)
	}
	if got := nestedString(root, "missing", "deep"); got != "" {
		t.Fatalf("nestedString(missing) = %q, want empty", got)
	}
}

func TestCollectOstreeStatusSuccess(t *testing.T) {
	fakeCommand(t, "rpm-ostree", `echo '{"deployments":[{"booted":true,"checksum":"abc123"}]}'`)
	st, err := collectOstreeStatus(context.Background())
	if err != nil {
		t.Fatalf("collectOstreeStatus: %v", err)
	}
	if st.Checksum != "abc123" || !st.Booted {
		t.Fatalf("status = %+v", st)
	}
}

func TestCollectOstreeStatusFallbacks(t *testing.T) {
	t.Run("bootc after rpm parse failure", func(t *testing.T) {
		fakeCommand(t, "rpm-ostree", "echo not-json")
		fakeCommand(t, "bootc", `echo '{"image":{"reference":"quay.io/os:v2"}}'`)

		st, err := collectOstreeStatus(context.Background())
		if err != nil {
			t.Fatalf("collectOstreeStatus: %v", err)
		}
		if st.Source != "bootc status --json" || st.Version != "v2" {
			t.Fatalf("status = %+v, want bootc v2", st)
		}
	})

	t.Run("ostree admin after rpm and bootc fail", func(t *testing.T) {
		fakeCommand(t, "rpm-ostree", "exit 1")
		fakeCommand(t, "bootc", "echo '{}'")
		fakeCommand(t, "ostree", "printf '  old deployment\\n* fedora abc123.0\\n'")

		st, err := collectOstreeStatus(context.Background())
		if err != nil {
			t.Fatalf("collectOstreeStatus: %v", err)
		}
		if st.Source != "ostree admin status" || st.Deployment != "fedora abc123.0" || !st.Booted {
			t.Fatalf("status = %+v, want ostree admin booted deployment", st)
		}
	})
}

func TestParseRPMOstreeStatus(t *testing.T) {
	raw := []byte(`{
	  "deployments": [
	    {"booted": false, "version": "old", "checksum": "oldsum"},
	    {"booted": true, "version": "edge.1", "checksum": "newsum", "origin": "edge", "container-image-reference": "quay.io/example/os:edge.1"}
	  ]
	}`)

	status, err := parseRPMOstreeStatus(raw)
	if err != nil {
		t.Fatalf("parseRPMOstreeStatus: %v", err)
	}
	if !status.Booted || status.Version != "edge.1" || status.Checksum != "newsum" || status.Image != "quay.io/example/os:edge.1" {
		t.Fatalf("unexpected ostree status: %+v", status)
	}
}

func TestOstreeUploadPath(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, defaultOstreeUploadPath},
		{"no ostree section", &Config{}, defaultOstreeUploadPath},
		{"blank path", &Config{Ostree: &OstreeConfig{UploadPath: "  "}}, defaultOstreeUploadPath},
		{"explicit path", &Config{Ostree: &OstreeConfig{UploadPath: "/srv/img.tar"}}, "/srv/img.tar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ostreeUploadPath(tt.cfg); got != tt.want {
				t.Fatalf("ostreeUploadPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
