# Agent Notes

This project is a service that runs on a Red Hat Device Edge host.
It is used to configure the node itself, including host folders, host network interfaces, firewall ports, and the Podman containers that should be present.

This tool is installed and running on a Red Hat Device Edge edge node **without MicroShift** — there is no Kubernetes/MicroShift layer, so workloads are managed directly via Podman rather than through Kubernetes manifests.

Treat `config.json` as node configuration, not just container configuration.
