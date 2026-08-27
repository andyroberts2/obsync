package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/loop"
	"github.com/andyroberts2/obsync/internal/watcher"
)

// This is seam 1: a real vault directory, a real bare remote reached over
// file://, real git underneath, and the loop driven end to end. The only two
// things faked are the ones the design budgets for — the clock and the watcher
// — so the suite runs offline, on a fork's PR, with no credential, and every
// assertion below is about the state of two git repositories and one directory
// rather than about how obsync got there.
//
// Nothing here fakes git. A fake would test obsync's beliefs about git, and
// those beliefs are the entire risk surface.

// humanIdentity is who commits in the vault when obsync did not. It is passed
// per invocation rather than written to the vault's .git/config, so a test that
// wants a human's identity in that file is the one putting it there.
var humanIdentity = []string{"-c", "user.name=A Human", "-c", "user.email=human@example.invalid"}

// vaultEnv is a vault, its remote, and one obsync driving them.
type vaultEnv struct {
	t *testing.T

	// vault is the working tree obsync syncs; remote is the bare repo its
	// origin points at, reached over file:// like every other remote obsync
	// supports; repoURL is the route obsync was given to it.
	vault   string
	remote  string
	repoURL string

	clock *fakeClock
	log   *lockedBuffer

	// wakes is the fake watcher: the channel that says something happened and
	// never what. It is unbuffered, which is what makes a test deterministic —
	// a second wake cannot be delivered until the run the first one started has
	// finished, because the loop only looks at this channel between runs.
	//
	// It is what drives every test that is not about the watcher itself. The
	// ones that are drive obsync's production watcher instead, over the same
	// real vault, and then this channel is not the one the loop is listening
	// to: watching says which.
	wakes    chan struct{}
	watching bool

	// cfg and logger are what the loop is built from, held for two reasons: so
	// that the loop can be built after the vault rather than with it — the
	// production watcher needs the vault to exist before it can watch it — and
	// so that restart can build a second loop over the same vault, remote and
	// clock.
	cfg    config.Config
	logger *slog.Logger

	syncLoop *loop.Loop

	// cancel and finished belong to a loop left turning in the background,
	// which is what turn starts and what a test needs when it has to act while
	// a sync run is still in flight.
	cancel   context.CancelFunc
	finished chan struct{}
	turning  bool
	stopped  bool

	// laptop is the other device's clone of the same remote, made on first use
	// by remoteCommit.
	laptop string
}

// newVault builds a vault that is already a git repo with one commit, a bare
// remote holding that commit, and an obsync configured to sync them.
//
// The loop is not turning yet. A test drives it one run at a time with wake, or
// leaves it turning in the background with turn — either way, nothing obsync
// does happens while a test is still building the vault it will look at.
func newVault(t *testing.T) *vaultEnv {
	t.Helper()
	return newVaultReachedBy(t, nil)
}

// newWatchedVault is newVault with obsync's production watcher in place of the
// fake one: a real inotify watch per directory in a real vault, waking a real
// loop. It is still seam 1 — the vault, the remote and git are the same real
// ones — and the clock is still injected, which is what lets a test say that
// obsync noticed something before a tick could have been what noticed it.
func newWatchedVault(t *testing.T) *vaultEnv {
	t.Helper()

	env := buildAttachedVault(t, nil)
	env.watching = true
	watching := watcher.Watch(env.vault, env.logger)
	t.Cleanup(func() {
		if err := watching.Close(); err != nil {
			t.Errorf("closing the watcher: %v", err)
		}
	})
	env.driveWith(watching.Wakes())
	return env
}

// newVaultReachedBy is newVault with the route to the remote left to the
// caller: reach is handed the half-built environment and answers with the URL
// obsync is pointed at plus any further variables that route needs.
//
// The default is file://, which is what every test that is not about the
// credential path wants — it authenticates with nothing, so nothing about a
// credential can go wrong in it. The credential path's own tests hand back an
// http:// URL in front of the same bare repo, because file:// is exactly the
// route that cannot see any of what they assert.
func newVaultReachedBy(t *testing.T, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
	t.Helper()

	env := buildAttachedVault(t, reach)
	env.driveWith(env.wakes)
	return env
}

// makeVaultARepoOn is the vault an operator already has: a directory that is a
// git repo, on the branch they are sitting on, with an origin and a commit. It
// is the bootstrap case obsync attaches to, and the branch it is on is what the
// tracked branch resolves to (§3).
func (e *vaultEnv) makeVaultARepoOn(branch string) {
	e.t.Helper()

	e.mustGit(e.vault, "init", "--quiet", "-b", branch)
	e.mustGit(e.vault, "remote", "add", config.RemoteName, e.repoURL)

	// An Obsidian vault always has .obsidian/, which is later the vault
	// sentinel (#32). It is here from the start so that every test runs
	// against a vault shaped like a real one.
	e.writeNote(".obsidian/app.json", "{}\n")
	e.mustGit(e.vault, "add", "-A")
	e.mustGit(e.vault, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "the vault before obsync")...)
}

// pushVaultTo is what a human's own git left behind before obsync existed: the
// bytes in the remote and a remote-tracking ref in the vault. The bytes go over
// file:// and the ref is written by hand rather than pushed through origin, so
// that building a vault stays credential-free whatever route obsync is given.
func (e *vaultEnv) pushVaultTo(branch string) {
	e.t.Helper()

	e.mustGit(e.vault, "push", "--quiet", "file://"+e.remote, "refs/heads/"+branch+":refs/heads/"+branch)
	e.mustGit(e.vault, "update-ref", "refs/remotes/"+config.RemoteName+"/"+branch, "refs/heads/"+branch)
}

// buildAttachedVault is newVaultReachedBy with nothing turning yet: the vault
// an operator already has, its remote and the configuration, and no loop. The
// production watcher has to be pointed at a vault that already exists, so the
// two are built in that order and driveWith joins them.
func buildAttachedVault(t *testing.T, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
	t.Helper()

	env := buildVault(t, reach)
	env.makeVaultARepoOn("main")
	env.pushVaultTo("main")
	return env
}

// newVaultToBootstrap builds the two things bootstrap decides about — a bare
// remote and the directory obsync is pointed at — and stops there. The
// directory exists and is empty, the remote holds no refs, and what either of
// them holds next is the test's to say, because that pair is the whole of what
// bootstrap reads (§3, gate 2).
//
// Every environment in this suite is built out of the same two directories;
// newVaultReachedBy is this plus a vault that is already a repo, which is the
// case obsync attaches to.
func newVaultToBootstrap(t *testing.T, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
	t.Helper()

	env := buildVault(t, reach)
	env.driveWith(env.wakes)
	return env
}

// buildVault is newVaultToBootstrap with nothing turning yet: the two
// directories bootstrap decides about, and the configuration, and no loop.
func buildVault(t *testing.T, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
	t.Helper()

	base := t.TempDir()
	env := &vaultEnv{
		t:        t,
		vault:    filepath.Join(base, "vault"),
		remote:   filepath.Join(base, "remote.git"),
		clock:    newFakeClock(),
		log:      &lockedBuffer{},
		wakes:    make(chan struct{}),
		finished: make(chan struct{}),
	}

	env.mustGit(base, "init", "--bare", "--quiet", "-b", "main", env.remote)
	// The same setting obsync's own private config carries, for the same
	// reason and one the harness has of its own: measured on git 2.52, a push
	// into this bare repo has its receive-pack detach a background maintenance
	// process into a session of its own, which the pin in runGit's environment
	// does not reach. Unreaped where nothing reaps — and a suite that builds a
	// vault per test leaks one per test.
	env.mustGit(env.remote, "config", "gc.autoDetach", "false")

	repoURL, extra := "file://"+env.remote, []string(nil)
	if reach != nil {
		repoURL, extra = reach(env)
	}
	env.repoURL = repoURL

	// The mount point, and nothing in it. Docker creates the directory a volume
	// is mounted at whatever the volume holds, so an empty directory is what an
	// operator's first deployment actually presents obsync with.
	if err := os.MkdirAll(env.vault, 0o755); err != nil {
		t.Fatalf("creating the vault: %v", err)
	}

	// The configuration comes through the same environment block an operator
	// sets, resolved by the same code that resolves it in production. Its
	// startup line goes nowhere: the buffer below holds what the sync loop
	// says, so a test asserting that a quiet run is quiet is not reading
	// startup's output.
	cfg, _, err := config.Resolve(append([]string{
		"OBSYNC_REPO=" + repoURL,
		"OBSYNC_VAULT_PATH=" + env.vault,
	}, extra...), io.Discard)
	if err != nil {
		t.Fatalf("resolving the test configuration: %v", err)
	}

	// The level is the resolved one rather than a fixed Info, so a test that
	// needs to see what DEBUG carries — every git invocation with its full
	// argv (§9) — asks for it through the same variable an operator sets.
	env.cfg = cfg
	env.logger = slog.New(slog.NewTextHandler(env.log, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return env
}

// driveWith builds the loop the rest of this harness turns, woken by the
// channel it is given. Every test drives one of exactly two: the fake watcher's
// channel, or the production watcher's.
func (e *vaultEnv) driveWith(wakes <-chan struct{}) {
	e.t.Helper()

	e.syncLoop = loop.New(e.cfg, e.logger, e.clock, wakes)
	e.t.Cleanup(e.stop)
}

// newVaultToBootstrapWith is newVaultToBootstrap with further variables on the
// config surface set — OBSYNC_BRANCH, which is the bootstrap override (§3).
func newVaultToBootstrapWith(t *testing.T, extra ...string) *vaultEnv {
	t.Helper()

	return newVaultToBootstrap(t, func(e *vaultEnv) (string, []string) {
		return "file://" + e.remote, extra
	})
}

// newEmptyVault is the bootstrap case an operator meets on a first deployment:
// an empty directory, and a remote holding a vault to clone into it. The
// remote's default branch is named, because "the remote's default branch" is
// what obsync resolves the tracked branch to here and taking `main` instead is
// the mistake §3 exists to prevent.
func newEmptyVault(t *testing.T, defaultBranch string, alsoBranches ...string) *vaultEnv {
	t.Helper()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote(defaultBranch, alsoBranches...)
	return env
}

// seedRemote gives the bare remote a vault to be cloned: one commit on every
// branch named, the first of them the remote's own HEAD, plus an annotated tag
// — which is a ref obsync's one-branch-each-direction refspec may never carry
// either way (§3).
func (e *vaultEnv) seedRemote(defaultBranch string, alsoBranches ...string) {
	e.t.Helper()

	seed := filepath.Join(e.t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		e.t.Fatalf("creating the seed repo: %v", err)
	}
	e.mustGit(seed, "init", "--quiet", "-b", defaultBranch)
	for path, content := range map[string]string{
		".obsidian/app.json":       "{}\n",
		"Notes/from the remote.md": remoteSeedNote,
	} {
		full := filepath.Join(seed, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			e.t.Fatalf("creating the folder for %q: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			e.t.Fatalf("writing %q: %v", path, err)
		}
	}
	e.mustGit(seed, "add", "-A")
	e.mustGit(seed, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "the vault someone else pushed")...)
	e.mustGit(seed, append(append([]string{}, humanIdentity...), "tag", "-a", "v1", "-m", "a tag on the remote")...)

	refspecs := []string{"refs/heads/" + defaultBranch + ":refs/heads/" + defaultBranch, "refs/tags/v1:refs/tags/v1"}
	for _, branch := range alsoBranches {
		refspecs = append(refspecs, "refs/heads/"+defaultBranch+":refs/heads/"+branch)
	}
	e.mustGit(seed, append([]string{"push", "--quiet", "file://" + e.remote}, refspecs...)...)
	e.mustGit(e.remote, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
}

// remoteSeedNote is the note a seeded remote holds, and therefore the bytes a
// vault obsync cloned must be holding afterwards.
const remoteSeedNote = "the note someone else pushed\n"

// The timing constants a test may not read from obsync, restated here on
// purpose: a test that asserts 120s by importing the constant that sets it
// asserts nothing. These are §1's and §2's numbers, and each is written out at
// the assertion that uses it.
const (
	networkDeadline  = 120 * time.Second
	shutdownDeadline = 30 * time.Second
	quietWindow      = 10 * time.Second
	maxWaitCap       = 5 * time.Minute
	tick             = 60 * time.Second
	tickJitter       = 6 * time.Second
)

// wake is one wake-up, and the loop's unit of work: obsync performs exactly one
// sync run for it and this returns when that run is over.
//
// It is the startup run — obsync runs the loop immediately and then falls into
// its cadence (§2) — driven and then stopped, so a test that does not care when
// obsync acts does not have to say. A test that does care turns the loop
// instead and drives the clock.
//
// Driving it this way is what makes every test below deterministic: the vault a
// run looks at is the vault the test finished building, never one it is still
// writing.
func (e *vaultEnv) wake() {
	e.t.Helper()

	if e.stopped {
		e.t.Fatal("this test woke obsync after stopping it; a stopped loop performs no more runs")
	}
	if e.turning {
		e.t.Fatal("this loop is already turning; a turning loop is woken with watcherWake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		e.syncLoop.Run(ctx)
	}()

	e.awaitIdle()
	cancel()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		e.t.Fatalf("obsync did not finish its run within 60s of a stop; it said:\n%s", e.log.String())
	}
}

// turn leaves the loop running in the background, woken by the watcher's
// channel and by its own tick, which is how obsync actually runs: a wake-up
// arrives or a deadline falls due, a run happens, and nothing is looked at
// again until that run is over.
//
// It returns while the startup run is still in flight, because that is what a
// test driving a run that never finishes needs. A test that wants the vault
// settled first says so with awaitIdle.
func (e *vaultEnv) turn() {
	e.t.Helper()

	if e.turning {
		e.t.Fatal("the loop is already turning")
	}
	e.turning = true
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() {
		defer close(e.finished)
		e.syncLoop.Run(ctx)
	}()
}

// awaitIdle blocks until obsync has finished whatever it was doing and is
// waiting on the clock again. It is the handshake every timing assertion is
// built on: time moves only when a test says so, and a test only says so once
// obsync is waiting for it.
//
// The deadline it steps over is a git being timed out, taken out in the middle
// of a run rather than between two. Told apart by length rather than by asking:
// the tick is always in the running for the next thing due, so the loop never
// waits longer than one, and the only deadline longer than a tick is the 120s a
// network git gets.
//
// That discriminator is one-sided, and the boundary is worth knowing before a
// test walks into it: the grace a killed process group gets is shorter than a
// tick, so it reads as an idle wait. Nothing here advances the clock after a
// kill — the two tests that kill a git assert and stop — and a test that wants
// to would need a handshake that asks which deadline rather than how long.
func (e *vaultEnv) awaitIdle() {
	e.t.Helper()

	limit := time.After(60 * time.Second)
	for {
		select {
		case waited := <-e.clock.waits:
			if waited > tick+tickJitter {
				continue
			}
			return
		case <-limit:
			e.t.Fatalf("obsync did not come back to waiting on the clock within 60s; it said:\n%s", e.log.String())
		}
	}
}

// advance moves the fake clock forward by d and returns once obsync has
// finished reacting to it. Moving past nothing returns immediately, which is
// the whole of how a test asserts that obsync did not act early.
func (e *vaultEnv) advance(d time.Duration) {
	e.t.Helper()

	if e.clock.advance(d) {
		e.awaitIdle()
	}
}

// turn starts the loop; watcherWake hands the turning loop one wake-up. The
// channel is unbuffered, so the send completes only when the loop comes back
// for it — which it does only between runs, because only one sync run is ever
// in flight (§2).
//
// A wake-up does not start a run. It says something happened, never what, and
// what it moves is the moment a run is due: ten seconds of quiet from here, or
// the max-wait cap if the vault never gets there (§2).
func (e *vaultEnv) watcherWake() {
	e.t.Helper()

	if e.watching {
		e.t.Fatal("this vault is watched for real; a wake-up comes from writing in it, not from here")
	}
	select {
	case e.wakes <- struct{}{}:
	case <-time.After(30 * time.Second):
		e.t.Fatalf("obsync did not take a wake-up within 30s; it said:\n%s", e.log.String())
	}
	// The loop takes its next deadline out fresh after every wake-up, so
	// waiting for that one is what makes the wake and the clock ordered rather
	// than merely likely to be.
	e.awaitIdle()
}

// watcherGone closes the watcher's channel, which is what a watcher that has
// torn its watches down looks like from inside the loop (§1's ENOSPC path), and
// what any watcher that stops for a reason of its own looks like too.
//
// It waits for the same handshake watcherWake does, because obsync should react
// to this by taking its next deadline out and carrying on ticking. If it has
// stopped instead, that is what this says, rather than waiting out a bound on a
// loop that is never coming back.
func (e *vaultEnv) watcherGone() {
	e.t.Helper()

	if e.watching {
		e.t.Fatal("this vault is watched for real; its watcher stands down when it cannot watch, " +
			"not when a test says so")
	}
	if !e.turning {
		e.t.Fatal("nothing is turning to lose its watcher")
	}
	close(e.wakes)

	limit := time.After(60 * time.Second)
	for {
		select {
		case <-e.finished:
			e.t.Fatal("obsync's sync loop returned when its watcher went away, want it left in " +
				"tick-only mode: a watcher tearing its watches down is not a sync failure, and " +
				"exiting on one is silent non-backup with nothing to announce it (§1, §2)")
		case waited := <-e.clock.waits:
			if waited > tick+tickJitter {
				continue
			}
			return
		case <-limit:
			e.t.Fatalf("obsync did not come back to waiting on the clock within 60s; it said:\n%s", e.log.String())
		}
	}
}

// sigterm is the signal without the wait: obsync refuses to start a new run and
// finishes the one in flight, and the test stays free to drive the clock while
// it does.
func (e *vaultEnv) sigterm() {
	e.t.Helper()

	if !e.turning {
		e.t.Fatal("nothing is turning to be stopped")
	}
	e.cancel()
}

// stop is SIGTERM: obsync refuses to start a new run, finishes the one in
// flight, and returns. Every assertion helper calls it first, because what
// obsync did is only settled once the run doing it is over.
func (e *vaultEnv) stop() {
	e.t.Helper()

	if e.stopped {
		return
	}
	e.stopped = true
	if e.turning {
		e.cancel()
		select {
		case <-e.finished:
		case <-time.After(60 * time.Second):
			e.t.Fatalf("obsync did not finish its run within 60s of a stop; it said:\n%s", e.log.String())
		}
	}
	if err := e.syncLoop.Close(); err != nil {
		e.t.Errorf("closing the sync loop: %v", err)
	}
}

// writeNote writes a file in the vault, creating the folders above it.
func (e *vaultEnv) writeNote(path, content string) {
	e.t.Helper()

	full := filepath.Join(e.vault, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatalf("creating the folder for %q: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatalf("writing %q: %v", path, err)
	}
}

func (e *vaultEnv) deleteNote(path string) {
	e.t.Helper()

	if err := os.Remove(filepath.Join(e.vault, path)); err != nil {
		e.t.Fatalf("deleting %q: %v", path, err)
	}
}

// remoteFile is the content of a path at the remote's tip, which is the whole
// point of obsync existing.
func (e *vaultEnv) remoteFile(path string) string {
	e.t.Helper()
	e.stop()

	out, code := e.git(e.remote, "cat-file", "blob", "refs/heads/main:"+path)
	if code != 0 {
		e.t.Fatalf("the remote holds no %q at its tip. obsync said:\n%s", path, e.log.String())
	}
	return out
}

// remoteFileOn, remoteHoldsBranch and commitsOnBranch are the whole-of-the-
// remote assertions with the branch named rather than assumed. Every other
// helper here reads `main`, which is the branch every vault in this suite is on
// — but the branch obsync resolved is the thing bootstrap decides, so a test
// about bootstrap has to be able to say which one it means.
func (e *vaultEnv) remoteFileOn(branch, path string) string {
	e.t.Helper()
	e.stop()

	out, code := e.git(e.remote, "cat-file", "blob", "refs/heads/"+branch+":"+path)
	if code != 0 {
		e.t.Fatalf("the remote holds no %q at the tip of %q. obsync said:\n%s", path, branch, e.log.String())
	}
	return out
}

// vaultFile is what the vault holds at a path, and vaultHolds whether it holds
// anything there at all. A clone is only a clone if the bytes arrived.
func (e *vaultEnv) vaultFile(path string) string {
	e.t.Helper()
	e.stop()

	content, err := os.ReadFile(filepath.Join(e.vault, path))
	if err != nil {
		e.t.Fatalf("the vault holds no %q: %v. obsync said:\n%s", path, err, e.log.String())
	}
	return string(content)
}

// stillTurning reports that obsync is parked alive rather than stopped: a
// refusal it cannot act on leaves it re-checking, never exiting (§7).
func (e *vaultEnv) stillTurning() bool {
	e.t.Helper()

	if !e.turning {
		e.t.Fatal("nothing is turning, so there is nothing to still be turning")
	}
	select {
	case <-e.finished:
		return false
	default:
		return true
	}
}

// remoteHoldsYet and commitsSoFar look at the world without stopping obsync, so
// that a test can go on driving the clock afterwards. They are safe for the
// same reason every timing assertion in this suite is: advance and watcherWake
// return only once obsync is waiting on the clock again, so nothing is in
// flight while they read.
func (e *vaultEnv) remoteHoldsYet(path string) bool {
	e.t.Helper()

	_, code := e.git(e.remote, "cat-file", "-e", "refs/heads/main:"+path)
	return code == 0
}

// remoteContentYet is remoteFile without stopping obsync: what the remote's tip
// holds at a path, and whether it holds anything there at all.
func (e *vaultEnv) remoteContentYet(path string) (string, bool) {
	e.t.Helper()

	out, code := e.git(e.remote, "cat-file", "blob", "refs/heads/main:"+path)
	if code != 0 {
		return "", false
	}
	return out, true
}

func (e *vaultEnv) commitsSoFar(dir string) string {
	e.t.Helper()

	return strings.TrimSpace(e.mustGit(dir, "rev-list", "--count", "refs/heads/main"))
}

// runAlreadyStopped drives the loop with its context already done, which is a
// SIGTERM that arrived before obsync performed a single run.
func (e *vaultEnv) runAlreadyStopped() {
	e.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.syncLoop.Run(ctx)
}

// remoteHolds reports whether the remote's tip holds anything at a path, which
// is how the absence of one is asserted — a deletion that arrived is a path
// that is not there rather than bytes that changed.
func (e *vaultEnv) remoteHolds(path string) bool {
	e.t.Helper()
	e.stop()

	_, code := e.git(e.remote, "cat-file", "-e", "refs/heads/main:"+path)
	return code == 0
}

// remoteSubject is the subject line of the remote tip's commit; remoteMessage
// is the whole of it.
func (e *vaultEnv) remoteSubject() string {
	e.t.Helper()
	subject, _, _ := strings.Cut(e.remoteMessage(), "\n")
	return subject
}

func (e *vaultEnv) remoteMessage() string {
	e.t.Helper()
	e.stop()

	return strings.TrimRight(e.mustGit(e.remote, "log", "-1", "--format=%B", "refs/heads/main"), "\n")
}

// remoteAuthor is the commit identity the remote tip was written under.
func (e *vaultEnv) remoteAuthor() string {
	e.t.Helper()
	e.stop()

	return strings.TrimRight(e.mustGit(e.remote, "log", "-1", "--format=%an <%ae>", "refs/heads/main"), "\n")
}

// commitsOn counts the commits on a branch, in the vault or in the remote.
func (e *vaultEnv) commitsOn(dir string) string {
	e.t.Helper()
	e.stop()

	return strings.TrimSpace(e.mustGit(dir, "rev-list", "--count", "refs/heads/main"))
}

// said is everything obsync logged, once it has stopped saying it.
func (e *vaultEnv) said() string {
	e.t.Helper()
	e.stop()

	return e.log.String()
}

// git runs real git and returns its stdout and exit status. The harness drives
// git exactly as obsync does — as a subprocess — and pins its configuration
// too, so that a developer's own ~/.gitconfig cannot change what a test means.
func (e *vaultEnv) git(dir string, args ...string) (string, int) {
	e.t.Helper()
	return runGit(e.t, dir, args...)
}

func (e *vaultEnv) mustGit(dir string, args ...string) string {
	e.t.Helper()

	out, code := runGit(e.t, dir, args...)
	if code != 0 {
		e.t.Fatalf("git %s in %s exited %d: %s", strings.Join(args, " "), dir, code, out)
	}
	return out
}

func runGit(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		// gc.autoDetach=false is one of the settings obsync's own private
		// config carries (§7 forbids a detached background repack), and the
		// harness pins it for a reason of its own: measured on git 2.52, a
		// commit or a push otherwise leaves behind one background maintenance
		// process that has detached into a session of its own, and a suite
		// that builds a vault per test leaks two of those per test. They are
		// harmless where something reaps them and immortal where nothing
		// does. Pinned through git's environment spelling rather than -c so
		// that every call site here gets it without touching its argv.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=gc.autoDetach",
		"GIT_CONFIG_VALUE_0=false",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit):
		return out.String(), exit.ExitCode()
	default:
		t.Fatalf("running git %s in %s: %v", strings.Join(args, " "), dir, err)
		return "", 0
	}
}

// installHook writes an executable hook into the bare remote. A local bare repo
// takes a real pre-receive hook, which is what makes every verdict a remote can
// return reproducible offline with no flake.
func (e *vaultEnv) installHook(name, script string) {
	e.t.Helper()

	path := filepath.Join(e.remote, "hooks", name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		e.t.Fatalf("installing the %s hook: %v", name, err)
	}
}

func (e *vaultEnv) removeHook(name string) {
	e.t.Helper()

	if err := os.Remove(filepath.Join(e.remote, "hooks", name)); err != nil {
		e.t.Fatalf("removing the %s hook: %v", name, err)
	}
}

// lockedBuffer is what obsync logs into. The loop writes from its own
// goroutine, so the buffer is locked rather than left to -race to find.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClock is the other injected seam. Every timing rule in this design is a
// constant with a measured reason beside it, and this is what makes each one
// testable in a suite that runs in seconds rather than in two minutes per
// deadline.
//
// It never advances on its own: time passes only when a test says so, and only
// as far as the next thing waiting on it.
type fakeClock struct {
	mu      sync.Mutex
	start   time.Time
	now     time.Time
	waiting []*sleeper
	taken   []time.Duration

	// live is the deadline obsync is currently waiting on, which is the last
	// one it took out: the sync loop takes out exactly one at a time, and when
	// a wake-up shortens it the shorter one is the one it now listens to. The
	// one it stopped listening to is left in waiting, where firing it is
	// harmless — nobody reads it — and the distinction is what lets a test know
	// whether moving the clock woke obsync or moved past nothing.
	live *sleeper

	// waits carries one value per deadline taken out, in order, so a test can
	// wait for obsync to be waiting rather than sleeping until it probably is.
	waits chan time.Duration
}

type sleeper struct {
	due time.Time
	ch  chan time.Time
}

func newFakeClock() *fakeClock {
	// A fixed instant, so nothing in a test depends on the day it runs.
	start := time.Date(2026, 8, 24, 14, 3, 0, 0, time.UTC)
	return &fakeClock{start: start, now: start, waits: make(chan time.Duration, 1024)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	waiter := &sleeper{due: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.waiting = append(c.waiting, waiter)
	c.taken = append(c.taken, d)
	c.live = waiter
	c.mu.Unlock()

	select {
	case c.waits <- d:
	default:
	}
	return waiter.ch
}

// drainDeadlines discards every deadline obsync has taken out so far, so that a
// test waiting for the next one is waiting for one it has not already seen. It
// has two constituencies: a run with more than one network git in it, where a
// fetch and a push each take out a deadline of the same length and a test that
// has to wait for the second cannot tell it from the first by watching; and a
// test that has just waited on the OS rather than on obsync, and therefore does
// not know how many wake-ups arrived.
func (c *fakeClock) drainDeadlines() {
	for {
		select {
		case <-c.waits:
		default:
			return
		}
	}
}

// awaitDeadline blocks until obsync has taken out its next deadline, so that a
// test never advances time past something that has not started waiting yet.
func (c *fakeClock) awaitDeadline(t *testing.T) {
	t.Helper()

	select {
	case <-c.waits:
	case <-time.After(30 * time.Second):
		t.Fatal("obsync took out no deadline within 30s")
	}
}

// advanceToNextDeadline moves time to exactly the moment the earliest waiter is
// due, and fires everything due then. Nothing moves further: what a test
// asserts afterwards is that obsync acted at the deadline the design states,
// and not merely at some point after it.
func (c *fakeClock) advanceToNextDeadline(t *testing.T) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.waiting) == 0 {
		t.Fatal("nothing is waiting on the clock, so there is no deadline to advance to")
	}
	next := c.waiting[0].due
	for _, waiter := range c.waiting[1:] {
		if waiter.due.Before(next) {
			next = waiter.due
		}
	}
	c.now = next
	c.fire()
}

// advance moves time forward by d, fires every deadline that falls due, and
// reports whether the one obsync is actually waiting on was among them — which
// is how a test knows whether to expect obsync to do anything at all.
func (c *fakeClock) advance(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
	live := c.live
	c.fire()
	return live != nil && c.live == nil
}

// fire delivers to every waiter that is due and drops it. It is called with the
// lock held.
func (c *fakeClock) fire() {
	var still []*sleeper
	for _, waiter := range c.waiting {
		if waiter.due.After(c.now) {
			still = append(still, waiter)
			continue
		}
		if waiter == c.live {
			c.live = nil
		}
		waiter.ch <- c.now
	}
	c.waiting = still
}

// elapsed is how much time obsync has been allowed to believe has passed.
func (c *fakeClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.start)
}

// networkDeadlinesTaken is how many times obsync asked for a git to be timed
// out. It is the whole of §1's asymmetry expressed as a number: a run full of
// local git commands and one network git takes exactly one.
//
// Counted by duration, because obsync waits on the same clock for its cadence
// as it times a git out with, and the two are different acts: the network
// deadline is the only one that kills anything.
func (c *fakeClock) networkDeadlinesTaken() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for _, d := range c.taken {
		if d == networkDeadline {
			count++
		}
	}
	return count
}

// waitsTaken is every duration obsync has waited on, in order.
func (c *fakeClock) waitsTaken() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration{}, c.taken...)
}

// vaultHoldsYet is vaultHolds without stopping obsync, so that a test can go on
// driving the clock afterwards.
func (e *vaultEnv) vaultHoldsYet(path string) bool {
	e.t.Helper()

	_, err := os.Lstat(filepath.Join(e.vault, path))
	return err == nil
}

// saidSoFar is everything obsync has logged without stopping it, so that a test
// can read a refusal and then go on driving the clock to see it clear.
func (e *vaultEnv) saidSoFar() string {
	e.t.Helper()
	return e.log.String()
}

// remoteHoldsBranchYet is remoteHoldsBranch without stopping obsync.
func (e *vaultEnv) remoteHoldsBranchYet(branch string) bool {
	e.t.Helper()

	_, code := e.git(e.remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return code == 0
}

// commitsOnBranchYet is commitsOnBranch without stopping obsync.
func (e *vaultEnv) commitsOnBranchYet(dir, branch string) string {
	e.t.Helper()

	return strings.TrimSpace(e.mustGit(dir, "rev-list", "--count", "refs/heads/"+branch))
}

// The other device, and the half of "bidirectional" this suite could not reach
// before: a second clone of the same remote, where someone writes a note and
// pushes it. It is a real clone driven by real git rather than a ref written by
// hand, so what obsync fetches is what a remote really holds.
func (e *vaultEnv) remoteCommit(path, content string) {
	e.t.Helper()

	laptop := e.laptopUpToDate()
	full := filepath.Join(laptop, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatalf("creating the folder for %q on the laptop: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatalf("writing %q on the laptop: %v", path, err)
	}
	e.mustGit(laptop, "add", "-A")
	e.mustGit(laptop, e.asAHuman("commit", "--quiet", "-m", "written on the laptop")...)
	e.mustGit(laptop, "push", "--quiet", "file://"+e.remote, "refs/heads/main:refs/heads/main")
}

// remotePurgesItsTip is the operator who force-pushed to purge a leaked secret:
// the commit at the remote's tip is gone, and the branch is force-pushed over
// the vault's own history. This is the act obsync must never undo — the vault
// is now *ahead* of the remote, so nothing but detection stands between the
// purge and a push that restores it (§3, user story 35).
//
// The force is the human's, in their own clone. obsync has no such flag.
func (e *vaultEnv) remotePurgesItsTip() {
	e.t.Helper()

	laptop := e.laptopUpToDate()
	e.mustGit(laptop, "reset", "--hard", "--quiet", "HEAD~1")
	e.mustGit(laptop, "push", "--quiet", "--force", "file://"+e.remote, "refs/heads/main:refs/heads/main")
}

// remoteRewritesItsHistory is the same act with something put back in its
// place, which is what a real purge looks like: the tip is replaced rather than
// removed, so the vault ends up diverged from the remote rather than ahead of
// it, and it is the merge rather than the push that would resurrect what the
// rewrite took out.
func (e *vaultEnv) remoteRewritesItsHistory() {
	e.t.Helper()

	laptop := e.laptopUpToDate()
	e.mustGit(laptop, "reset", "--hard", "--quiet", "HEAD~1")
	if err := os.MkdirAll(filepath.Join(laptop, "Notes"), 0o755); err != nil {
		e.t.Fatalf("creating Notes on the laptop: %v", err)
	}
	if err := os.WriteFile(filepath.Join(laptop, "Notes", "purged.md"), []byte("nothing to see\n"), 0o644); err != nil {
		e.t.Fatalf("writing the note that replaces the purged one: %v", err)
	}
	e.mustGit(laptop, "add", "-A")
	e.mustGit(laptop, e.asAHuman("commit", "--quiet", "-m", "the history someone rewrote")...)
	e.mustGit(laptop, "push", "--quiet", "--force", "file://"+e.remote, "refs/heads/main:refs/heads/main")
}

// The other repair obsync's own remedy names: the human decides the vault's
// history is the one they meant and puts it back on the remote, rather than
// taking the remote's. It is a force-push, in their own clone of their own
// repo — obsync has no such flag — and afterwards the remote holds exactly what
// obsync's branch holds, so there is nothing left for a merge to resurrect.
func (e *vaultEnv) vaultsHistoryIsForcedBackOntoTheRemote() {
	e.t.Helper()

	e.mustGit(e.vault, "push", "--quiet", "--force", "file://"+e.remote,
		"refs/heads/main:refs/heads/main")
}

// laptopUpToDate is that second clone, made on first use and brought up to the
// remote's tip on every use — someone whose laptop is behind pulls before they
// push, and a test is not about that.
func (e *vaultEnv) laptopUpToDate() string {
	e.t.Helper()

	base := filepath.Dir(e.vault)
	if e.laptop == "" {
		e.laptop = filepath.Join(base, "laptop")
		e.mustGit(base, "clone", "--quiet", "file://"+e.remote, e.laptop)
		if err := os.MkdirAll(filepath.Join(e.laptop, "Notes"), 0o755); err != nil {
			e.t.Fatalf("creating Notes on the laptop: %v", err)
		}
		return e.laptop
	}
	e.mustGit(e.laptop, "fetch", "--quiet", "file://"+e.remote, "refs/heads/main")
	e.mustGit(e.laptop, "reset", "--hard", "--quiet", "FETCH_HEAD")
	return e.laptop
}

// asAHuman prefixes a git argv with an identity that is not obsync's, so that a
// commit made by the test is visibly not one obsync made.
func (e *vaultEnv) asAHuman(args ...string) []string {
	return append(append([]string{}, humanIdentity...), args...)
}

// vaultTip and remoteTip are the two commits a sync run exists to bring
// together. Equal means the vault and the remote hold the same history, which
// is a fast-forward's whole result — and, unlike a commit count, says so even
// when both sides have the same number of commits.
func (e *vaultEnv) vaultTip() string {
	e.t.Helper()
	return strings.TrimSpace(e.mustGit(e.vault, "rev-parse", "refs/heads/main"))
}

func (e *vaultEnv) remoteTip() string {
	e.t.Helper()
	return strings.TrimSpace(e.mustGit(e.remote, "rev-parse", "refs/heads/main"))
}

// remoteBranches is every branch the remote holds, which is how "obsync pushes
// straight to the tracked branch — no device branch" is asserted: the absence
// of a second branch rather than the presence of the first.
func (e *vaultEnv) remoteBranches() []string {
	e.t.Helper()

	out := e.mustGit(e.remote, "for-each-ref", "--format=%(refname)", "refs/heads/")
	return strings.Fields(out)
}

// restart is the operator's reflex: the container is stopped and started again,
// which is the one thing that clears everything obsync holds in memory. The
// vault, the remote and the clock are untouched — a restart does not rewind the
// world — so what survives it is whatever obsync can re-derive from the repo.
func (e *vaultEnv) restart() {
	e.t.Helper()

	e.stop()
	e.stopped, e.turning = false, false
	e.finished = make(chan struct{})
	e.driveWith(e.wakes)
}
