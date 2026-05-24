# servermaster

`servermaster` is a small Podman container reconciler written in Go.
It starts a small web server on port `8080`, reads a JSON config file, ensures each declared container is recreated with the desired settings, and starts everything through the Podman API socket.

## How it works

1. Starts a web server on `:8080` with `/servermaster`, the config-upload endpoint (see [Remote configuration](#remote-configuration)), and the OS-update endpoints (see [OS updates](#os-updates)).
2. Reads container definitions from the configured JSON file.
3. Ensures any declared host folders exist with the configured mode and owner.
4. Applies any host interface configuration declared in the config file.
5. Opens any declared firewall ports through firewalld D-Bus.
6. Starts the rootful `podman.socket` using systemd.
7. Waits for the Podman Unix socket to become reachable.
8. Stops any running containers that are not declared in the config file.
9. Pulls each image.
10. Removes any existing container with the same name.
11. Creates and starts the container from the declared spec.

## Usage

```sh
servermaster -config /data/config/containers.json
```

If `-config` is omitted, it defaults to `/data/config/containers.json`.

### Running as a service

A systemd unit ships in this repo as [`servermaster.service`](servermaster.service). It runs `/usr/bin/servermaster -config /data/config/containers.json` as `root` and stays running as the web server on port `8080`.

For a manual install, place the binary at `/usr/bin/servermaster` (the path the unit expects), then install and enable the unit:

```sh
make build
sudo install -m 0755 dist/servermaster /usr/bin/servermaster
sudo install -m 0644 servermaster.service /etc/systemd/system/servermaster.service
sudo systemctl daemon-reload
sudo systemctl enable --now servermaster.service
```

On image-mode hosts (for example Red Hat Device Edge built from a blueprint), don't install at runtime — build the RPM (below), bake it into the image, and enable the unit declaratively:

```toml
[customizations.services]
enabled = ["servermaster.service"]
```

### Building an RPM

`make rpm` builds a static (CGO-free) binary and packages it with the systemd unit and license into an RPM under `dist/`. Packaging uses the pure-Go [`cmd/mkrpm`](cmd/mkrpm), so the build host needs **no** `rpmbuild` or spec file — only the Go toolchain. The RPM installs `servermaster` to `/usr/bin` and the unit to `/usr/lib/systemd/system`, declares `Requires: podman, nmstate` (`Recommends: firewalld`), and reloads/restarts the service through systemd scriptlets.

```sh
make rpm                       # version from the latest git tag, or 0.0.0 if untagged
make rpm VERSION=1.2.3         # explicit version
make rpm GOARCH=arm64          # cross-build for aarch64 edge devices
```

Tagged releases on GitHub attach prebuilt `x86_64` and `aarch64` RPMs, so for a release you can download the package directly instead of building it.

Add the resulting package to an image builder blueprint (`[[packages]]` from a custom repo) and enable the service as shown above.

## Status

`GET /servermaster` returns a pretty-printed JSON status document. It includes the running ostree/bootc version, free disk space, running Podman containers with image/version metadata, and the last 100 log lines from each running container.

```sh
curl http://node:8080/servermaster
```

## Remote configuration

`POST /config` (or `PUT`) accepts a raw `config.json` document, validates it, writes it atomically to the active config path (the `-config` path), and immediately converges the host to it — the same folders / host interfaces / firewall ports / containers reconcile that runs at startup. The uploaded body is written verbatim, so a successful upload becomes the new source of truth.

- A malformed or invalid config is rejected with `400` and is **not** written to disk or applied.
- A valid config that fails to apply (for example firewalld is down) is still saved; the response is `500` describing the failure, and the next reconcile or service restart retries it.
- The body is capped at 1 MiB.

```sh
curl -X POST --data-binary @config.json http://node:8080/config
```

> **Security:** like the `/ostree/*` endpoints, `/config` is unauthenticated. Anyone who can reach `:8080` can rewrite the node's folders, host interfaces, firewall ports, and containers. Only expose this port on a trusted, isolated management network.

## OS updates

The web server exposes two endpoints for staging and applying an OS image (an ostree/bootc update tarball):

- `POST /ostree/upload` streams the request body to `ostree.upload_path`. The body is written to a temporary file and renamed into place, so an interrupted upload is never left where the apply command would pick it up.
- `POST /ostree/upgrade` runs `ostree.apply_command` and then reboots the host once it succeeds. Pass `?reboot=false` to apply without rebooting (useful for testing). Returns `400` if no `apply_command` is configured.

```sh
# Stage the image, then apply it and reboot.
curl --data-binary @update.tar http://node:8080/ostree/upload
curl -X POST http://node:8080/ostree/upgrade
```

> **Security:** these endpoints are unauthenticated. Anyone who can reach `:8080` can replace the OS image and reboot the host. Only expose this port on a trusted, isolated management network (for example, restrict it with `firewall_ports`/`interfaces` or a host firewall).

## Configuration

Top-level fields in the JSON file:

- `podman_mode`: optional; only `rootful` is supported, and omitting it uses rootful mode
- `folders`: optional list of host folders to create before containers start
- `interfaces`: optional list of host network interface settings
- `firewall_ports`: optional list of firewalld runtime ports to open
- `containers`: list of container definitions
- `ostree`: optional OS-update settings used by the `/ostree/*` endpoints

### Folder object

- `path`: host folder path that must exist
- `chmod`: optional octal permissions string (for example `0755`)
- `user`: optional owner as `user`, `uid`, `user:group`, or `uid:gid`

### Interface object

Host interface changes are applied declaratively through NetworkManager with `nmstatectl` (the `nmstate` package must be installed on the host). The generated desired state is written to `/etc/nmstate/servermaster.yml` and applied with `nmstatectl apply`, so the configuration persists across reboots and is reapplied by `nmstate.service`. DNS servers from all interfaces are merged into nmstate's single global resolver list. This replaces the older netlink/`resolvectl` path, which fought NetworkManager and required `systemd-resolved` (not enabled by default on RHEL/Red Hat Device Edge).

- `name`: host interface name (for example `eth0`)
- `ip_address`: static IP to assign to the host interface
- `subnet`: subnet CIDR for the host interface
- `gateway`: default gateway IP for the host interface
- `dns`: DNS server list for the host interface

### Firewall port object

Firewall ports are opened with `github.com/godbus/dbus/v5` against firewalld's D-Bus API. Each port is written to both the runtime configuration (immediate effect, no reload) and the permanent configuration (survives `firewall-cmd --reload` and reboot). An empty `zone` resolves to firewalld's default zone.

- `zone`: optional firewalld zone (empty uses the default zone)
- `port`: port or port range as a string (for example `8080` or `8000-8010`)
- `protocol`: protocol (`tcp` default; supports `tcp`, `udp`, `sctp`, and `dccp`)

### Ostree object

- `upload_path`: where `/ostree/upload` writes the uploaded image (default `/data/ostree/update.tar`)
- `apply_command`: argv list run by `/ostree/upgrade` to apply the staged image (for example `["bootc", "switch", "--transport", "oci-archive", "/data/ostree/update.tar"]`)

### Container fields

- `name`: container name
- `image`: image reference to pull and run
- `user`: optional user or user/group for the container process (for example `1000`, `1000:1000`, or `nginx`)
- `env`: key/value environment variables
- `ports`: list of published ports
- `volumes`: list of bind mounts
- `command`: optional command override
- `restart`: optional Podman restart policy

### Port object

- `host_ip`: host bind IP (for example `0.0.0.0`)
- `host_port`: host port
- `container_port`: container port
- `protocol`: protocol (`tcp` default)

### Volume object

- `host_path`: source path on host
- `container_path`: target path in container
- `read_only`: whether mount is read-only
- `selinux`: optional Podman relabel option for the bind mount — `z` (shared label) or `Z` (private label). On SELinux-enforcing hosts (Red Hat Device Edge defaults to enforcing) an unlabeled host path is denied to the container, so set this unless the path is already labeled `container_file_t`.

## Example config

See `config.json` in this repository for a complete example with folders, host interface settings, firewall ports, container ports, and volumes.
