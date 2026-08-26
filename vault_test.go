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
	// supports.
	vault  string
	remote string

	clock *fakeClock
	log   *lockedBuffer

	// wakes is the watcher: the channel that says something happened and never
	// what. It is unbuffered, which is what makes a test deterministic — a
	// second wake cannot be delivered until the run the first one started has
	// finished, because the loop only looks at this channel between runs.
	wakes chan struct{}

	syncLoop *loop.Loop

	// cancel and finished belong to a loop left turning in the background,
	// which is what turn starts and what a test needs when it has to act while
	// a sync run is still in flight.
	cancel   context.CancelFunc
	finished chan struct{}
	turning  bool
	stopped  bool
}

// newVault builds a vault that is already a git repo with one commit, a bare
// remote holding that commit, and an obsync configured to sync them.
//
// The loop is not turning yet. A test drives it one run at a time with wake, or
// leaves it turning in the background with turn — either way, nothing obsync
// does happens while a test is still building the vault it will look at.
func newVault(t *testing.T) *vaultEnv {
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
	if err := os.MkdirAll(env.vault, 0o755); err != nil {
		t.Fatalf("creating the vault: %v", err)
	}
	env.mustGit(env.vault, "init", "--quiet", "-b", "main")
	env.mustGit(env.vault, "remote", "add", config.RemoteName, "file://"+env.remote)

	// An Obsidian vault always has .obsidian/, which is later the vault
	// sentinel (#32). It is here from the start so that every test runs
	// against a vault shaped like a real one.
	env.writeNote(".obsidian/app.json", "{}\n")
	env.mustGit(env.vault, "add", "-A")
	env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "the vault before obsync")...)
	env.mustGit(env.vault, "push", "--quiet", config.RemoteName, "refs/heads/main:refs/heads/main")

	// The configuration comes through the same environment block an operator
	// sets, resolved by the same code that resolves it in production. Its
	// startup line goes nowhere: the buffer below holds what the sync loop
	// says, so a test asserting that a quiet run is quiet is not reading
	// startup's output.
	cfg, _, err := config.Resolve([]string{
		"OBSYNC_REPO=file://" + env.remote,
		"OBSYNC_VAULT_PATH=" + env.vault,
	}, io.Discard)
	if err != nil {
		t.Fatalf("resolving the test configuration: %v", err)
	}

	// Info is the level an operator gets by default, and the level at which
	// "healthy is quiet" is a claim worth checking (§9).
	log := slog.New(slog.NewTextHandler(env.log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	env.syncLoop = loop.New(cfg, log, env.clock, env.wakes)
	t.Cleanup(env.stop)
	return env
}

// wake is one wake-up, and the loop's unit of work: obsync performs exactly one
// sync run for it and this returns when that run is over.
//
// It is one whole turn of the loop, driven with its context already done —
// which is what a SIGTERM arriving during startup is, and what §1 says obsync
// does with one: refuse to start a new run, finish the current one, return.
// Startup runs the loop immediately (§2), so that current run is the sync run
// this wake-up is asking for.
//
// Driving it synchronously is what makes every test below deterministic: the
// vault a run looks at is the vault the test finished building, never one it is
// still writing.
func (e *vaultEnv) wake() {
	e.t.Helper()

	if e.stopped {
		e.t.Fatal("this test woke obsync after stopping it; a stopped loop performs no more runs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.syncLoop.Run(ctx)
}

// turn leaves the loop running in the background, woken by the watcher's
// channel, which is how obsync actually runs: a wake-up arrives, a run happens,
// and nothing looks at the channel again until that run is over.
//
// It is what a test uses when it has to do something while a run is in flight —
// advance the clock past a deadline, say — and it is stopped by the ordinary
// stop, which is the SIGTERM path.
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

// watcherWake hands the turning loop one wake-up. The channel is unbuffered, so
// the send completes only when the loop comes back for it — which it does only
// between runs, because only one sync run is ever in flight (§2).
func (e *vaultEnv) watcherWake() {
	e.t.Helper()

	select {
	case e.wakes <- struct{}{}:
	case <-time.After(30 * time.Second):
		e.t.Fatalf("obsync did not take a wake-up within 30s; it said:\n%s", e.log.String())
	}
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
	taken   int

	// registered carries one value per deadline taken out, so a test can wait
	// for obsync to be waiting rather than sleeping until it probably is.
	registered chan struct{}
}

type sleeper struct {
	due time.Time
	ch  chan time.Time
}

func newFakeClock() *fakeClock {
	// A fixed instant, so nothing in a test depends on the day it runs.
	start := time.Date(2026, 8, 24, 14, 3, 0, 0, time.UTC)
	return &fakeClock{start: start, now: start, registered: make(chan struct{}, 64)}
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
	c.taken++
	c.mu.Unlock()

	select {
	case c.registered <- struct{}{}:
	default:
	}
	return waiter.ch
}

// awaitDeadline blocks until obsync has taken out its next deadline, so that a
// test never advances time past something that has not started waiting yet.
func (c *fakeClock) awaitDeadline(t *testing.T) {
	t.Helper()

	select {
	case <-c.registered:
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

	var still []*sleeper
	for _, waiter := range c.waiting {
		if waiter.due.After(c.now) {
			still = append(still, waiter)
			continue
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

// deadlinesTaken is how many times obsync asked to be timed out. It is the
// whole of §1's asymmetry expressed as a number: a run full of local git
// commands and one network git takes exactly one.
func (c *fakeClock) deadlinesTaken() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.taken
}
