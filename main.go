// Command servermaster reconciles a Red Hat Device Edge node to a JSON
// configuration: it manages host folders and files, host network interfaces
// (through nmstate), firewalld ports, and the Podman containers that should be
// present, treating config.json as the single source of truth for node state.
// It also serves a status endpoint and the ostree OS-update endpoints on :8080.
package main

import (
	"flag"
	"log"
)

const (
	defaultConfigPath = "/etc/servermaster.json"
)

func main() {
	captureServiceLog()

	configPath := flag.String("config", defaultConfigPath, "path to config JSON file")
	listenAddress := flag.String("listen", defaultListenAddress, `web server listen address: a host:port for TCP (":8080" for all interfaces, "127.0.0.1:8080" for loopback only) or a Unix socket path ("unix:///run/servermaster/servermaster.sock")`)
	flag.Parse()

	if err := runServiceFunc(*listenAddress, *configPath); err != nil {
		log.Fatal(err)
	}
}
