// Command obsync keeps a mounted Obsidian vault continuously in step with one
// remote git branch, running as a Docker sidecar beside the editor that writes
// the vault.
//
// The design is issue #21 — body plus both comments — and section references
// (§1–§12) throughout this repo are its sections. This build recognises the
// declared surface's subcommands (§10), reports the build version, resolves the
// config surface (§8), and turns a sync loop that commits the vault and pushes
// it (#24).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/credential"
	"github.com/andyroberts2/obsync/internal/loop"
)

// version is stamped at link time with -ldflags "-X main.version=<version>",
// and is what `obsync status` reports (§10). It is deliberately not derived at
// runtime and never a knob: the version's whole job is to identify the bytes of
// the image an operator pinned (§12), so it has to come from the build.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

// run is main's body with its environment, streams and exit status returned
// rather than taken from the process, so a test can drive the same boundary a
// human does.
//
// The subcommands are the closed set of §10; there are no flags, here or
// anywhere, because obsync is a compose sidecar behind a fixed entrypoint and
// never a CLI anyone types. The environment is the whole configuration
// surface (§8).
func run(args []string, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		_, _ = fmt.Fprintln(stdout, "the sync loop keeps no status file in this build")
		return 0

	case "":
		return syncLoop(environ, stderr)

	case "credential-helper":
		// git's, not a human's: git runs it as `credential.helper` with the
		// operation it wants appended, which is why an invocation with no
		// operation is the one thing here that answers a human at all (§10).
		if len(args) != 2 {
			_, _ = fmt.Fprintln(stderr, "obsync: credential-helper is git's; git names the operation it wants")
			return 1
		}
		return credential.Helper(args[1], environ, stdin, stdout)

	case "healthcheck":
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
// in one line what it thinks it was told, and turns the sync loop until
// SIGTERM.
//
// Only one class of failure is on the exit path — a config error, decidable
// from the environment block alone (§8). Everything obsync meets after that is
// a gate or a failed run, and neither exits: obsync parks alive and keeps
// re-checking, because exiting discards backoff state and turns a diagnosable
// stuck state into a crash loop.
func syncLoop(environ []string, stderr io.Writer) int {
	// Installed before anything else, so a SIGTERM arriving during startup is
	// a clean exit rather than a kill.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	cfg, log, err := config.Resolve(environ, stderr)
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

	// The watcher is the loop's other injected dependency and it does not
	// exist yet (#39), so obsync has no wake-up but the startup run: the tick
	// that will drive the rest is #25's. A nil channel is exactly that — a
	// loop with nothing to wake it — rather than a placeholder to remove.
	l := loop.New(cfg, log, clock.System{}, nil)
	defer func() { _ = l.Close() }()

	l.Run(ctx)
	return 0
}
