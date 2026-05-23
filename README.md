# servermastef

`servermastef` is a small Podman container reconciler written in Go.
It reads a JSON config file, ensures each declared container is recreated with the desired settings, and starts everything through the Podman API socket.

## How it works

1. Reads container definitions from the configured JSON file.
2. Ensures any declared host folders exist with the configured mode and owner.
3. Applies any host interface configuration declared in the config file.
4. Opens any declared firewall ports through firewalld D-Bus.
5. Starts `podman.socket` using systemd (`rootful` or `rootless` mode).
6. Waits for the Podman Unix socket to become reachable.
7. Stops any running containers that are not declared in the config file.
8. Pulls each image.
9. Removes any existing container with the same name.
10. Creates and starts the container from the declared spec.

## Usage

```sh
servermaster -config /data/config/containers.json
```

If `-config` is omitted, it defaults to `/data/config/containers.json`.

## Configuration

Top-level fields in the JSON file:

- `podman_mode`: `rootful` (default) or `rootless`
- `folders`: optional list of host folders to create before containers start
- `interfaces`: optional list of host network interface settings
- `firewall_ports`: optional list of firewalld runtime ports to open
- `containers`: list of container definitions

### Folder object

- `path`: host folder path that must exist
- `chmod`: optional octal permissions string (for example `0755`)
- `user`: optional owner as `user`, `uid`, `user:group`, or `uid:gid`

### Interface object

Host interface changes are applied on the host with netlink and `resolvectl`, so the process needs permission to change host networking.

- `name`: host interface name (for example `eth0`)
- `ip_address`: static IP to assign to the host interface
- `subnet`: subnet CIDR for the host interface
- `gateway`: default gateway IP for the host interface
- `dns`: DNS server list for the host interface

### Firewall port object

Firewall ports are opened with `github.com/godbus/dbus/v5` against firewalld's runtime D-Bus API.

- `zone`: optional firewalld zone (empty uses the default zone)
- `port`: port or port range as a string (for example `8080` or `8000-8010`)
- `protocol`: protocol (`tcp` default; supports `tcp`, `udp`, `sctp`, and `dccp`)

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

## Example config

See `config.json` in this repository for a complete example with folders, host interface settings, firewall ports, container ports, and volumes.
