# servermastef

`servermastef` is a small Podman container reconciler written in Go.
It reads a JSON config file, ensures each declared container is recreated with the desired settings, and starts everything through the Podman API socket.

## How it works

1. Reads container definitions from `/data/config/containers.json`.
2. Starts `podman.socket` using systemd (`rootful` or `rootless` mode).
3. Waits for the Podman Unix socket to become reachable.
4. Pulls each image.
5. Removes any existing container with the same name.
6. Creates and starts the container from the declared spec.

## Configuration

Top-level fields in the JSON file:

- `podman_mode`: `rootful` (default) or `rootless`
- `containers`: list of container definitions

### Container fields

- `name`: container name
- `image`: image reference to pull and run
- `env`: key/value environment variables
- `ports`: list of published ports
- `volumes`: list of bind mounts
- `interfaces`: optional list of per-network interface settings
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

### Interface object

- `name`: interface name inside container (for example `eth0`)
- `network`: Podman network name (defaults to `podman` when omitted)
- `ip_address`: static IP for the interface
- `subnet`: subnet CIDR
- `gateway`: gateway IP
- `dns`: DNS server list for the container

## Example config

See `config.json` in this repository for a complete example with ports, volumes, and network interface settings.
