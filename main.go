// Command servermaster reconciles a Red Hat Device Edge node to a JSON
// configuration: it manages host folders and files, host network interfaces
// (through nmstate), firewalld ports, and the Podman containers that should be
// present, treating config.json as the single source of truth for node state.
// It also serves a status endpoint and the ostree OS-update endpoints on :8080.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

const (
	defaultConfigPath = "/etc/servermaster.json"
)

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to config JSON file")
	validateFilePath := flag.String("validatefile", "", "validate the given config JSON file and exit, without starting the service or changing the host")
	listenAddress := flag.String("listen", defaultListenAddress, `web server listen address: a host:port for TCP (":8080" for all interfaces, "127.0.0.1:8080" for loopback only) or a Unix socket path ("unix:///run/servermaster/servermaster.sock")`)
	flag.Parse()

	// -validatefile is a one-shot check: load and validate the named config, then
	// exit. It must not start the web server or converge the host, so it runs
	// before captureServiceLog and writes its result straight to stdout/stderr.
	if *validateFilePath != "" {
		if err := validateConfigFile(*validateFilePath); err != nil {
			fmt.Fprintf(os.Stderr, "config %s is invalid: %v\n", *validateFilePath, err)
			os.Exit(1)
		}
		fmt.Printf("config %s is valid\n", *validateFilePath)
		return
	}

	captureServiceLog()

	if err := runServiceFunc(*listenAddress, *configPath); err != nil {
		log.Fatal(err)
	}
}
