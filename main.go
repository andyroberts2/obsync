// Command obsync keeps a mounted Obsidian vault continuously in step with one
// remote git branch, running as a Docker sidecar beside the editor that writes
// the vault.
//
// The design is issue #21 — body plus both comments — and section references
// (§1–§12) throughout this repo are its sections. This build recognises the
// declared surface's subcommands (§10), reports the build version, and
// resolves the config surface (§8) — and nothing syncs yet.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/andyroberts2/obsync/internal/config"
)

// version is stamped at link time with -ldflags "-X main.version=<version>",
// and is what `obsync status` reports (§10). It is deliberately not derived at
// runtime and never a knob: the version's whole job is to identify the bytes of
// the image an operator pinned (§12), so it has to come from the build.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

// run is main's body with its environment, streams and exit status returned
// rather than taken from the process, so a test can drive the same boundary a
// human does.
//
// The subcommands are the closed set of §10; there are no flags, here or
// anywhere, because obsync is a compose sidecar behind a fixed entrypoint and
// never a CLI anyone types. The environment is the whole configuration
// surface (§8).
func run(args []string, environ []string, stdout, stderr io.Writer) int {
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

	case "":
		return syncLoop(environ, stderr)

	case "healthcheck", "credential-helper":
		// Recognised, so the surface an operator meets is the one §10 declares,
		// and saying so out loud beats healthcheck's eventual silence while
		// there is no status file for it to be silent about.
		_, _ = fmt.Fprintf(stderr, "obsync: %s is not implemented in this build\n", subcommand)
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

// syncLoop is §10's default subcommand. It resolves the config surface, says
// in one line what it thinks it was told, and then parks until SIGTERM.
//
// Parking is the point rather than a placeholder: §8 puts exactly one class of
// failure on the exit path — a config error, decidable from the environment
// block alone — and everything else obsync meets is a gate, which parks alive
// and keeps re-checking. The loop that would turn between the echo and the
// signal is the tracer bullet's (#24).
func syncLoop(environ []string, stderr io.Writer) int {
	// Installed before anything else, so a SIGTERM arriving during startup is
	// a clean exit rather than a kill.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// The resolved config is dropped because there is nothing yet to hand it
	// to; Resolve has already echoed it, which is this build's whole job.
	_, log, err := config.Resolve(environ, stderr)
	if err != nil {
		var configErr *config.Error
		if !errors.As(err, &configErr) {
			log.Error("obsync cannot start", "problem", err)
			return 1
		}
		// One line per problem rather than one line carrying all of them:
		// logfmt is read a line at a time, by a human and by Loki alike.
		for _, problem := range configErr.Problems {
			log.Error("obsync cannot start", "problem", problem)
		}
		return 1
	}

	log.Warn("the sync loop is not implemented in this build; obsync has resolved its configuration " +
		"and will wait for SIGTERM")
	<-ctx.Done()
	return 0
}
