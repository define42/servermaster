# servermaster

`servermaster` is a Go service for declaratively configuring a Red Hat Device
Edge node without MicroShift. It manages host folders, host network interfaces,
firewalld ports, OS update staging, and Podman containers directly from one JSON
configuration file.

The active config file is the source of truth for the node. On every reconcile,
`servermaster` stops any running container that is not declared in that config.
Do not start ad-hoc containers and expect them to survive; add them to the
config instead.

## What It Does

On startup, and after a successful remote config upload, `servermaster`:

1. Starts an HTTP server on `:8080`.
2. Reads the active JSON config file.
3. Creates declared host folders with the requested mode and owner.
4. Applies declared host interface settings through `nmstatectl`.
5. Opens declared firewalld ports in runtime and permanent configuration.
6. Starts the rootful `podman.socket` through systemd.
7. Waits for the Podman Unix socket to become reachable.
8. Stops running containers that are not declared in the config.
9. Pulls declared images.
10. Recreates declared containers from the desired spec.
11. Starts the declared containers.

## Usage

```sh
servermaster -config /data/config/containers.json
```

If `-config` is omitted, it defaults to `/data/config/containers.json`.
The binary has no `install` subcommand or flag; it always starts the service
process.

## Installation

### Systemd Unit

A systemd unit ships in this repo as
[`servermaster.service`](servermaster.service). It runs:

```sh
/usr/bin/servermaster -config /data/config/containers.json
```

For a manual systemd installation, place the binary where the unit expects it,
then install and enable the unit:

```sh
make build
sudo install -m 0755 dist/servermaster /usr/bin/servermaster
sudo install -m 0644 servermaster.service /etc/systemd/system/servermaster.service
sudo systemctl daemon-reload
sudo systemctl enable --now servermaster.service
```

On image-mode hosts, such as Red Hat Device Edge images built from a blueprint,
prefer the RPM flow below. Bake the package into the image and enable the unit
declaratively:

```toml
[customizations.services]
enabled = ["servermaster.service"]
```

### RPM

`make rpm` builds a static, CGO-free binary and packages it with the systemd unit
and license into `dist/`. Packaging uses the pure-Go [`cmd/mkrpm`](cmd/mkrpm),
so the build host needs only the Go toolchain.

The RPM installs:

- `servermaster` to `/usr/bin/servermaster`
- `servermaster.service` to `/usr/lib/systemd/system/servermaster.service`

It declares `Requires: podman, nmstate`, `Recommends: firewalld`, and uses
systemd scriptlets to reload and restart the service when appropriate.

```sh
make rpm                       # version from the latest git tag, or 0.0.0 if untagged
make rpm VERSION=1.2.3         # explicit version
make rpm GOARCH=arm64          # cross-build for aarch64 edge devices
```

Tagged GitHub releases attach prebuilt `x86_64` and `aarch64` RPMs.

## HTTP API

All HTTP endpoints are unauthenticated. Expose `:8080` only on a trusted
management network.

### Status

`GET /servermaster` returns a pretty-printed JSON status document with:

- ostree/bootc deployment information
- free disk space
- running Podman containers
- image and version metadata where available
- the last 100 log lines from each running container
- status collection errors, if any

```sh
curl http://node:8080/servermaster
```

### Remote Configuration

`POST /config` and `PUT /config` accept a raw config JSON document. The service
validates the body, writes it atomically to the active `-config` path, and
immediately reconciles the node to the new desired state.

- Malformed or invalid config is rejected with `400` and is not written.
- Valid config is saved before apply. If apply fails, the response is `500` and
  the next reconcile or service restart retries it.
- Request bodies are capped at 1 MiB.

```sh
curl -X POST --data-binary @config.json http://node:8080/config
```

### OS Updates

The OS update endpoints stage and apply an ostree/bootc update tarball:

- `POST /ostree/upload` streams the request body to `ostree.upload_path`. The
  body is written to a temporary file and renamed into place after upload.
- `POST /ostree/upgrade` runs `ostree.apply_command` and reboots the host after
  a successful apply.

Pass `?reboot=false` to `/ostree/upgrade` to apply without rebooting.
`/ostree/upgrade` returns `400` when no `apply_command` is configured.

```sh
curl --data-binary @update.tar http://node:8080/ostree/upload
curl -X POST http://node:8080/ostree/upgrade
```

## Configuration

Top-level fields:

- `podman_mode`: optional; only `rootful` is supported, and omitted uses rootful
- `folders`: optional list of host folders to create
- `interfaces`: optional list of host interface settings
- `firewall_ports`: optional list of firewalld ports to open
- `containers`: list of container definitions
- `ostree`: optional OS update settings for the `/ostree/*` endpoints

### Folders

- `path`: host folder path that must exist
- `chmod`: optional octal permissions string, for example `0755`
- `user`: optional owner as `user`, `uid`, `user:group`, or `uid:gid`

### Interfaces

Host interface changes are applied through NetworkManager with `nmstatectl`.
The generated desired state is written to `/etc/nmstate/servermaster.yml` and
applied with `nmstatectl apply`, so the configuration persists across reboots
and is reapplied by `nmstate.service`.

DNS servers from all interfaces are merged into nmstate's single global resolver
list.

- `name`: host interface name, for example `eth0`
- `ip_address`: static IP to assign to the host interface
- `subnet`: subnet CIDR for the host interface
- `gateway`: default gateway IP for the host interface
- `dns`: DNS server list for the host interface

### Firewall Ports

Firewall ports are opened through firewalld's D-Bus API. Each port is written to
both runtime configuration for immediate effect and permanent configuration for
reload and reboot persistence. An empty `zone` uses firewalld's default zone.

- `zone`: optional firewalld zone
- `port`: port or port range as a string, for example `8080` or `8000-8010`
- `protocol`: protocol; `tcp` default, supports `tcp`, `udp`, `sctp`, and `dccp`

### Ostree

- `upload_path`: where `/ostree/upload` writes the uploaded image; default
  `/data/ostree/update.tar`
- `apply_command`: argv list run by `/ostree/upgrade` to apply the staged image

Example `apply_command`:

```json
["bootc", "switch", "--transport", "oci-archive", "/data/ostree/update.tar"]
```

### Containers

- `name`: container name
- `image`: image reference to pull and run
- `user`: optional user or user/group for the container process
- `env`: key/value environment variables
- `ports`: list of published ports
- `volumes`: list of bind mounts
- `command`: optional command override
- `restart`: optional Podman restart policy

### Container Ports

- `host_ip`: host bind IP, for example `0.0.0.0`
- `host_port`: host port
- `container_port`: container port
- `protocol`: protocol; `tcp` default

### Container Volumes

- `host_path`: source path on the host
- `container_path`: target path in the container
- `read_only`: whether the bind mount is read-only
- `selinux`: optional Podman relabel option, either `z` for a shared label or
  `Z` for a private label

On SELinux-enforcing hosts, including default Red Hat Device Edge systems, an
unlabeled host path is denied to the container. Set `selinux` unless the path is
already labeled `container_file_t`.

## Example Config

See [`config.json`](config.json) for a complete example with folders, host
interface settings, firewall ports, container ports, volumes, and OS update
settings.
