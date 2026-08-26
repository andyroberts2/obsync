// Command obsync keeps a mounted Obsidian vault continuously in step with one
// remote git branch, running as a Docker sidecar beside the editor that writes
// the vault.
//
// The design is issue #21 — body plus both comments — and section references
// (§1–§12) throughout this repo are its sections. This build is the project
// skeleton: the declared surface's subcommands (§10) are recognised and the
// build version is reported, and nothing syncs yet.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is stamped at link time with -ldflags "-X main.version=<version>",
// and is what `obsync status` reports (§10). It is deliberately not derived at
// runtime and never a knob: the version's whole job is to identify the bytes of
// the image an operator pinned (§12), so it has to come from the build.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's body with its streams and exit status returned rather than
// taken from the process, so a test can drive the same boundary a human does.
//
// The subcommands are the closed set of §10; there are no flags, here or
// anywhere, because obsync is a compose sidecar behind a fixed entrypoint and
// never a CLI anyone types (§8).
func run(args []string, stdout, stderr io.Writer) int {
	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}

	switch subcommand {
	case "status":
		// §10: human-readable to stdout, exit 0 always, includes the build
		// version. It gains the rest of its report when the status file exists.
		//
		// The write errors are dropped explicitly rather than by omission: a
		// failed write to stdout has nowhere left to be reported.
		_, _ = fmt.Fprintf(stdout, "obsync %s\n", version)
		_, _ = fmt.Fprintln(stdout, "the sync loop is not implemented in this build")
		return 0

	case "", "healthcheck", "credential-helper":
		// Recognised, so the surface an operator meets is the one §10 declares,
		// and saying so out loud beats healthcheck's eventual silence while
		// there is no status file for it to be silent about.
		name := subcommand
		if name == "" {
			name = "the sync loop"
		}
		_, _ = fmt.Fprintf(stderr, "obsync: %s is not implemented in this build\n", name)
		return 1

	default:
		_, _ = fmt.Fprintf(stderr, "obsync: unknown subcommand %q\n", subcommand)
		usage(stderr)
		return 1
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage:
  obsync                     run the sync loop until SIGTERM
  obsync healthcheck         silent; exit 0 healthy, 1 unhealthy
  obsync status              report to stdout; exit 0 always
  obsync credential-helper   git's credential-helper protocol

obsync is configured entirely by OBSYNC_* environment variables.
`)
}
