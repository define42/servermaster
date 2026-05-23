# servermastef

Containers can optionally define `interfaces` in `config.json` to set per-interface network values:

- `name`: interface name inside container (for example `eth0`)
- `network`: Podman network name (defaults to `podman` when omitted)
- `ip_address`: static IP for the interface
- `subnet`: subnet CIDR
- `gateway`: gateway IP
- `dns`: DNS server list for the container