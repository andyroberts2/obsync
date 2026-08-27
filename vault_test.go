package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
//
// testing.TB rather than *testing.T, so that a benchmark reaches seam 1 through
// the same harness a test does. Sizing (#44) measures obsync's own cost against
// a real vault and a real bare remote, and a second builder for it would be a
// second definition of what a vault is.
type vaultEnv struct {
	t testing.TB

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

	// environ is the block the configuration above was resolved from, kept so
	// that a subcommand run in a process of its own is handed exactly what the
	// loop was — which is what `docker exec` and the image's HEALTHCHECK both
	// get, since both inherit the container's environment.
	environ []string

	// home is the HOME a subcommand of this obsync is started with, empty for
	// the test runner's own. It is settable because HOME is how a key arrives
	// (§8), so what else is mounted beside one is a property of a real
	// deployment rather than of a test harness.
	home string

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

	// aside is where theVaultGoesEmpty put the vault, so that
	// theVaultComesBack can put it back.
	aside string

	// rotted is the loose object theDiskRotsTheObjectGitNeedsMost damaged and
	// the bytes it held, so that theRottedObjectIsRestored can put the disk
	// back the way a human recovering their repository would.
	rotted      string
	rottedBytes []byte
}

// newVault builds a vault that is already a git repo with one commit, a bare
// remote holding that commit, and an obsync configured to sync them.
//
// The loop is not turning yet. A test drives it one run at a time with wake, or
// leaves it turning in the background with turn — either way, nothing obsync
// does happens while a test is still building the vault it will look at.
func newVault(t testing.TB) *vaultEnv {
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
func newVaultReachedBy(t testing.TB, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
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
func buildAttachedVault(t testing.TB, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
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
func buildVault(t testing.TB, reach func(*vaultEnv) (repoURL string, extra []string)) *vaultEnv {
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
	env.environ = append([]string{
		"OBSYNC_REPO=" + repoURL,
		"OBSYNC_VAULT_PATH=" + env.vault,
	}, extra...)
	cfg, _, err := config.Resolve(env.environ, io.Discard)
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
// asserts nothing. These are §1's, §2's and §6's numbers, and each is written
// out at the assertion that uses it.
const (
	networkDeadline  = 120 * time.Second
	shutdownDeadline = 30 * time.Second
	quietWindow      = 10 * time.Second
	maxWaitCap       = 5 * time.Minute
	tick             = 60 * time.Second
	tickJitter       = 6 * time.Second
	settleInterval   = time.Second
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

// theVaultGoesEmpty is the vault sentinel's own case (§7): every note and
// `.obsidian/` itself are gone while `.git` is still there, which is what a
// dropped or misdirected mount looks like from inside git — every tracked file
// reported deleted, and a fail-open local half would commit exactly that.
//
// The entries are moved aside rather than deleted, because the other half of
// the case is the mount coming back with the vault still on it.
func (e *vaultEnv) theVaultGoesEmpty() {
	e.t.Helper()

	entries, err := os.ReadDir(e.vault)
	if err != nil {
		e.t.Fatalf("reading the vault: %v", err)
	}
	e.aside = filepath.Join(e.t.TempDir(), "aside")
	if err := os.MkdirAll(e.aside, 0o755); err != nil {
		e.t.Fatalf("making somewhere to put the vault: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.Rename(filepath.Join(e.vault, entry.Name()), filepath.Join(e.aside, entry.Name())); err != nil {
			e.t.Fatalf("moving %q out of the vault: %v", entry.Name(), err)
		}
	}
}

// theVaultComesBack puts back what theVaultGoesEmpty moved aside.
func (e *vaultEnv) theVaultComesBack() {
	e.t.Helper()

	entries, err := os.ReadDir(e.aside)
	if err != nil {
		e.t.Fatalf("reading what was moved out of the vault: %v", err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(e.aside, entry.Name()), filepath.Join(e.vault, entry.Name())); err != nil {
			e.t.Fatalf("moving %q back into the vault: %v", entry.Name(), err)
		}
	}
}

// theRepositoryGoes and theRepositoryComesBack are gate 2 on a vault obsync has
// already bootstrapped: the `.git` moved aside and put back. Moved rather than
// deleted, because what the gate has to be true of is a repository that comes
// back holding the history it had.
func (e *vaultEnv) theRepositoryGoes() {
	e.t.Helper()

	e.aside = filepath.Join(e.t.TempDir(), "aside")
	if err := os.Rename(filepath.Join(e.vault, ".git"), e.aside); err != nil {
		e.t.Fatalf("moving the repository out of the vault: %v", err)
	}
}

func (e *vaultEnv) theRepositoryComesBack() {
	e.t.Helper()

	if err := os.Rename(e.aside, filepath.Join(e.vault, ".git")); err != nil {
		e.t.Fatalf("moving the repository back into the vault: %v", err)
	}
}

// theHumanLeavesAMergeHalfFinished is gate 4's own case: a human ran `git
// merge` in their own vault, it conflicted, and they walked away from it. It is
// their merge, driven by their own git with their own identity, and what it
// leaves behind is a real MERGE_HEAD and a real unmerged index.
func (e *vaultEnv) theHumanLeavesAMergeHalfFinished() {
	e.t.Helper()

	e.mustGit(e.vault, "checkout", "--quiet", "-b", "a-branch-the-human-made")
	e.writeNote("Daily/contested.md", "what the human wrote on their branch\n")
	e.mustGit(e.vault, "add", "-A")
	e.mustGit(e.vault, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "on the human's branch")...)
	e.mustGit(e.vault, "checkout", "--quiet", "main")
	e.writeNote("Daily/contested.md", "what the human wrote on main\n")
	e.mustGit(e.vault, "add", "-A")
	e.mustGit(e.vault, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "on main")...)

	// The merge is expected to conflict, which is the whole point of it: an
	// exit status of zero here would mean the vault is not in the state the
	// gate is about.
	if _, code := e.git(e.vault, append(append([]string{}, humanIdentity...), "merge", "a-branch-the-human-made")...); code == 0 {
		e.t.Fatal("the human's merge did not conflict, so nothing was left half-finished")
	}
}

// vaultTipYet and vaultHoldsBranchYet look at the vault without stopping
// obsync, the way remoteHoldsYet looks at the remote.
func (e *vaultEnv) vaultTipYet() string {
	e.t.Helper()

	return strings.TrimSpace(e.mustGit(e.vault, "rev-parse", "HEAD"))
}

func (e *vaultEnv) vaultHoldsBranchYet(branch string) bool {
	e.t.Helper()

	_, code := e.git(e.vault, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return code == 0
}

// aSecondBareRemote is somewhere obsync was never pointed at, for the one gate
// whose whole subject is a vault landing in the wrong place.
func (e *vaultEnv) aSecondBareRemote() string {
	e.t.Helper()

	elsewhere := filepath.Join(e.t.TempDir(), "elsewhere.git")
	e.mustGit(e.t.TempDir(), "init", "--bare", "--quiet", "-b", "main", elsewhere)
	e.mustGit(elsewhere, "config", "gc.autoDetach", "false")
	return elsewhere
}

// theVaultBecomesUnwritable is gate 1's own case: the vault directory is there
// and obsync's UID may not create anything in it, which is what a vault chowned
// to somebody else looks like from inside the container.
//
// The mode is put back at the end of the test whatever happens, because a
// directory a test cannot delete outlives the test.
func (e *vaultEnv) theVaultBecomesUnwritable() {
	e.t.Helper()

	if err := os.Chmod(e.vault, 0o555); err != nil {
		e.t.Fatalf("making the vault unwritable: %v", err)
	}
	e.t.Cleanup(func() { _ = os.Chmod(e.vault, 0o755) })
}

func (e *vaultEnv) theVaultBecomesWritableAgain() {
	e.t.Helper()

	if err := os.Chmod(e.vault, 0o755); err != nil {
		e.t.Fatalf("making the vault writable again: %v", err)
	}
}

// aSecondObsyncOnTheSameVault is gate 8's own case: a second obsync, configured
// exactly as the first, pointed at the same vault. It has its own clock and its
// own log, because what it says and when it acts are the whole of what the test
// is about.
func (e *vaultEnv) aSecondObsyncOnTheSameVault() *vaultEnv {
	e.t.Helper()

	second := &vaultEnv{
		t:        e.t,
		vault:    e.vault,
		remote:   e.remote,
		repoURL:  e.repoURL,
		clock:    newFakeClock(),
		log:      &lockedBuffer{},
		wakes:    make(chan struct{}),
		finished: make(chan struct{}),
		cfg:      e.cfg,
	}
	second.logger = slog.New(slog.NewTextHandler(second.log, &slog.HandlerOptions{Level: e.cfg.LogLevel}))
	second.driveWith(second.wakes)
	return second
}

// someoneElseHoldsTheIndexLock is the third writer's own git holding the lock
// obsync's `git add` needs — a plugin, a human at a terminal, or a backup tool
// driving git in the vault. It is a real `.git/index.lock`, which is the only
// thing there is to hold.
func (e *vaultEnv) someoneElseHoldsTheIndexLock() {
	e.t.Helper()

	if err := os.WriteFile(filepath.Join(e.vault, ".git", "index.lock"), nil, 0o644); err != nil {
		e.t.Fatalf("taking the index lock: %v", err)
	}
}

func (e *vaultEnv) theIndexLockIsReleased() {
	e.t.Helper()

	if err := os.Remove(filepath.Join(e.vault, ".git", "index.lock")); err != nil {
		e.t.Fatalf("releasing the index lock: %v", err)
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

func runGit(t testing.TB, dir string, args ...string) (string, int) {
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

// installVaultHook writes an executable hook into the vault's own repo, and
// removeVaultHook takes it away again. A hook is the human's file in the
// human's repo, and obsync sets no core.hooksPath, so this is how a test
// arranges the one thing that fails a local git obsync had every reason to
// expect to succeed.
func (e *vaultEnv) installVaultHook(name, script string) {
	e.t.Helper()

	path := filepath.Join(e.vault, ".git", "hooks", name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		e.t.Fatalf("installing the vault's %s hook: %v", name, err)
	}
}

func (e *vaultEnv) removeVaultHook(name string) {
	e.t.Helper()

	if err := os.Remove(filepath.Join(e.vault, ".git", "hooks", name)); err != nil {
		e.t.Fatalf("removing the vault's %s hook: %v", name, err)
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

	// spent is every settle interval obsync has spent, as against taken, which
	// is every deadline it has waited on. The two are different acts (§6,
	// internal/clock).
	spent []time.Duration

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

	// duringSettle is what happens in the vault while obsync is inside a
	// settle interval — the third writer, or a human still typing. It is the
	// one window a test cannot reach through the clock, because obsync spends
	// it inside a run rather than waiting on it between two, so it is reached
	// through the Sleep that opens it instead.
	//
	// It stays registered until a test replaces it, which is what "still being
	// written" means across more than one run; a test whose writer finishes
	// clears it.
	duringSettle func()
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

// Sleep is the settle interval, and it is the one wait this clock services
// itself rather than leaving for a test to drive: obsync spends it in the
// middle of a sync run, so nothing outside that run is in a position to move
// the clock past it.
//
// Time does not move here, and that is deliberate rather than a shortcut. What
// the settle guard compares is two readings of the *filesystem's* clock, never
// this one, so a path can move across the interval without a nanosecond
// passing here — and leaving this clock where it was keeps every cadence
// deadline a run is measured against exactly where the test put it.
//
// It is kept apart from the deadlines rather than among them, because those are
// the ones a test drives and this is not one — and keeping it apart is also
// what lets a test assert the interval's own number.
func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.spent = append(c.spent, d)
	during := c.duringSettle
	c.mu.Unlock()

	if during != nil {
		during()
	}
}

// settleIntervalsSpent is every gap obsync has put between two readings of the
// filesystem, in order.
func (c *fakeClock) settleIntervalsSpent() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration{}, c.spent...)
}

// duringSettle registers what the vault does while obsync is inside its settle
// interval, which is how a test puts a writer in the one window the guard
// exists to see across. Passing nil is the writer having finished.
func (e *vaultEnv) duringSettle(during func()) {
	e.t.Helper()

	e.clock.mu.Lock()
	defer e.clock.mu.Unlock()
	e.clock.duringSettle = during
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

// vaultFileYet is vaultFile without stopping obsync: what the vault holds at a
// path, for a test that has more clock to drive afterwards.
func (e *vaultEnv) vaultFileYet(path string) string {
	e.t.Helper()

	content, err := os.ReadFile(filepath.Join(e.vault, path))
	if err != nil {
		e.t.Fatalf("the vault holds no %q: %v. obsync said:\n%s", path, err, e.log.String())
	}
	return string(content)
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

	e.onTheLaptop(func(laptop string) { e.writeNoteOnTheLaptop(laptop, path, content) })
}

// writeNoteOnTheLaptop is the other device writing one note, inside an
// onTheLaptop block that is writing several. remoteCommit is the one-note case
// and is this plus the commit and the push.
func (e *vaultEnv) writeNoteOnTheLaptop(laptop, path, content string) {
	e.t.Helper()

	full := filepath.Join(laptop, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatalf("creating the folder for %q on the laptop: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatalf("writing %q on the laptop: %v", path, err)
	}
}

// onTheLaptop is the other device doing whatever the test needs to its own
// clone, and pushing the result as one commit. Writing a note is the common
// case and has its own helper above; deleting one, renaming one, or putting a
// folder where the vault has a file are the acts §4's conflict table is written
// in terms of, and each is a line of ordinary git in a human's own repo.
func (e *vaultEnv) onTheLaptop(act func(laptop string)) {
	e.t.Helper()

	laptop := e.laptopUpToDate()
	act(laptop)
	e.mustGit(laptop, "add", "-A")
	e.mustGit(laptop, e.asAHuman("commit", "--quiet", "-m", "written on the laptop")...)
	e.mustGit(laptop, "push", "--quiet", "file://"+e.remote, "refs/heads/main:refs/heads/main")
}

// renameNote is what a human does in Obsidian when they rename a note: the
// bytes move to a new name, and nothing tells git about it beforehand.
func (e *vaultEnv) renameNote(from, to string) {
	e.t.Helper()

	full := filepath.Join(e.vault, to)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatalf("creating the folder for %q: %v", to, err)
	}
	if err := os.Rename(filepath.Join(e.vault, from), full); err != nil {
		e.t.Fatalf("renaming %q to %q: %v", from, to, err)
	}
}

// conflictCopies is every conflict copy in the vault, found the way a human
// finds one and the way the attention note will (§4): by the filename pattern,
// which is the whole of the recovery state. Vault-relative and sorted, so a
// test reads the same list every run.
func (e *vaultEnv) conflictCopies() []string {
	e.t.Helper()

	var found []string
	err := filepath.WalkDir(e.vault, func(entry string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(e.vault, entry)
		if err != nil {
			return err
		}
		// Only when it is a directory. A `.git` that is a file is a submodule
		// or a linked worktree, and fs.SkipDir for something that is not a
		// directory skips the rest of the vault root rather than the repo.
		if info.IsDir() {
			if relative == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.Contains(relative, "(obsync conflict ") {
			found = append(found, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		e.t.Fatalf("looking for conflict copies in the vault: %v", err)
	}
	sort.Strings(found)
	return found
}

// theConflictCopy is the one copy a test expected obsync to write, and fails
// naming what it found instead.
func (e *vaultEnv) theConflictCopy() string {
	e.t.Helper()

	copies := e.conflictCopies()
	if len(copies) != 1 {
		e.t.Fatalf("the vault holds %d conflict copies (%v), want exactly one. obsync said:\n%s",
			len(copies), copies, e.log.String())
	}
	return copies[0]
}

// remoteTree is every path the remote's tip holds, sorted — the whole of what a
// fresh clone would arrive with.
func (e *vaultEnv) remoteTree() []string {
	e.t.Helper()

	out := e.mustGit(e.remote, "ls-tree", "-r", "-z", "--name-only", "refs/heads/main")
	paths := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	sort.Strings(paths)
	return paths
}

// mergeParents is how many parents the remote's tip commit has. A merge that
// kept both sides has two, which is what makes it a merge rather than obsync
// having picked one.
func (e *vaultEnv) mergeParents() int {
	e.t.Helper()
	e.stop()

	out := strings.TrimSpace(e.mustGit(e.remote, "log", "-1", "--format=%P", "refs/heads/main"))
	if out == "" {
		return 0
	}
	return len(strings.Fields(out))
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

// vaultRef is what a ref in the vault names, or "" when the vault does not hold
// it. It is how the failed-apply anchor is asserted on: the anchor is a ref
// obsync writes and a human deletes, so both its presence and its absence are
// observable state rather than something a test has to ask obsync about (§7).
func (e *vaultEnv) vaultRef(ref string) string {
	e.t.Helper()

	out, exit := e.git(e.vault, "rev-parse", "--verify", "--quiet", ref)
	if exit != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// obsyncRefsOnTheRemote is every ref under refs/obsync/ the remote holds, which
// is the assertion behind "the anchor sits outside refs/heads/ and is never
// pushed": obsync's refspec is one branch in each direction, so a ref of its
// own can never travel (§3, §7).
//
// Split on whitespace rather than NUL, which is the same reading remoteBranches
// below does and is right for the same reason `cat-file --batch-check` is read
// a line at a time: the rule against splitting git's output is about paths, and
// there is no path in this listing. git-check-ref-format(1) forbids a space and
// any control character in a ref name, so the one thing that could hold a
// separator cannot be here.
func (e *vaultEnv) obsyncRefsOnTheRemote() []string {
	e.t.Helper()

	return strings.Fields(e.mustGit(e.remote, "for-each-ref", "--format=%(refname)", "refs/obsync/"))
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

// newVaultWith is newVault with further variables on the config surface set —
// OBSYNC_SIZE_CEILING, which is the one value in §5's area that is configured
// at all, because it is a fact about the remote rather than a taste.
func newVaultWith(t *testing.T, extra ...string) *vaultEnv {
	t.Helper()

	return newVaultReachedBy(t, func(e *vaultEnv) (string, []string) {
		return "file://" + e.remote, extra
	})
}

// vaultAlreadyTracks is the vault an operator brings to obsync with history
// already in it: the paths named are written, committed by the human, and
// pushed, so they are tracked in both the vault and the remote before obsync
// has ever looked. It is the state the churn subset exists for — ignore rules
// only ever affect untracked paths, so a floor entry already in history churns
// forever until something takes it out of the index (§5).
func (e *vaultEnv) vaultAlreadyTracks(paths ...string) {
	e.t.Helper()

	for _, path := range paths {
		// A file the test already wrote keeps its own bytes: this says what the
		// history holds, not what the file says, and a test that wrote a
		// .gitignore before calling this meant the .gitignore it wrote.
		if e.vaultHoldsYet(path) {
			continue
		}
		e.writeNote(path, "what "+path+" held before obsync\n")
	}
	e.mustGit(e.vault, "add", "-A")
	e.mustGit(e.vault, e.asAHuman("commit", "--quiet", "-m", "the vault's own history")...)
	e.pushVaultTo("main")
}

// excludeFile is the repo's own exclude file, which is where obsync writes the
// ignore floor and one of its owned paths (§5, §10).
func (e *vaultEnv) excludeFile() string {
	e.t.Helper()

	content, err := os.ReadFile(filepath.Join(e.vault, ".git", "info", "exclude"))
	if err != nil {
		e.t.Fatalf("reading the repo's exclude file: %v. obsync said:\n%s", err, e.log.String())
	}
	return string(content)
}

// vaultTracks reports whether the vault's index carries a path, which is the
// question `git rm --cached` changes the answer to and nothing else does.
func (e *vaultEnv) vaultTracks(path string) bool {
	e.t.Helper()

	out, code := e.git(e.vault, "ls-files", "--error-unmatch", "--", ":(literal)"+path)
	return code == 0 && strings.TrimSpace(out) != ""
}

// remoteSubjects is every commit subject on the remote's branch, newest first.
// It is how "exactly once, ever" is asserted about a commit obsync makes at
// most one of.
func (e *vaultEnv) remoteSubjects() []string {
	e.t.Helper()

	out := e.mustGit(e.remote, "log", "--format=%s", "refs/heads/main")
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// theDiskRotsTheObjectGitNeedsMost damages one loose object in the vault's own
// object store — the tree at HEAD, which every `git status` has to read — and
// remembers what it held so that a test can put it back.
//
// It is the damage this design was measured against, and it is real rather than
// simulated: the bytes on disk stop being a valid object, and what git does
// about that is git's business. Measured at both matrix points, 2.38.5 and
// 2.52.0: `git status` exits 128, and rebuilding the index does not help,
// because the object the rebuild reads is the damaged one.
func (e *vaultEnv) theDiskRotsTheObjectGitNeedsMost() {
	e.t.Helper()

	oid := strings.TrimSpace(e.mustGit(e.vault, "rev-parse", "HEAD^{tree}"))
	path := filepath.Join(e.vault, ".git", "objects", oid[:2], oid[2:])
	sound, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatalf("reading the vault's HEAD tree at %s to rot it: %v — this test needs a loose "+
			"object, and a packed one is a different kind of damage", path, err)
	}
	e.rotted, e.rottedBytes = path, sound
	// Loose objects are written read-only, which is git saying they are
	// immutable rather than protecting them from a failing disk.
	if err := os.Chmod(path, 0o644); err != nil {
		e.t.Fatalf("making %s writable to rot it: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("bytes that are not an object any more"), 0o644); err != nil {
		e.t.Fatalf("rotting %s: %v", path, err)
	}
}

// theRottedObjectIsRestored is the human's own recovery: the object is sound
// again and nothing else about the repository has changed. It is the whole of
// what a damage freeze needs in order to clear, because obsync clears it by
// retrying the work rather than by being told anything.
func (e *vaultEnv) theRottedObjectIsRestored() {
	e.t.Helper()

	if e.rotted == "" {
		e.t.Fatal("nothing has rotted, so there is nothing to restore")
	}
	if err := os.WriteFile(e.rotted, e.rottedBytes, 0o444); err != nil {
		e.t.Fatalf("restoring %s: %v", e.rotted, err)
	}
}

// theVaultsIndexIsTruncated is the damage an unclean shutdown leaves: the index
// is still there and is no longer an index. It is the one kind of damage obsync
// repairs, because the index is derived state — a cache of HEAD, holding no
// history — and it is the reason the rebuild is unconditional rather than
// triggered by matching git's prose.
//
// Measured at both matrix points: `git status` exits 128 with `fatal:
// .git/index: index file smaller than expected`, which is git's everything-code
// and therefore tells obsync nothing on its own.
func (e *vaultEnv) theVaultsIndexIsTruncated() {
	e.t.Helper()

	path := filepath.Join(e.vault, ".git", "index")
	if err := os.WriteFile(path, []byte("DIRC-and-then-nothing"), 0o644); err != nil {
		e.t.Fatalf("truncating %s: %v", path, err)
	}
}

// aThirdWriterRemovesTheVaultsIndex is the state obsync's own index rebuild can
// leave, reached by the writer obsync cannot see: a human or a script that ran
// `rm .git/index` in the vault, on a repository with nothing else wrong with it
// and no failure streak behind it.
//
// Measured at both matrix points: a missing index is one git reads as *empty*,
// so `git status` exits 0 and reports every tracked path twice — as a staged
// deletion and as untracked. Nothing about that is a failure, which is exactly
// why it is the dangerous one.
func (e *vaultEnv) aThirdWriterRemovesTheVaultsIndex() {
	e.t.Helper()

	if err := os.Remove(filepath.Join(e.vault, ".git", "index")); err != nil {
		e.t.Fatalf("removing the vault's index: %v", err)
	}
}

// vaultStages reports whether the vault's index holds a path staged for a
// commit that HEAD does not have — a human's own `git add`, which the index
// rebuild is what discards.
func (e *vaultEnv) vaultStages(path string) bool {
	e.t.Helper()

	out, code := e.git(e.vault, "diff-index", "--cached", "--name-only", "-z", "HEAD")
	if code != 0 {
		return false
	}
	for _, staged := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if staged == path {
			return true
		}
	}
	return false
}

// writeAttachment writes a file of exactly size bytes in the vault: an
// attachment someone dragged in, which is the only thing in a vault big enough
// for the size ceiling to have an opinion about.
func (e *vaultEnv) writeAttachment(path string, size int) {
	e.t.Helper()

	e.writeNote(path, strings.Repeat("a", size))
}

// theOtherDevicePushesInsideTheRun is the race a push can lose: somebody else
// lands a commit on the remote between obsync's fetch and obsync's push.
//
// It is reached through the settle interval, which is the one window a test can
// act inside a sync run at all (duringSettle) — and it is the right window
// rather than a convenient one. The write side spends it immediately before
// obsync applies the tree it computed against the tip it fetched, so a commit
// landing there is one obsync's push was never going to be a fast-forward of,
// which is exactly what losing a race means.
//
// Each firing is its own commit, because a run spends the guard more than once
// and a second push of the same bytes would be no push at all. It goes on
// firing until a test clears it with duringSettle(nil), which is the other
// device stopping.
func (e *vaultEnv) theOtherDevicePushesInsideTheRun() {
	e.t.Helper()

	landed := 0
	e.duringSettle(func() {
		landed++
		e.remoteCommit("Laptop/note "+strconv.Itoa(landed)+".md", "written on the laptop\n")
	})
}

// remoteRefusesPacksOver is a real receive.maxInputSize on the bare remote: the
// limit git enforces incrementally inside index-pack, which is why a client
// uploads the whole doomed pack every time and why waiting to be sure costs
// real bytes (§7). It is the remote's own setting; obsync neither reads nor
// writes it.
func (e *vaultEnv) remoteRefusesPacksOver(bytes int) {
	e.t.Helper()

	e.mustGit(e.remote, "config", "receive.maxInputSize", strconv.Itoa(bytes))
}

// theVaultBecomesASubmoduleCheckout turns the vault's `.git` from a directory
// into the *file* a submodule or a linked worktree has: the repository moves
// out of the vault and a `gitdir:` pointer takes its place.
//
// It is a shape obsync attaches to deliberately rather than a curiosity —
// resolveOwnedPaths asks git where the repository is for exactly this reason,
// and it names the attention note as one of the writers that depends on the
// answer. Everything obsync does still has to work when the vault root holds
// `.git` as a file it can neither descend into nor own.
func (e *vaultEnv) theVaultBecomesASubmoduleCheckout() {
	e.t.Helper()

	elsewhere := filepath.Join(filepath.Dir(e.vault), "vault.git")
	if err := os.Rename(filepath.Join(e.vault, ".git"), elsewhere); err != nil {
		e.t.Fatalf("moving the repository out of the vault: %v", err)
	}
	// core.worktree is what points the detached repository back at the vault,
	// which is how git itself lays a submodule out.
	e.mustGit(elsewhere, "config", "core.worktree", e.vault)
	pointer := filepath.Join(e.vault, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: "+elsewhere+"\n"), 0o644); err != nil {
		e.t.Fatalf("writing the vault's gitdir pointer: %v", err)
	}
}

// theHumanDeletesInObsidian is a note deleted from inside Obsidian with its own
// trash rather than the system's: the file moves to the vault's `.trash/`,
// which is a real folder in the vault and one the ignore floor covers (§5).
func (e *vaultEnv) theHumanDeletesInObsidian(relative string) {
	e.t.Helper()

	trashed := filepath.Join(e.vault, ".trash", path.Base(relative))
	if err := os.MkdirAll(filepath.Dir(trashed), 0o755); err != nil {
		e.t.Fatalf("creating the vault's trash: %v", err)
	}
	if err := os.Rename(filepath.Join(e.vault, relative), trashed); err != nil {
		e.t.Fatalf("moving %q to the vault's trash: %v", relative, err)
	}
}
