package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fakePodman is an in-process stand-in for the libpod REST API served over a
// unix socket, letting the Podman client functions be exercised without a real
// Podman. Tests set the fields to shape its responses and read the recorded
// request slices to assert what the code under test called.
type fakePodman struct {
	containers    map[string]bool                     // name/id -> exists (exists endpoint)
	images        map[string]bool                     // ref -> present (exists endpoint)
	list          []listedContainer                   // GET /containers/json
	inspect       map[string]containerInspectResponse // id/name -> inspect
	inspectStatus int                                 // status for GET /containers/{id}/json (0 -> 200)
	logFrames     []byte                              // multiplexed GET /containers/{id}/logs body
	logsStatus    int                                 // status for GET /containers/{id}/logs (0 -> 200)
	pullBody      string                              // streamed POST /images/pull body
	pullStatus    int                                 // status for POST /images/pull (0 -> 200)
	failCreate    bool                                // make POST /containers/create return 500

	created []string
	started []string
	stopped []string
	removed []string
	pulled  []string
}

// start launches the fake server on a temp unix socket, points the package's
// podmanSocketPath at it, and returns a bindings context wired to it. Everything
// is torn down on test cleanup.
func (f *fakePodman) start(t *testing.T) context.Context {
	t.Helper()
	if f.containers == nil {
		f.containers = map[string]bool{}
	}
	if f.images == nil {
		f.images = map[string]bool{}
	}
	if f.inspect == nil {
		f.inspect = map[string]containerInspectResponse{}
	}

	sock := filepath.Join(t.TempDir(), "podman.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	server := &http.Server{Handler: f.handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	prev := podmanSocketPath
	podmanSocketPath = sock
	t.Cleanup(func() { podmanSocketPath = prev })

	ctx, err := bindings.NewConnection(context.Background(), "unix:"+sock)
	if err != nil {
		t.Fatalf("connect to fake podman: %v", err)
	}
	return ctx
}

func TestSplitLogFrame(t *testing.T) {
	if got := splitLogFrame(""); got != nil {
		t.Fatalf("splitLogFrame(empty) = %v, want nil", got)
	}
	if got := splitLogFrame("a\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLogFrame = %v, want [a b]", got)
	}
}

func TestAppendStatusError(t *testing.T) {
	if got := appendStatusError("", "first"); got != "first" {
		t.Fatalf("appendStatusError(empty) = %q", got)
	}
	if got := appendStatusError("first", "second"); got != "first; second" {
		t.Fatalf("appendStatusError = %q, want joined", got)
	}
}

func TestContainerUpToDateNilConfig(t *testing.T) {
	inspect := containerInspectResponse{State: &containerInspectState{Running: true}, Config: nil}
	if containerUpToDate(inspect, "hash") {
		t.Fatal("a running container with no config is not up to date")
	}
}

// handler routes the libpod endpoints used by the tool. The bindings prefix
// every path with /v<ver>/libpod, which is stripped before dispatch.
func (f *fakePodman) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Libpod-API-Version", "5.8.2")
		w.WriteHeader(http.StatusOK)
	})
	f.registerContainerRoutes(mux)
	f.registerImageRoutes(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i := strings.Index(r.URL.Path, "/libpod"); i >= 0 {
			r.URL.Path = r.URL.Path[i+len("/libpod"):]
		}
		mux.ServeHTTP(w, r)
	})
}

func TestDemuxShortReader(t *testing.T) {
	buf := make([]byte, 1024)
	if _, _, err := demuxHeader(strings.NewReader("123"), buf); err == nil {
		t.Fatal("demuxHeader expected error on truncated header")
	}
	if _, err := demuxFrame(strings.NewReader("12"), buf, 10); err == nil {
		t.Fatal("demuxFrame expected error on truncated payload")
	}
	// An undersized buffer is grown internally before the 8-byte header read.
	header := multiplexedLog(1, "")[:8]
	fd, length, err := demuxHeader(strings.NewReader(string(header)), make([]byte, 4))
	if err != nil || fd != 1 || length != 0 {
		t.Fatalf("demuxHeader(small buf) = %d,%d,%v", fd, length, err)
	}
}

func (f *fakePodman) registerContainerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, f.list)
	})
	mux.HandleFunc("GET /containers/{name}/exists", func(w http.ResponseWriter, r *http.Request) {
		f.existsResponse(w, f.containers[r.PathValue("name")])
	})
	mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		if f.inspectStatus != 0 {
			writeJSON(w, f.inspectStatus, errBody("inspect boom"))
			return
		}
		writeJSON(w, http.StatusOK, f.inspect[r.PathValue("name")])
	})
	mux.HandleFunc("GET /containers/{name}/logs", func(w http.ResponseWriter, _ *http.Request) {
		if f.logsStatus != 0 {
			writeJSON(w, f.logsStatus, errBody("logs boom"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.logFrames)
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
		f.created = append(f.created, "create")
		if f.failCreate {
			writeJSON(w, http.StatusInternalServerError, errBody("create boom"))
			return
		}
		writeJSON(w, http.StatusCreated, containerCreateResponse{ID: "ctr-new"})
	})
	mux.HandleFunc("POST /containers/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		f.started = append(f.started, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /containers/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.stopped = append(f.stopped, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /containers/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.removed = append(f.removed, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
}

func (f *fakePodman) registerImageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /images/{ref}/exists", func(w http.ResponseWriter, r *http.Request) {
		f.existsResponse(w, f.images[r.PathValue("ref")])
	})
	mux.HandleFunc("POST /images/pull", func(w http.ResponseWriter, r *http.Request) {
		f.pulled = append(f.pulled, r.URL.Query().Get("reference"))
		if f.pullStatus != 0 {
			writeJSON(w, f.pullStatus, errBody("pull boom"))
			return
		}
		body := f.pullBody
		if body == "" {
			body = `{"images":["sha256:abc"]}`
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func (f *fakePodman) existsResponse(w http.ResponseWriter, present bool) {
	if present {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func errBody(msg string) map[string]any {
	return map[string]any{"cause": msg, "message": msg, "response": 500}
}

// multiplexedLog builds one podman log frame: an 8-byte header (stream fd in
// byte 0, payload length big-endian in bytes 4-7) followed by the payload.
func multiplexedLog(fd byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = fd
	binary.BigEndian.PutUint32(header[4:8], uint32(len(text))) //nolint:gosec // test payloads are tiny; length fits uint32.
	return append(header, []byte(text)...)
}

func TestContainerExists(t *testing.T) {
	f := &fakePodman{containers: map[string]bool{"web": true}}
	ctx := f.start(t)

	got, err := containerExists(ctx, "web")
	if err != nil || !got {
		t.Fatalf("containerExists(web) = %v, %v; want true, nil", got, err)
	}
	got, err = containerExists(ctx, "absent")
	if err != nil || got {
		t.Fatalf("containerExists(absent) = %v, %v; want false, nil", got, err)
	}
}

func TestImageExists(t *testing.T) {
	f := &fakePodman{images: map[string]bool{"nginx": true}}
	ctx := f.start(t)

	got, err := imageExists(ctx, "nginx")
	if err != nil || !got {
		t.Fatalf("imageExists(nginx) = %v, %v; want true, nil", got, err)
	}
	got, err = imageExists(ctx, "absent")
	if err != nil || got {
		t.Fatalf("imageExists(absent) = %v, %v; want false, nil", got, err)
	}
}

func TestPullImage(t *testing.T) {
	f := &fakePodman{}
	ctx := f.start(t)

	if err := pullImage(ctx, "nginx"); err != nil {
		t.Fatalf("pullImage: %v", err)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "nginx" {
		t.Fatalf("pull recorded %v, want [nginx]", f.pulled)
	}
}

func TestPullImageReportsError(t *testing.T) {
	f := &fakePodman{pullBody: `{"error":"manifest unknown"}`}
	ctx := f.start(t)

	err := pullImage(ctx, "nginx")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("pullImage err = %v, want manifest unknown", err)
	}
}

func TestListContainers(t *testing.T) {
	f := &fakePodman{list: []listedContainer{
		{ID: "abc", Names: []string{"web"}, State: "running"},
	}}
	ctx := f.start(t)

	got, err := listContainers(ctx)
	if err != nil {
		t.Fatalf("listContainers: %v", err)
	}
	if len(got) != 1 || got[0].Names[0] != "web" {
		t.Fatalf("listContainers = %+v", got)
	}
}

func TestInspectContainer(t *testing.T) {
	f := &fakePodman{inspect: map[string]containerInspectResponse{
		"web": {ID: "abc", Name: "/web", ImageName: "docker.io/library/nginx:1.25"},
	}}
	ctx := f.start(t)

	got, err := inspectContainer(ctx, "web")
	if err != nil {
		t.Fatalf("inspectContainer: %v", err)
	}
	if got.ImageName != "docker.io/library/nginx:1.25" {
		t.Fatalf("inspect = %+v", got)
	}
}

func TestStartStopRemoveContainer(t *testing.T) {
	f := &fakePodman{}
	ctx := f.start(t)

	if err := startContainer(ctx, "web"); err != nil {
		t.Fatalf("startContainer: %v", err)
	}
	if err := stopContainer(ctx, "web"); err != nil {
		t.Fatalf("stopContainer: %v", err)
	}
	if err := removeContainer(ctx, "web"); err != nil {
		t.Fatalf("removeContainer: %v", err)
	}
	if len(f.started) != 1 || len(f.stopped) != 1 || len(f.removed) != 1 {
		t.Fatalf("recorded start=%v stop=%v remove=%v", f.started, f.stopped, f.removed)
	}
}

func TestCreateContainer(t *testing.T) {
	f := &fakePodman{}
	ctx := f.start(t)

	created, err := createContainer(ctx, &containerSpec{Name: "web", Image: "nginx"})
	if err != nil {
		t.Fatalf("createContainer: %v", err)
	}
	if created.ID != "ctr-new" {
		t.Fatalf("created.ID = %q, want ctr-new", created.ID)
	}
}

func TestCreateContainerError(t *testing.T) {
	f := &fakePodman{failCreate: true}
	ctx := f.start(t)

	if _, err := createContainer(ctx, &containerSpec{Name: "web", Image: "nginx"}); err == nil {
		t.Fatal("createContainer expected error on 500")
	}
}

func TestReconcileContainerCreatesWhenMissing(t *testing.T) {
	f := &fakePodman{} // no existing container, no image present
	ctx := f.start(t)

	if err := reconcileContainer(ctx, ContainerConfig{Name: "web", Image: "nginx"}); err != nil {
		t.Fatalf("reconcileContainer: %v", err)
	}
	if len(f.pulled) != 1 {
		t.Fatalf("expected image pull, pulled=%v", f.pulled)
	}
	if len(f.created) != 1 || len(f.started) != 1 {
		t.Fatalf("expected create+start, created=%v started=%v", f.created, f.started)
	}
	if len(f.removed) != 0 {
		t.Fatalf("nothing should be removed when missing, removed=%v", f.removed)
	}
}

func TestReconcileContainerLeavesCurrentRunning(t *testing.T) {
	c := ContainerConfig{Name: "web", Image: "nginx"}
	hash := configHash(c)
	f := &fakePodman{
		containers: map[string]bool{"web": true},
		images:     map[string]bool{"nginx": true},
		inspect: map[string]containerInspectResponse{"web": {
			State:  &containerInspectState{Running: true},
			Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}},
		}},
	}
	ctx := f.start(t)

	if err := reconcileContainer(ctx, c); err != nil {
		t.Fatalf("reconcileContainer: %v", err)
	}
	if len(f.created) != 0 || len(f.started) != 0 || len(f.pulled) != 0 {
		t.Fatalf("up-to-date container must be left alone: created=%v started=%v pulled=%v", f.created, f.started, f.pulled)
	}
}

func TestReconcileContainerRecreatesStale(t *testing.T) {
	c := ContainerConfig{Name: "web", Image: "nginx"}
	f := &fakePodman{
		containers: map[string]bool{"web": true},
		images:     map[string]bool{"nginx": true}, // present, so no pull
		inspect: map[string]containerInspectResponse{"web": {
			State:  &containerInspectState{Running: true},
			Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: "stale"}},
		}},
	}
	ctx := f.start(t)

	if err := reconcileContainer(ctx, c); err != nil {
		t.Fatalf("reconcileContainer: %v", err)
	}
	if len(f.pulled) != 0 {
		t.Fatalf("image present, should not pull; pulled=%v", f.pulled)
	}
	if len(f.removed) != 1 || len(f.created) != 1 || len(f.started) != 1 {
		t.Fatalf("stale container should be removed+recreated: removed=%v created=%v started=%v", f.removed, f.created, f.started)
	}
}

func TestStopUnmanagedContainers(t *testing.T) {
	f := &fakePodman{list: []listedContainer{
		{ID: "a", Names: []string{"web"}, State: "running"},   // configured, keep
		{ID: "b", Names: []string{"rogue"}, State: "running"}, // unmanaged + running -> stop
		{ID: "c", Names: []string{"old"}, State: "exited"},    // unmanaged but already stopped -> skip
	}}
	ctx := f.start(t)

	if err := stopUnmanagedContainers(ctx, []ContainerConfig{{Name: "web"}}); err != nil {
		t.Fatalf("stopUnmanagedContainers: %v", err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "b" {
		t.Fatalf("expected only rogue (id b) stopped, got %v", f.stopped)
	}
}

func TestCollectRunningContainerStatuses(t *testing.T) {
	f := &fakePodman{
		list: []listedContainer{
			{ID: "abc", Names: []string{"web"}, State: "running", Image: "nginx"},
			{ID: "def", Names: []string{"stopped"}, State: "exited"},
		},
		inspect: map[string]containerInspectResponse{"abc": {
			ID:        "abc",
			Name:      "/web",
			ImageName: "docker.io/library/nginx:1.25",
		}},
		logFrames: append(multiplexedLog(1, "ready\n"), multiplexedLog(2, "warn\n")...),
	}
	ctx := f.start(t)

	got, err := collectRunningContainerStatuses(ctx, 50)
	if err != nil {
		t.Fatalf("collectRunningContainerStatuses: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("running statuses = %d, want 1 (only running container)", len(got))
	}
	st := got[0]
	if st.Name != "web" || st.Image != "docker.io/library/nginx:1.25" || st.Version != "1.25" {
		t.Fatalf("status overlay mismatch: %+v", st)
	}
	if len(st.Logs) != 2 || st.Logs[0] != "stdout: ready" || st.Logs[1] != "stderr: warn" {
		t.Fatalf("logs = %v, want [stdout: ready stderr: warn]", st.Logs)
	}
}

func TestBuildRunningContainerStatusRecordsErrors(t *testing.T) {
	f := &fakePodman{
		list:          []listedContainer{{ID: "abc", Names: []string{"web"}, State: "running"}},
		inspectStatus: http.StatusInternalServerError,
		logsStatus:    http.StatusInternalServerError,
	}
	ctx := f.start(t)

	got, err := collectRunningContainerStatuses(ctx, 10)
	if err != nil {
		t.Fatalf("collectRunningContainerStatuses: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Error, "inspect") || !strings.Contains(got[0].Error, "logs") {
		t.Fatalf("error = %q, want both inspect and logs recorded", got[0].Error)
	}
}

func TestContainerLogLinesStreamError(t *testing.T) {
	f := &fakePodman{logFrames: multiplexedLog(3, "boom")}
	ctx := f.start(t)

	if _, err := containerLogLines(ctx, "web", 10); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("containerLogLines err = %v, want stream error boom", err)
	}
}

// TestPodmanClientWithoutConnection exercises the GetClient error guard that
// every client function shares: a bare context carries no bindings client.
func TestPodmanClientWithoutConnection(t *testing.T) {
	ctx := context.Background()
	checks := []func() error{
		func() error { _, err := containerExists(ctx, "x"); return err },
		func() error { _, err := imageExists(ctx, "x"); return err },
		func() error { return pullImage(ctx, "x") },
		func() error { _, err := listContainers(ctx); return err },
		func() error { _, err := inspectContainer(ctx, "x"); return err },
		func() error { _, err := createContainer(ctx, &containerSpec{Image: "x"}); return err },
		func() error { return startContainer(ctx, "x") },
		func() error { return stopContainer(ctx, "x") },
		func() error { return removeContainer(ctx, "x") },
		func() error { _, err := containerLogLines(ctx, "x", 1); return err },
	}
	for i, check := range checks {
		if err := check(); err == nil {
			t.Fatalf("check %d: expected error without a connection", i)
		}
	}
}

func TestReconcileContainerExistsCheckError(t *testing.T) {
	if err := reconcileContainer(context.Background(), ContainerConfig{Name: "web", Image: "nginx"}); err == nil {
		t.Fatal("expected error when the container existence check cannot connect")
	}
}

func TestReconcileContainerPullError(t *testing.T) {
	f := &fakePodman{pullStatus: http.StatusInternalServerError} // missing container, missing image
	ctx := f.start(t)
	if err := reconcileContainer(ctx, ContainerConfig{Name: "web", Image: "nginx"}); err == nil {
		t.Fatal("expected error when the image pull fails")
	}
}

func TestContainerIsCurrentInspectError(t *testing.T) {
	f := &fakePodman{containers: map[string]bool{"web": true}, inspectStatus: http.StatusInternalServerError}
	ctx := f.start(t)
	if _, err := containerIsCurrent(ctx, "web", true, "hash"); err == nil {
		t.Fatal("expected error when inspect fails")
	}
}

func TestStopUnmanagedContainersListError(t *testing.T) {
	if err := stopUnmanagedContainers(context.Background(), nil); err == nil {
		t.Fatal("expected error when listing containers cannot connect")
	}
}

func TestCollectRunningContainerStatusesConnectError(t *testing.T) {
	prev := podmanSocketPath
	podmanSocketPath = filepath.Join(t.TempDir(), "absent.sock")
	defer func() { podmanSocketPath = prev }()
	if _, err := collectRunningContainerStatuses(context.Background(), 10); err == nil {
		t.Fatal("expected error connecting to a nonexistent socket")
	}
}

func TestPullImageNonSuccess(t *testing.T) {
	f := &fakePodman{pullStatus: http.StatusInternalServerError}
	ctx := f.start(t)
	if err := pullImage(ctx, "nginx"); err == nil {
		t.Fatal("expected error when pull returns 500")
	}
}

func TestPullImageUnexpectedReport(t *testing.T) {
	// A stream line is informational; the trailing empty object is unexpected.
	f := &fakePodman{pullBody: "{\"stream\":\"pulling\"}\n{}\n"}
	ctx := f.start(t)
	if err := pullImage(ctx, "nginx"); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("err = %v, want unexpected image pull response", err)
	}
}

func TestPullImageDecodeError(t *testing.T) {
	f := &fakePodman{pullBody: "{\"stream\":\"ok\"}\n{bad json"}
	ctx := f.start(t)
	if err := pullImage(ctx, "nginx"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func TestReconcileContainerCreateError(t *testing.T) {
	f := &fakePodman{images: map[string]bool{"nginx": true}, failCreate: true}
	ctx := f.start(t)
	if err := reconcileContainer(ctx, ContainerConfig{Name: "web", Image: "nginx"}); err == nil {
		t.Fatal("expected error when create fails")
	}
}

func TestContainerLogLinesEndpointError(t *testing.T) {
	f := &fakePodman{logsStatus: http.StatusInternalServerError}
	ctx := f.start(t)
	if _, err := containerLogLines(ctx, "web", 10); err == nil {
		t.Fatal("expected error when logs endpoint returns 500")
	}
}

func TestContainerLogLinesBadChannel(t *testing.T) {
	f := &fakePodman{logFrames: multiplexedLog(7, "x")} // channel 7 is out of range
	ctx := f.start(t)
	if _, err := containerLogLines(ctx, "web", 10); err == nil || !strings.Contains(err.Error(), "lost sync") {
		t.Fatalf("err = %v, want lost sync", err)
	}
}

func TestContainerLogLinesLargeFrame(t *testing.T) {
	big := strings.Repeat("x", 4096) // larger than the 1 KiB read buffer
	f := &fakePodman{logFrames: multiplexedLog(1, big+"\n")}
	ctx := f.start(t)
	lines, err := containerLogLines(ctx, "web", 10)
	if err != nil {
		t.Fatalf("containerLogLines: %v", err)
	}
	if len(lines) != 1 || lines[0] != "stdout: "+big {
		t.Fatalf("large frame not reassembled: got %d lines", len(lines))
	}
}

func TestContainerLogLinesTailTrim(t *testing.T) {
	var frames []byte
	for i := range 5 {
		frames = append(frames, multiplexedLog(1, "line"+strconv.Itoa(i)+"\n")...)
	}
	f := &fakePodman{logFrames: frames}
	ctx := f.start(t)

	lines, err := containerLogLines(ctx, "web", 2)
	if err != nil {
		t.Fatalf("containerLogLines: %v", err)
	}
	if len(lines) != 2 || lines[0] != "stdout: line3" || lines[1] != "stdout: line4" {
		t.Fatalf("tail-trimmed lines = %v, want last two", lines)
	}
}

func TestContainerNeedsStop(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"Running", true},
		{"paused", true},
		{"restarting", true},
		{"unrecognized", true},
		{"created", false},
		{"configured", false},
		{"dead", false},
		{"exited", false},
		{"removing", false},
		{"stopped", false},
		{"EXITED", false},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := containerNeedsStop(tt.state); got != tt.want {
				t.Fatalf("containerNeedsStop(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestContainerIsConfigured(t *testing.T) {
	configured := map[string]struct{}{"web": {}, "db": {}}

	tests := []struct {
		name      string
		container listedContainer
		want      bool
	}{
		{"matches", listedContainer{Names: []string{"web"}}, true},
		{"matches one of many", listedContainer{Names: []string{"other", "db"}}, true},
		{"no match", listedContainer{Names: []string{"unmanaged"}}, false},
		{"no names", listedContainer{Names: nil}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerIsConfigured(tt.container, configured); got != tt.want {
				t.Fatalf("containerIsConfigured(%v) = %v, want %v", tt.container.Names, got, tt.want)
			}
		})
	}
}

func TestContainerDisplayName(t *testing.T) {
	tests := []struct {
		name      string
		container listedContainer
		want      string
	}{
		{"prefers name", listedContainer{Names: []string{"web"}, ID: "abc123"}, "web"},
		{"falls back to id", listedContainer{ID: "abc123"}, "abc123"},
		{"unknown", listedContainer{}, "<unknown>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerDisplayName(tt.container); got != tt.want {
				t.Fatalf("containerDisplayName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateSpec(t *testing.T) {
	c := ContainerConfig{
		Name:  "web",
		Image: "docker.io/library/nginx:latest",
		User:  "0:0",
		Env:   map[string]string{"TZ": "Europe/Copenhagen"},
		Ports: []PortConfig{
			{HostIP: "0.0.0.0", HostPort: 8081, ContainerPort: 80},
			{HostIP: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
		},
		Volumes: []VolumeConfig{
			{HostPath: "/var/data/web", ContainerPath: "/usr/share/nginx/html", ReadOnly: true, SELinux: "Z"},
			{HostPath: "/var/data/cache", ContainerPath: "/cache", ReadOnly: false},
		},
		Restart:     "always",
		NetworkMode: "host",
		ReadOnly:    true,
		CapAdd:      []string{"CAP_NET_ADMIN"},
		CapDrop:     []string{"CAP_SYS_ADMIN"},
	}

	spec, err := createSpec(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.Name != "web" || spec.Image != c.Image || spec.User != "0:0" {
		t.Fatalf("spec metadata mismatch: %+v", spec)
	}

	if spec.PortMappings[0].Protocol != "tcp" {
		t.Fatalf("port without protocol should default to tcp, got %q", spec.PortMappings[0].Protocol)
	}
	if spec.PortMappings[0].HostPort != 8081 || spec.PortMappings[0].ContainerPort != 80 {
		t.Fatalf("first port mapping mismatch: %+v", spec.PortMappings[0])
	}
	if spec.PortMappings[1].Protocol != "udp" {
		t.Fatalf("explicit protocol should be preserved, got %q", spec.PortMappings[1].Protocol)
	}

	if !reflect.DeepEqual(spec.Mounts[0].Options, []string{"rbind", "ro", "Z"}) {
		t.Fatalf("read-only mount options = %v, want [rbind ro Z]", spec.Mounts[0].Options)
	}
	if !reflect.DeepEqual(spec.Mounts[1].Options, []string{"rbind", "rw"}) {
		t.Fatalf("read-write mount options = %v, want [rbind rw]", spec.Mounts[1].Options)
	}
	if spec.Mounts[0].Type != "bind" || spec.Mounts[0].Source != "/var/data/web" || spec.Mounts[0].Destination != "/usr/share/nginx/html" {
		t.Fatalf("mount mapping mismatch: %+v", spec.Mounts[0])
	}

	if spec.RestartPolicy != "always" {
		t.Fatalf("restart policy = %q, want always", spec.RestartPolicy)
	}

	checkRuntimeOptions(t, spec)
}

func checkRuntimeOptions(t *testing.T, spec *containerSpec) {
	t.Helper()
	if spec.Netns == nil || spec.Netns.NSMode != "host" {
		t.Fatalf("netns = %+v, want nsmode=host", spec.Netns)
	}
	if !spec.ReadOnlyFilesystem {
		t.Fatalf("read_only_filesystem = false, want true")
	}
	if !reflect.DeepEqual(spec.CapAdd, []string{"CAP_NET_ADMIN"}) {
		t.Fatalf("cap_add = %v, want [CAP_NET_ADMIN]", spec.CapAdd)
	}
	if !reflect.DeepEqual(spec.CapDrop, []string{"CAP_SYS_ADMIN"}) {
		t.Fatalf("cap_drop = %v, want [CAP_SYS_ADMIN]", spec.CapDrop)
	}
}

func TestCreateSpecDefaults(t *testing.T) {
	spec, err := createSpec(ContainerConfig{Name: "x", Image: "busybox"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Netns != nil {
		t.Fatalf("netns should be nil when network_mode is unset, got %+v", spec.Netns)
	}
	if spec.ReadOnlyFilesystem {
		t.Fatalf("read_only_filesystem should default to false")
	}
	if spec.CapAdd != nil || spec.CapDrop != nil {
		t.Fatalf("capabilities should default to nil, got add=%v drop=%v", spec.CapAdd, spec.CapDrop)
	}
}

func TestConfigHash(t *testing.T) {
	base := ContainerConfig{
		Name:  "web",
		Image: "docker.io/library/nginx:1.25",
		Env:   map[string]string{"A": "1", "B": "2"},
		Ports: []PortConfig{{HostPort: 8081, ContainerPort: 80}},
	}

	baseHash := configHash(base)

	// Map ordering must not affect the hash (Go marshals map keys sorted), and an
	// equal config must produce an equal hash.
	reordered := base
	reordered.Env = map[string]string{"B": "2", "A": "1"}
	if configHash(reordered) != baseHash {
		t.Fatal("hash changed for an equal config (map literal order should not matter)")
	}

	changes := map[string]ContainerConfig{
		"image":   {Name: "web", Image: "docker.io/library/nginx:1.26"},
		"env":     {Name: "web", Image: "docker.io/library/nginx:1.25", Env: map[string]string{"A": "1", "B": "3"}},
		"command": {Name: "web", Image: "docker.io/library/nginx:1.25", Command: []string{"sleep", "1"}},
		"restart": {Name: "web", Image: "docker.io/library/nginx:1.25", Restart: "always"},
	}
	for name, changed := range changes {
		if configHash(changed) == baseHash {
			t.Fatalf("hash did not change when %s changed", name)
		}
	}
}

func TestContainerUpToDate(t *testing.T) {
	const hash = "abc123"

	running := containerInspectResponse{
		State:  &containerInspectState{Running: true, Status: "running"},
		Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}},
	}
	if !containerUpToDate(running, hash) {
		t.Fatal("running container with matching hash should be up to date")
	}

	tests := []struct {
		name    string
		inspect containerInspectResponse
		hash    string
	}{
		{
			name:    "hash differs",
			inspect: running,
			hash:    "different",
		},
		{
			name: "not running",
			inspect: containerInspectResponse{
				State:  &containerInspectState{Running: false, Status: "exited"},
				Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}},
			},
			hash: hash,
		},
		{
			name: "missing label",
			inspect: containerInspectResponse{
				State:  &containerInspectState{Running: true},
				Config: &containerInspectConfig{Labels: map[string]string{}},
			},
			hash: hash,
		},
		{
			name:    "no state",
			inspect: containerInspectResponse{Config: &containerInspectConfig{Labels: map[string]string{configHashLabel: hash}}},
			hash:    hash,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if containerUpToDate(tt.inspect, tt.hash) {
				t.Fatalf("%s should not be up to date", tt.name)
			}
		})
	}
}

func TestImageReferenceVersion(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"docker.io/library/nginx:1.25", "1.25"},
		{"localhost:5000/app/backend:v2", "v2"},
		{"localhost:5000/app/backend", ""},
		{"quay.io/example/app@sha256:abc", "sha256:abc"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			if got := imageReferenceVersion(tt.ref); got != tt.want {
				t.Fatalf("imageReferenceVersion(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
