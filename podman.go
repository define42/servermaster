package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
)

const (
	podmanRootfulMode = "rootful"
)

// podmanSocketPath is the libpod API socket the tool talks to. It is a variable
// rather than a constant so tests can point the client at a fake socket.
//
//nolint:gochecknoglobals // injectable seam so the Podman client can be tested against a fake socket.
var podmanSocketPath = "/run/podman/podman.sock"

type containerSpec struct {
	Name          string            `json:"name,omitempty"`
	Image         string            `json:"image"`
	User          string            `json:"user,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Command       []string          `json:"command,omitempty"`
	PortMappings  []portMapping     `json:"portmappings,omitempty"`
	Mounts        []mount           `json:"mounts,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

type portMapping struct {
	HostIP        string `json:"host_ip"`
	ContainerPort uint16 `json:"container_port"`
	HostPort      uint16 `json:"host_port"`
	Range         uint16 `json:"range"`
	Protocol      string `json:"protocol"`
}

type mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type imagePullReport struct {
	Stream string   `json:"stream"`
	Error  string   `json:"error"`
	Images []string `json:"images"`
	ID     string   `json:"id"`
}

type listedContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
}

type runningContainerStatus struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Names       []string `json:"names,omitempty"`
	State       string   `json:"state"`
	Image       string   `json:"image,omitempty"`
	ImageID     string   `json:"image_id,omitempty"`
	ImageDigest string   `json:"image_digest,omitempty"`
	Version     string   `json:"version,omitempty"`
	Logs        []string `json:"logs"`
	Error       string   `json:"error,omitempty"`
}

type containerInspectResponse struct {
	ID          string                  `json:"Id"`
	Name        string                  `json:"Name"`
	Image       string                  `json:"Image"`
	ImageName   string                  `json:"ImageName"`
	ImageDigest string                  `json:"ImageDigest"`
	State       *containerInspectState  `json:"State"`
	Config      *containerInspectConfig `json:"Config"`
}

type containerInspectState struct {
	Running bool   `json:"Running"`
	Status  string `json:"Status"`
}

type containerInspectConfig struct {
	Labels map[string]string `json:"Labels"`
}

// configHashLabel records, on the container, a hash of the ContainerConfig it
// was created from. reconcileContainer uses it to leave a running container
// untouched when its desired config is unchanged.
const configHashLabel = "servermaster.config-hash"

// configHash is a stable fingerprint of a container's desired configuration. Go
// marshals struct fields in declaration order and map keys in sorted order, so
// the encoding — and therefore the hash — is deterministic for equal configs.
func configHash(c ContainerConfig) string {
	data, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// reconcileContainer converges a single declared container to its desired state.
// A container that is already running with a matching config hash is left as-is,
// so an unchanged container is never restarted, re-pulled, or recreated. Any
// other case (missing, stopped, or config changed) is (re)created from the spec,
// pulling the image only when it is not already present in local storage.
func reconcileContainer(ctx context.Context, c ContainerConfig) error {
	spec, err := createSpec(c)
	if err != nil {
		return err
	}

	desiredHash := configHash(c)
	if spec.Labels == nil {
		spec.Labels = make(map[string]string, 1)
	}
	spec.Labels[configHashLabel] = desiredHash

	exists, err := containerExists(ctx, c.Name)
	if err != nil {
		return fmt.Errorf("check container %q failed: %w", c.Name, err)
	}

	current, err := containerIsCurrent(ctx, c.Name, exists, desiredHash)
	if err != nil {
		return err
	}
	if current {
		log.Printf("container %s unchanged, leaving it running", c.Name)
		return nil
	}

	present, err := imageExists(ctx, c.Image)
	if err != nil {
		return fmt.Errorf("check image %q failed: %w", c.Image, err)
	}
	if !present {
		if err := pullImage(ctx, c.Image); err != nil {
			return fmt.Errorf("pull image %q failed: %w", c.Image, err)
		}
	}

	if exists {
		if err := removeContainer(ctx, c.Name); err != nil {
			return fmt.Errorf("remove container %q failed: %w", c.Name, err)
		}
	}

	created, err := createContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container %q failed: %w", c.Name, err)
	}

	if err := startContainer(ctx, created.ID); err != nil {
		return fmt.Errorf("start container %q failed: %w", c.Name, err)
	}

	log.Printf("started container %s", c.Name)
	return nil
}

// containerIsCurrent reports whether an existing, declared container can be left
// running untouched: it must already exist and, on inspection, be running with a
// matching config hash. A container that does not exist is never current.
func containerIsCurrent(ctx context.Context, name string, exists bool, desiredHash string) (bool, error) {
	if !exists {
		return false, nil
	}
	inspect, err := inspectContainer(ctx, name)
	if err != nil {
		return false, fmt.Errorf("inspect container %q failed: %w", name, err)
	}
	return containerUpToDate(inspect, desiredHash), nil
}

// containerUpToDate reports whether an existing container is running and was
// created from the desired config (matching hash label). A stopped container, or
// one created before this label existed or from a different config, is not up to
// date and must be recreated.
func containerUpToDate(inspect containerInspectResponse, desiredHash string) bool {
	if inspect.State == nil || !inspect.State.Running {
		return false
	}
	if inspect.Config == nil {
		return false
	}
	return inspect.Config.Labels[configHashLabel] == desiredHash
}

func createSpec(c ContainerConfig) (*containerSpec, error) {
	s := &containerSpec{
		Name:    c.Name,
		Image:   c.Image,
		User:    c.User,
		Env:     c.Env,
		Command: c.Command,
	}

	for _, p := range c.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}

		// validateConfig (run before any reconcile) guarantees both ports are in
		// 1-65535, so neither uint16 conversion can overflow.
		hostPort := uint16(p.HostPort)           //nolint:gosec // bounded to 1-65535 by validateConfig.
		containerPort := uint16(p.ContainerPort) //nolint:gosec // bounded to 1-65535 by validateConfig.
		s.PortMappings = append(s.PortMappings, portMapping{
			HostIP:        p.HostIP,
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      proto,
		})
	}

	for _, v := range c.Volumes {
		options := []string{"rbind"}

		if v.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}

		if relabel := strings.TrimSpace(v.SELinux); relabel != "" {
			options = append(options, relabel)
		}

		s.Mounts = append(s.Mounts, mount{
			Type:        "bind",
			Source:      v.HostPath,
			Destination: v.ContainerPath,
			Options:     options,
		})
	}

	if c.Restart != "" {
		s.RestartPolicy = c.Restart
	}

	return s, nil
}

// imageExists reports whether an image reference is already present in local
// storage, using the same 204/404 exists endpoint pattern as containerExists.
func imageExists(ctx context.Context, rawImage string) (bool, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return false, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/images/%s/exists", nil, nil, rawImage)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.IsSuccess() {
		return true, nil
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}

	return false, response.Process(nil)
}

func pullImage(ctx context.Context, rawImage string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("reference", rawImage)

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/images/pull", params, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	if !response.IsSuccess() {
		return response.Process(nil)
	}

	var pullErrors []error
	decoder := json.NewDecoder(response.Body)
	for {
		var report imagePullReport
		if err := decoder.Decode(&report); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			pullErrors = append(pullErrors, fmt.Errorf("failed to decode image pull response: %w", err))
			break
		}

		switch {
		case report.Stream != "":
			fmt.Fprint(os.Stderr, report.Stream)
		case report.Error != "":
			pullErrors = append(pullErrors, errors.New(report.Error))
		case len(report.Images) > 0 || report.ID != "":
		default:
			pullErrors = append(pullErrors, fmt.Errorf("unexpected image pull response: %+v", report))
		}
	}

	return errors.Join(pullErrors...)
}

func stopUnmanagedContainers(ctx context.Context, configured []ContainerConfig) error {
	configuredNames := make(map[string]struct{}, len(configured))
	for _, c := range configured {
		configuredNames[c.Name] = struct{}{}
	}

	existing, err := listContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers failed: %w", err)
	}

	for _, container := range existing {
		if containerIsConfigured(container, configuredNames) || !containerNeedsStop(container.State) {
			continue
		}

		if container.ID == "" {
			return fmt.Errorf("cannot stop unmanaged container %q: missing id", containerDisplayName(container))
		}

		if err := stopContainer(ctx, container.ID); err != nil {
			return fmt.Errorf("stop unmanaged container %q failed: %w", containerDisplayName(container), err)
		}

		log.Printf("stopped unmanaged container %s", containerDisplayName(container))
	}

	return nil
}

func listContainers(ctx context.Context) ([]listedContainer, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("all", "true")

	var containers []listedContainer
	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/json", params, nil)
	if err != nil {
		return containers, err
	}
	defer func() { _ = response.Body.Close() }()

	return containers, response.Process(&containers)
}

func inspectContainer(ctx context.Context, nameOrID string) (containerInspectResponse, error) {
	var inspect containerInspectResponse

	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return inspect, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/json", nil, nil, nameOrID)
	if err != nil {
		return inspect, err
	}
	defer func() { _ = response.Body.Close() }()

	return inspect, response.Process(&inspect)
}

func collectRunningContainerStatuses(ctx context.Context, logTail int) ([]runningContainerStatus, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	statusCtx, err := bindings.NewConnection(statusCtx, "unix:"+podmanSocketPath)
	if err != nil {
		return nil, err
	}

	existing, err := listContainers(statusCtx)
	if err != nil {
		return nil, err
	}

	var running []runningContainerStatus
	for _, container := range existing {
		if !containerIsRunning(container.State) {
			continue
		}
		running = append(running, buildRunningContainerStatus(statusCtx, container, logTail))
	}

	return running, nil
}

// buildRunningContainerStatus assembles the status for one running container,
// folding inspect and log failures into the status's Error field rather than
// failing the whole collection.
func buildRunningContainerStatus(ctx context.Context, container listedContainer, logTail int) runningContainerStatus {
	status := runningContainerStatus{
		ID:    container.ID,
		Name:  containerDisplayName(container),
		Names: append([]string(nil), container.Names...),
		State: container.State,
		Image: container.Image,
		Logs:  []string{},
	}

	if inspect, err := inspectContainer(ctx, container.ID); err != nil {
		status.Error = appendStatusError(status.Error, fmt.Sprintf("inspect: %v", err))
	} else {
		applyInspectToStatus(&status, inspect)
	}

	if logs, err := containerLogLines(ctx, container.ID, logTail); err != nil {
		status.Error = appendStatusError(status.Error, fmt.Sprintf("logs: %v", err))
	} else {
		status.Logs = logs
	}

	return status
}

// applyInspectToStatus overlays the richer detail from a container inspect onto
// the status built from the list entry.
func applyInspectToStatus(status *runningContainerStatus, inspect containerInspectResponse) {
	if inspect.Name != "" {
		status.Name = strings.TrimPrefix(inspect.Name, "/")
	}
	if inspect.ImageName != "" {
		status.Image = inspect.ImageName
	}
	status.ImageID = inspect.Image
	status.ImageDigest = inspect.ImageDigest
	status.Version = imageReferenceVersion(status.Image)
	if status.Version == "" {
		status.Version = imageReferenceVersion(status.ImageDigest)
	}
}

func containerIsRunning(state string) bool {
	return strings.EqualFold(state, "running")
}

func appendStatusError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func containerLogLines(ctx context.Context, nameOrID string, tail int) ([]string, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("stdout", "true")
	params.Set("stderr", "true")
	params.Set("tail", strconv.Itoa(tail))

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/logs", params, nil, nameOrID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if !response.IsSuccess() && !response.IsInformational() {
		return nil, response.Process(nil)
	}

	var lines []string
	buffer := make([]byte, 1024)
	for {
		fd, length, err := demuxHeader(response.Body, buffer)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return lines, err
		}

		frame, err := demuxFrame(response.Body, buffer, length)
		if err != nil {
			return lines, err
		}

		frameLines, err := logFrameLines(fd, frame)
		if err != nil {
			return lines, err
		}
		lines = append(lines, frameLines...)
	}

	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	return lines, nil
}

// logFrameLines turns one demuxed podman log frame into prefixed log lines, one
// per text line. Channel 1 is stdout and 2 is stderr; channel 3 carries a
// stream error, which is surfaced as an error.
func logFrameLines(fd int, frame []byte) ([]string, error) {
	stream := "stdout"
	switch fd {
	case 1:
		stream = "stdout"
	case 2:
		stream = "stderr"
	case 3:
		return nil, fmt.Errorf("podman log stream error: %s", strings.TrimSpace(string(frame)))
	}

	var lines []string
	for _, line := range splitLogFrame(string(frame)) {
		lines = append(lines, stream+": "+line)
	}
	return lines, nil
}

func demuxHeader(r io.Reader, buffer []byte) (int, int, error) {
	if len(buffer) < 8 {
		buffer = make([]byte, 8)
	}
	if _, err := io.ReadFull(r, buffer[0:8]); err != nil {
		return 0, 0, err
	}

	fd := int(buffer[0])
	if fd < 0 || fd > 3 {
		return 0, 0, fmt.Errorf("container log stream lost sync: channel %d", fd)
	}

	return fd, int(binary.BigEndian.Uint32(buffer[4:8])), nil
}

func demuxFrame(r io.Reader, buffer []byte, length int) ([]byte, error) {
	if len(buffer) < length {
		buffer = make([]byte, length)
	}
	if _, err := io.ReadFull(r, buffer[0:length]); err != nil {
		return nil, err
	}
	return buffer[0:length], nil
}

func splitLogFrame(frame string) []string {
	frame = strings.TrimSuffix(frame, "\n")
	if frame == "" {
		return nil
	}
	return strings.Split(frame, "\n")
}

func imageReferenceVersion(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	name, digest, hasDigest := strings.Cut(ref, "@")
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		return name[lastColon+1:]
	}
	if hasDigest {
		return digest
	}
	return ""
}

func containerIsConfigured(container listedContainer, configuredNames map[string]struct{}) bool {
	for _, name := range container.Names {
		if _, exists := configuredNames[name]; exists {
			return true
		}
	}

	return false
}

func containerNeedsStop(state string) bool {
	switch strings.ToLower(state) {
	case "created", "configured", "dead", "exited", "removing", "stopped":
		return false
	default:
		return true
	}
}

func containerDisplayName(container listedContainer) string {
	if len(container.Names) > 0 {
		return container.Names[0]
	}
	if container.ID != "" {
		return container.ID
	}
	return "<unknown>"
}

func containerExists(ctx context.Context, nameOrID string) (bool, error) {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return false, err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodGet, "/containers/%s/exists", nil, nil, nameOrID)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.IsSuccess() {
		return true, nil
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}

	return false, response.Process(nil)
}

func stopContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("ignore", "true")

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/containers/%s/stop", params, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}

func removeContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("force", "true")

	response, err := conn.DoRequest(ctx, nil, http.MethodDelete, "/containers/%s", params, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}

func createContainer(ctx context.Context, spec *containerSpec) (containerCreateResponse, error) {
	var created containerCreateResponse

	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return created, err
	}

	body, err := json.Marshal(spec)
	if err != nil {
		return created, err
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")

	response, err := conn.DoRequest(ctx, bytes.NewReader(body), http.MethodPost, "/containers/create", nil, headers)
	if err != nil {
		return created, err
	}
	defer func() { _ = response.Body.Close() }()

	return created, response.Process(&created)
}

func startContainer(ctx context.Context, nameOrID string) error {
	conn, err := bindings.GetClient(ctx)
	if err != nil {
		return err
	}

	response, err := conn.DoRequest(ctx, nil, http.MethodPost, "/containers/%s/start", nil, nil, nameOrID)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	return response.Process(nil)
}
