# edgecommander
[![codecov](https://codecov.io/gh/define42/edgecommander/graph/badge.svg?token=CI1DDQT3O4)](https://codecov.io/gh/define42/edgecommander)
`edgecommander` is a Go service for declaratively configuring a Red Hat Device
Edge node without MicroShift. It manages host folders, host files, host network
interfaces, firewalld ports, OS update staging, and Podman containers directly
from one JSON configuration file.

The active config file is the source of truth for the node. On every reconcile,
`edgecommander` stops any running container that is not declared in that config.
Do not start ad-hoc containers and expect them to survive; add them to the
config instead.

## What It Does

On startup, and after a successful remote config upload, `edgecommander`:

1. Starts the HTTP API on the `-listen` address (a Unix socket by default; see
   [Listen Address](#listen-address)).
2. Reads the active JSON config file.
3. Creates declared host folders with the requested mode and owner.
4. Writes declared files (creating parent directories) with the requested mode
   and owner.
5. Applies declared host interface settings through `nmstatectl`.
6. Starts `firewalld.service`, opens declared firewalld ports (and optional
   source-restricted rich rules) in runtime and permanent configuration, then
   closes any undeclared port/rich rule and removes every firewalld service.
7. Starts the rootful `podman.socket` through systemd.
8. Waits for the Podman Unix socket to become reachable.
9. Stops running containers that are not declared in the config.
10. For each declared container, leaves it running untouched if it is already
    running with an unchanged configuration; otherwise (re)creates and starts it
    from the desired spec, pulling the image only when it is not already present
    in local storage.

## Usage

```sh
edgecommander -config /etc/edgecommander.json
```

If `-config` is omitted, it defaults to `/etc/edgecommander.json`.
The binary has no `install` subcommand or flag; it always starts the service
process.

### Validating a Config

`-validatefile` runs the same validation the service applies before converging
the host, but against the named file only — it parses the JSON, checks every
field, and exits without starting the web server or changing the host:

```sh
edgecommander -validatefile config.json
```

It prints `config config.json is valid` and exits `0` when the file parses and
every field validates, or prints the first error to stderr and exits `1`
otherwise. Use it to check a `config.json` before deploying it.

### Listen Address

`-listen` controls where the HTTP API binds (default
`unix:///run/edgecommander/edgecommander.sock`):

- `:8080` — TCP on all interfaces.
- `127.0.0.1:8080` — TCP on loopback only, so the API is reachable only from
  the node itself (for example behind an SSH tunnel or a local reverse proxy).
- `unix:///run/edgecommander/edgecommander.sock` — a Unix-domain socket. The
  parent directory is created, a stale socket from an earlier run is cleared,
  and the socket is mode `0660` (not world-accessible). Use the socket's owning
  group to grant access. A relative path resolves against the process working
  directory, so prefer an absolute one.

Because the API is unauthenticated and root-equivalent (see [HTTP API](#http-api)),
it defaults to a Unix socket, which keeps it off the network entirely: running the
binary without an explicit `-listen` is safe no matter how it is launched, not just
under the shipped systemd unit (which passes the same socket path). Set a `host:port`
only to expose the API on a trusted management network, and prefer loopback
(`127.0.0.1:8080`) over all interfaces (`:8080`) when you do.

```sh
edgecommander -config /etc/edgecommander.json -listen 127.0.0.1:8080
edgecommander -config /etc/edgecommander.json -listen unix:///run/edgecommander/edgecommander.sock
```

Reach a Unix-socket listener with `curl --unix-socket` (the host in the URL is
ignored):

```sh
curl --unix-socket /run/edgecommander/edgecommander.sock http://localhost/edgecommander/status
```

## Installation

### RPM

`make rpm` builds a static, CGO-free binary and packages it with the systemd unit
and license into `dist/`. Packaging uses the pure-Go [`cmd/mkrpm`](cmd/mkrpm),
so the build host needs only the Go toolchain.

The RPM installs:

- `edgecommander` to `/usr/bin/edgecommander`
- `edgecommander.service` to `/usr/lib/systemd/system/edgecommander.service`

It declares `Requires: podman, nmstate`, `Recommends: firewalld`, and uses
systemd scriptlets to reload and restart the service when appropriate.

```sh
make rpm                       # version from the latest git tag, or 0.0.0 if untagged
make rpm VERSION=1.2.3         # explicit version
make rpm GOARCH=arm64          # cross-build for aarch64 edge devices
```

Tagged GitHub releases attach prebuilt `x86_64` and `aarch64` RPMs.

## HTTP API

All HTTP endpoints live under `/edgecommander/*` and are unauthenticated. Anyone
who can reach the listener can rewrite the node's config, run the ostree apply
command, and reboot the host — that is, it is root-equivalent. Expose it only on
a trusted management network, or restrict the listener with `-listen` (loopback
or a Unix socket; see [Listen Address](#listen-address)).

The `curl http://node:8080/...` examples below assume a TCP `-listen`. With the
default Unix socket, reach the same endpoints with
`curl --unix-socket /run/edgecommander/edgecommander.sock http://localhost/...`.

### Health

`GET /edgecommander/health` returns a plain-text liveness response.

### Status

`GET /edgecommander/status` returns a pretty-printed JSON status document with:

- the node's current `hostname`
- `uptime`: how long the host has been running, as whole `seconds` plus a
  `human` rendering (e.g. `1d 2h 3m 4s`), read from `/proc/uptime`
- ostree/bootc deployment information
- free disk space, one entry per real disk-backed filesystem (tmpfs and other
  virtual filesystems are excluded; a filesystem mounted at several paths is
  reported once)
- `memory`: total, free, available, and used RAM (bytes) plus used percent
- `cpu`: core count and current utilization percent (sampled briefly from
  `/proc/stat`)
- `inventory`: hardware inventory decoded from DMI/SMBIOS — service tag, BIOS
  firmware, baseboard, and populated memory modules. Reading DMI requires root
  and a host that exposes SMBIOS; where it is unavailable the field is omitted
  and the reason is recorded in `errors`
- live network configuration for every host interface (read from the kernel via
  netlink: addresses, routes, link state, link speed (Mbit/s, from sysfs),
  transmit queue length, and RX/TX counters — packets, bytes, errors, dropped,
  overruns, frame, carrier, and collisions — for all interfaces, not just managed
  ones, plus resolver nameservers from `/etc/resolv.conf`)
- running Podman containers
- image and version metadata where available
- the last 100 log lines from each running container
- `edgecommander_log`: the last 100 log lines from the `edgecommander` service
  itself (its own `log` output, also sent to stderr/journald)
- status collection errors, if any

```sh
curl http://node:8080/edgecommander/status
```

### Remote Configuration

`POST /edgecommander/config` and `PUT /edgecommander/config` accept a raw config
JSON document. The service validates the body, writes it atomically to the active
`-config` path, and immediately reconciles the node to the new desired state.

- Malformed or invalid config is rejected with `400` and is not written.
- Valid config is saved before apply. If apply fails, the response is `500` and
  the next reconcile or service restart retries it.
- Request bodies are capped at 1 MiB.

```sh
curl -X POST --data-binary @config.json http://node:8080/edgecommander/config
```

### Restart

`POST /edgecommander/restart` schedules a host reboot. The response is written
before the reboot is triggered.

```sh
curl -X POST http://node:8080/edgecommander/restart
```

### OS Updates

The OS update endpoints stage and apply an ostree/bootc update tarball:

- `POST /edgecommander/ostree/upload` streams the request body to
  `ostree.upload_path`. The body is written to a temporary file and renamed into
  place after upload.
- `POST /edgecommander/ostree/upgrade` runs `ostree.apply_command` and reboots
  the host after a successful apply. When `apply_command` is not configured it
  defaults to a bootc image-mode switch onto the upload path, so no `ostree`
  config is required on a standard bootc node.

Pass `?reboot=false` to `/edgecommander/ostree/upgrade` to apply without
rebooting.

```sh
curl --data-binary @update.tar http://node:8080/edgecommander/ostree/upload
curl -X POST http://node:8080/edgecommander/ostree/upgrade
```

## Configuration

Top-level fields:

- `podman_mode`: optional; only `rootful` is supported, and omitted uses rootful
- `hostname`: optional static hostname for the node (see [Hostname](#hostname))
- `folders`: optional list of host folders to create
- `interfaces`: optional list of host interface settings
- `routes`: optional list of static routes (default routes and routing tables)
- `firewall_ports`: optional list of firewalld ports to open
- `containers`: list of container definitions
- `ostree`: optional OS update settings for the `/edgecommander/ostree/*` endpoints

### Hostname

`hostname` sets the node's static hostname via `hostnamectl set-hostname`, which
writes `/etc/hostname` and updates the running hostname through
systemd-hostnamed, so the change persists across reboots. It must be a valid
RFC 1123 hostname: dot-separated labels of letters, digits, and hyphens, each
1-63 characters and not starting or ending with a hyphen, up to 253 characters
total. A full FQDN such as `node1.example.com` is accepted and becomes the
static hostname (the Red Hat-native convention); note that `hostname -f` and
reverse lookups still resolve via DNS or `/etc/hosts`, which `hostnamectl` does
not modify. Reconcile only invokes `hostnamectl` when the declared name differs
from the running one. Omitting `hostname` (or leaving it empty) leaves the
host's hostname unmanaged. The current hostname is also reported in
`/edgecommander/status`.

### Folders

- `path`: host folder path that must exist
- `chmod`: optional octal permissions string, for example `0755`
- `user`: optional owner as `user`, `uid`, `user:group`, or `uid:gid`

### Files

Declared files are written on every reconcile, after folders, with their parent
directories created as needed. Content comes from the config itself.

- `path`: host file path to write
- `content`: file contents
- `encoding`: how `content` is interpreted — `plain` (default) for literal text,
  or `base64` for base64-encoded bytes (use this for binary content)
- `chmod`: optional octal permissions string, for example `0755` (default `0644`)
- `user`: optional owner as `user`, `uid`, `user:group`, or `uid:gid`

```json
"files": [
  {
    "path": "/var/data/web/hello",
    "chmod": "0644",
    "user": "0:0",
    "content": "Hello, world!\n",
    "encoding": "plain"
  }
]
```

### Interfaces

Host interface changes are applied through NetworkManager with `nmstatectl`.
The generated desired state is written to `/etc/nmstate/edgecommander.yml` and
applied with `nmstatectl apply`, so the configuration persists across reboots
and is reapplied by `nmstate.service`. The apply is bounded by a timeout: an
interface that cannot reach its desired state fails the reconcile (with the
`nmstatectl` error) rather than blocking the service.

The named interface must be one NetworkManager manages (check `nmcli device
status`); externally-created `unmanaged` devices cannot be configured this way.

DNS servers from all interfaces are merged into nmstate's single global resolver
list.

- `name`: host interface name, for example `eth0` (or `eth0.100` for a VLAN)
- `type`: nmstate interface type; defaults to `ethernet`. Set `dummy` for a
  software test interface, `vlan` for an 802.1Q tagged interface, or `bond` for
  a Linux bonding (link-aggregation) interface. The value is passed to nmstate,
  which validates it; bridges need extra parameters and are not supported here.
- `ip_address`: static IP to assign to the host interface
- `subnet`: subnet CIDR for the host interface
- `gateway`: default gateway IP for the host interface
- `dns`: DNS server list for the host interface
- `mtu`: optional interface maximum transmission unit. The generic accepted
  range is `1`–`4294967295`; the interface's actual device-specific limits are
  enforced by nmstate/NetworkManager when applied. Omitting it leaves the MTU
  untouched.
- `ipv4_method`: optional IPv4 addressing mode for interfaces without a static
  IPv4 `ip_address`, mirroring NetworkManager's `ipv4.method`:
  - `dhcp` (alias `auto`): lease an IPv4 address over DHCP
  - `disabled`: turn IPv4 off

  It is mutually exclusive with a static IPv4 `ip_address` (which is the
  `manual` method), and independent of IPv6, so it can be paired with a static
  IPv6 `ip_address` or an `ipv6_method`.
- `ipv6_method`: optional IPv6 addressing mode for interfaces without a static
  IPv6 `ip_address`, mirroring NetworkManager's `ipv6.method`:
  - `link-local`: enable IPv6 with only the auto-generated link-local address
    (no DHCPv6, no SLAAC global address)
  - `auto`: SLAAC plus DHCPv6 (the router-advertisement default)
  - `dhcp`: DHCPv6 only, without SLAAC
  - `disabled`: turn IPv6 off

  It is mutually exclusive with a static IPv6 `ip_address` (which is the
  `manual` method). It does not touch IPv4, so an IPv4 `ip_address` can be set
  alongside it.
- `ipv6_addr_gen_mode`: optional IPv6 interface-identifier generation mode,
  mirroring `ipv6.addr-gen-mode`: `eui64` or `stable-privacy`. Requires IPv6 to
  be enabled, via `ipv6_method` or a static IPv6 `ip_address`.
- `txqueuelen`: optional transmit queue length (`0`–`4294967295`). nmstate has no
  field for it, so it is applied via netlink after the nmstate apply and
  reapplied on each reconcile; omitting it leaves the interface's queue length
  untouched
- `vlan`: required when `type` is `vlan`; the VLAN settings:
  - `base_interface`: the interface the VLAN rides on, for example `eth0`
  - `id`: the 802.1Q VLAN tag, `1`–`4094`
- `bond`: required when `type` is `bond`; the bond (link-aggregation) settings:
  - `mode`: bonding mode — one of `balance-rr`, `active-backup`, `balance-xor`,
    `broadcast`, `802.3ad` (LACP), `balance-tlb`, or `balance-alb`
  - `ports`: list of member interface names enslaved to the bond (at least one;
    duplicates and the bond's own name are rejected). Members listed here are
    enslaved by nmstate; they do not need their own entry under `interfaces`.
  - `miimon`: optional MII link-monitoring interval in milliseconds
    (`0`–`4294967295`). Omitting it leaves it untouched.
  - `primary`: optional preferred member for failover-style modes
    (`active-backup`, `balance-tlb`, `balance-alb`); must be one of `ports`.

A VLAN interface tags traffic on its `base_interface`; that base interface must
exist and be NetworkManager-managed.

```json
{
  "name": "eth0.100",
  "type": "vlan",
  "ip_address": "192.168.100.10",
  "subnet": "192.168.100.0/24",
  "vlan": { "base_interface": "eth0", "id": 100 }
}
```

A two-NIC active/backup bond with MII link monitoring — the declarative
equivalent of creating a `bond0` connection in NetworkManager with `eth1` and
`eth2` enslaved as members, `eth1` preferred as the active link:

```json
{
  "name": "bond0",
  "type": "bond",
  "ip_address": "192.168.1.50",
  "subnet": "192.168.1.0/24",
  "bond": {
    "mode": "active-backup",
    "ports": ["eth1", "eth2"],
    "miimon": 100,
    "primary": "eth1"
  }
}
```

An IPv6 link-local-only interface with EUI-64 addressing — the declarative
equivalent of `nmcli connection modify ens1f1np1 ipv6.method link-local
ipv6.addr-gen-mode eui64 connection.autoconnect yes` (autoconnect comes for
free, since nmstate writes a persisted profile that comes up on boot):

```json
{
  "name": "ens1f1np1",
  "ipv6_method": "link-local",
  "ipv6_addr_gen_mode": "eui64"
}
```

An interface that leases its IPv4 address over DHCP:

```json
{
  "name": "eth0",
  "ipv4_method": "dhcp"
}
```

### Routes

Static routes are applied through NetworkManager with `nmstatectl`, in the same
desired-state document as the interfaces (`/etc/nmstate/edgecommander.yml`), so
they persist across reboots and are reapplied by `nmstate.service`. Routes
declared here are **additive** to the per-interface default route derived from an
interface's `gateway`: use `gateway` for a simple default route, and `routes` for
additional static routes, default routes on a non-main table, or routes that need
an explicit metric.

Each route is applied to the interface named in `next_hop_interface`; that
interface need not be declared under `interfaces` (it may be DHCP-managed or
pre-existing), but it must exist and be NetworkManager-managed.

- `name`: optional human-readable label for the route. nmstate has no per-route
  name, so it is not applied to the kernel; it documents the route and is used to
  identify it in validation error messages.
- `destination`: target network in CIDR form — `0.0.0.0/0` or `::/0` for a
  default route, or a specific network such as `10.0.0.0/8`. Host bits are
  normalized to the network address.
- `next_hop_interface`: the egress interface for the route (required)
- `next_hop_address`: optional gateway the route forwards through; omit it for an
  on-link route reached directly over `next_hop_interface`. When set it must be
  the same IP family as `destination`.
- `table_id`: optional kernel routing table to install the route in
  (`0`–`4294967295`), mirroring nmstate's route `table-id`. Omitting it (or `0`)
  uses the main table (254). A non-main table is only consulted when a routing
  policy rule (`ip rule`) directs traffic to it; declaring such rules is out of
  scope here.
- `metric`: optional route priority (`0`–`4294967295`); lower wins among routes
  to the same destination. Omitting it lets nmstate and the kernel pick the
  default.

A specific static route through a gateway, plus a default route placed in routing
table 100 with a metric:

```json
"routes": [
  {
    "name": "corp-net",
    "destination": "10.0.0.0/8",
    "next_hop_interface": "eth0",
    "next_hop_address": "192.168.1.1"
  },
  {
    "name": "vlan100-default",
    "destination": "0.0.0.0/0",
    "next_hop_interface": "eth0.100",
    "next_hop_address": "192.168.100.1",
    "table_id": 100,
    "metric": 100
  }
]
```

### Firewall Ports

Firewall ports are opened through firewalld's D-Bus API. Each declaration is
written to both runtime configuration for immediate effect and permanent
configuration for reload and reboot persistence. An empty `zone` uses
firewalld's default zone. `edgecommander` starts `firewalld.service` first
(firewalld is not D-Bus activatable on a default install), so a stopped
firewalld is brought up automatically rather than failing the apply.

`firewall_ports` is the single source of truth for the entire firewall surface:
on every reconcile `edgecommander` closes any port open in firewalld — in any
zone, in both runtime and permanent config — that is not declared here, and
**removes every firewalld service** (for example `ssh`, `cockpit`, `dhcpv6-client`),
just as it stops undeclared containers. Access is expressed as ports (optionally
bound to a remote source), so an empty (or omitted) `firewall_ports` closes
everything.

> **Warning — SSH lockout.** The default zone allows SSH through the `ssh`
> *service*, not a port. Because services are stripped, SSH survives a reconcile
> only if you declare its port explicitly, for example
> `{ "zone": "public", "port": "22", "protocol": "tcp" }`. A config that omits
> `22/tcp` will lock you out of remote access on the next reconcile.

firewalld is an optional (`Recommends`) dependency. If it cannot be started and
no ports are declared, the firewall step is skipped (nothing to enforce); if
ports are declared it is an error, since the config cannot be satisfied.

- `zone`: optional firewalld zone
- `port`: port or port range as a string, for example `8080` or `8000-8010`
- `protocol`: protocol; `tcp` default, supports `tcp`, `udp`, `sctp`, and `dccp`
- `source`: optional remote source IP or CIDR (for example `10.0.0.10` or
  `10.0.0.0/24`); when set, edgecommander uses a firewalld rich rule so only
  traffic from that source can reach the port

### Ostree

The whole `ostree` block is optional; omit it and the defaults below apply, so a
standard bootc (image-mode) Device Edge node needs no ostree config.

- `upload_path`: where `/edgecommander/ostree/upload` writes the uploaded image;
  defaults to `/var/data/ostree/update.tar`
- `apply_command`: argv list run by `/edgecommander/ostree/upgrade` to apply the
  staged image; defaults to a bootc image-mode switch onto the upload path:
  `["bootc", "switch", "--transport", "oci-archive", "<upload_path>"]`. Override
  this on non-bootc (rpm-ostree) nodes.

### Containers

A declared container is recreated only when its configuration changes.
`edgecommander` records a hash of each container's config in the
`edgecommander.config-hash` label; on reconcile, a container already running with
a matching hash is left untouched — not stopped, re-pulled, or recreated. A
container is (re)created when it is missing, stopped, its config changed, or it
predates the label.

When a container is (re)created, the image is pulled **only if it is not already
present in local storage** — an image that exists locally is never re-pulled. A
moved tag (for example `:latest`) is therefore not picked up while a matching
image is cached; change the `image` reference (a new tag or digest) to pull an
updated image.

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
interface settings, routes, firewall ports, container ports, volumes, and OS
update settings.
