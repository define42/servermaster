// Command mkrpm packages the prebuilt servermaster binary together with its
// systemd unit and license into an RPM using the pure-Go
// github.com/google/rpmpack. No rpmbuild, spec file, or Go toolchain is needed
// in a buildroot, so the package can be produced on any build host. It is
// invoked by the makefile `rpm` target after `make build`.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/google/rpmpack"
)

const (
	packageName     = "servermaster"
	summary         = "Podman container reconciler and node configuration service"
	description     = "servermaster reads a JSON node configuration and reconciles host folders, host network interfaces (through nmstate), firewalld ports, and the Podman containers that should be present. It also serves /servermaster status and the /ostree OS-update endpoints on port 8080."
	url             = "https://github.com/define42/servermastef"
	licence         = "Apache-2.0"
	unitDestination = "/usr/lib/systemd/system/servermaster.service"
	licenseDest     = "/usr/share/licenses/servermaster/LICENSE"
)

// The service runs as root, so no dedicated user is created. These scriptlets
// mirror the systemd rpm macros: reload on install, disable on final removal,
// and restart on upgrade. $1 is the count of package instances that will remain
// after the transaction (0 = the package is being removed entirely).
const postinScript = `if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || :
fi
`

const preunScript = `if [ "$1" = 0 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl --no-reload disable --now servermaster.service || :
fi
`

const postunScript = `if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || :
    if [ "$1" -ge 1 ]; then
        systemctl try-restart servermaster.service || :
    fi
fi
`

type options struct {
	version    string
	release    string
	arch       string
	binarySrc  string
	binaryDest string
	unitSrc    string
	licenseSrc string
	out        string
}

func main() {
	o := options{}
	flag.StringVar(&o.version, "version", "0.0.0", "package version")
	flag.StringVar(&o.release, "release", "1", "package release")
	flag.StringVar(&o.arch, "arch", rpmArch(runtime.GOARCH), "package architecture")
	flag.StringVar(&o.binarySrc, "binary", "dist/servermaster", "path to the prebuilt binary")
	flag.StringVar(&o.binaryDest, "binary-dest", "/usr/bin/servermaster", "install path for the binary")
	flag.StringVar(&o.unitSrc, "unit", "servermaster.service", "path to the systemd unit file")
	flag.StringVar(&o.licenseSrc, "license", "LICENSE", "path to the license file")
	flag.StringVar(&o.out, "out", "", "output rpm path (default dist/<name>-<version>-<release>.<arch>.rpm)")
	flag.Parse()

	if o.out == "" {
		o.out = fmt.Sprintf("dist/%s-%s-%s.%s.rpm", packageName, o.version, o.release, o.arch)
	}

	if err := writeRPM(o); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n", o.out)
}

// rpmArch maps a Go GOARCH value to the matching RPM architecture tag.
func rpmArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

func writeRPM(o options) error {
	var requires rpmpack.Relations
	for _, dep := range []string{"podman", "nmstate"} {
		if err := requires.Set(dep); err != nil {
			return fmt.Errorf("add require %q: %w", dep, err)
		}
	}
	var recommends rpmpack.Relations
	if err := recommends.Set("firewalld"); err != nil {
		return fmt.Errorf("add recommend %q: %w", "firewalld", err)
	}

	rpm, err := rpmpack.NewRPM(rpmpack.RPMMetaData{
		Name:        packageName,
		Summary:     summary,
		Description: description,
		Version:     o.version,
		Release:     o.release,
		Arch:        o.arch,
		URL:         url,
		Licence:     licence,
		Requires:    requires,
		Recommends:  recommends,
	})
	if err != nil {
		return err
	}

	st, err := os.Stat(o.binarySrc)
	if err != nil {
		return err
	}
	mtime := uint32(st.ModTime().Unix())

	files := []struct {
		src  string
		dest string
		mode uint
	}{
		{o.binarySrc, o.binaryDest, 0o755},
		{o.unitSrc, unitDestination, 0o644},
		{o.licenseSrc, licenseDest, 0o644},
	}
	for _, f := range files {
		body, err := os.ReadFile(f.src)
		if err != nil {
			return err
		}
		rpm.AddFile(rpmpack.RPMFile{
			Name:  f.dest,
			Body:  body,
			Mode:  f.mode,
			Owner: "root",
			Group: "root",
			MTime: mtime,
			Type:  rpmpack.GenericFile,
		})
	}

	rpm.AddPostin(postinScript)
	rpm.AddPreun(preunScript)
	rpm.AddPostun(postunScript)

	dst, err := os.Create(o.out)
	if err != nil {
		return err
	}
	if err := rpm.Write(dst); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}
