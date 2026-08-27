// Command obsync keeps a mounted Obsidian vault continuously in step with one
// remote git branch, running as a Docker sidecar beside the editor that writes
// the vault.
//
// The design is issue #21 — body plus both comments — and section references
// (§1–§12) throughout this repo are its sections. This build recognises the
// declared surface's subcommands (§10), reports the build version, resolves the
// config surface (§8), turns a sync loop that commits the vault and pushes it
// (#24), woken by a watch on the vault (#39), and answers whether any of that
// needs a human (#37).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/credential"
	"github.com/andyroberts2/obsync/internal/git"
	"github.com/andyroberts2/obsync/internal/loop"
	"github.com/andyroberts2/obsync/internal/status"
	"github.com/andyroberts2/obsync/internal/watcher"
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
		// version. It is what a suspicious operator runs through `docker exec`,
		// and the most direct answer to "has this been working" (§9).
		//
		// Exit 0 always, including over a vault obsync cannot find and a
		// configuration it cannot use: the report says so, and a subcommand
		// whose job is to answer a question has answered it.
		//
		// The write errors are dropped explicitly rather than by omission: a
		// failed write to stdout has nowhere left to be reported.
		now := time.Now()
		vaultPath, file, health := look(environ, now)
		_, _ = fmt.Fprint(stdout, status.Report(version, vaultPath, file, health, now))
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
		// What the image's HEALTHCHECK calls, and the whole of what makes
		// `docker ps` show whether obsync needs a human (§9). Silent, and exit
		// 0 or 1: Docker reads the status and nothing reads the output, so a
		// line here would be noise in a place nobody looks, once a minute.
		//
		// There is no HTTP server and no port behind this. A sidecar whose
		// entire network posture is one outbound git remote should not acquire
		// a listening socket to answer a question Docker already has a slot
		// for, and a health port would be the tenth variable in a surface that
		// fought to stay at nine (§8, §9).
		if _, _, health := look(environ, time.Now()); health.NeedsHuman {
			return 1
		}
		return 0

	default:
		_, _ = fmt.Fprintf(stderr, "obsync: unknown subcommand %q\n", subcommand)
		usage(stderr)
		return 1
	}
}

// look is what both signal subcommands do: find the vault, read obsync's own
// record of itself, and derive the one verdict from it (§9).
//
// It is one function because the two subcommands must never disagree — the
// report a human reads and the exit status Docker acts on are two renderings of
// one answer, not two answers.
//
// Every way of failing to get to that record is unhealthy rather than an error
// to report: the question was never "did this read succeed", it was "does this
// need a human", and a vault that is not mounted, a directory that is not a
// repository and a container that has not finished a run yet are all a yes.
// That is the whole reason the file lives under `.git/obsync/` — the failure
// modes fall out rather than needing to be coded (§9).
//
// The vault path is the only thing it takes from the configuration, and
// deliberately so: the rest of the surface is the loop's business, and
// resolving it here would make the verdict wait on questions this subcommand is
// not asking. Resolve stats the credential file, which §8 says is a config
// error only *at startup* — later it is the self-healing bad-credential tier —
// so a healthcheck built on it would call a running, healthy obsync unhealthy
// for the length of a token rotation and invite the restart that turns it into
// a crash loop. A configuration obsync could not use needs no help from here
// either: obsync exited on it, so it wrote no status file, and the absent file
// already says so.
func look(environ []string, now time.Time) (vaultPath string, file status.File, health status.Health) {
	vaultPath = config.VaultPath(environ)
	path, err := git.StatusFilePath(vaultPath)
	if err != nil {
		return vaultPath, status.File{}, status.Unavailable(err)
	}
	file, health = status.Of(path, now)
	return vaultPath, file, health
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

	// The watcher is the loop's other injected dependency: an inotify watch on
	// every directory in the vault, whose whole contribution is to wake the
	// loop sooner than the next tick would. It never says what changed — every
	// run asks git — so a watcher that cannot watch costs latency and nothing
	// else, which is why it is started rather than checked: it answers a vault
	// it cannot watch with tick-only mode and a WARN, never with a refusal to
	// sync.
	watching := watcher.Watch(cfg.VaultPath, log)
	defer func() { _ = watching.Close() }()

	l := loop.New(cfg, log, clock.System{}, watching.Wakes())
	defer func() { _ = l.Close() }()

	l.Run(ctx)
	return 0
}
