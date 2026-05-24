# Agent Notes

This project is a service that runs on a Red Hat Device Edge host.
It is used to configure the node itself, including host folders, host network interfaces, firewall ports, and the Podman containers that should be present.

This tool is installed and running on a Red Hat Device Edge edge node **without MicroShift** — there is no Kubernetes/MicroShift layer, so workloads are managed directly via Podman rather than through Kubernetes manifests.

Treat `config.json` as node configuration, not just container configuration.

## Single source of truth

This tool has full control over the node. All changes to the system must be made through it, via `config.json` — never out of band. The config file is the single source of truth for node state.

Consequently, no container may run outside this config. On every reconcile the tool stops any running container that is not declared in `config.json` (see `stopUnmanagedContainers`), because anything undeclared is, by definition, drift from the source of truth. Do not start ad-hoc or debugging containers on the node expecting them to survive — declare them in `config.json` or they will be stopped.
