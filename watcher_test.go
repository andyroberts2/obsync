package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/watcher"
)

// The production watcher (#39), driven at seam 1: a real vault, real git, a
// real bare remote, and a real inotify watch per directory. The clock is still
// the injected one, which is the whole of what makes these assertions possible
// — a note that reaches the remote inside four quiet windows reached it because
// something told obsync to look, and the only thing that could have is the
// watcher.
//
// The watcher's own contribution is deliberately unobservable on its own: it
// wakes the loop and never says what changed, so there is nothing to assert
// about it except the latency it buys. That is what every test here measures.

// A human saves a note and obsync notices, rather than waiting out a tick. This
// is the whole point of the watcher existing: latency is the quiet window
// rather than the tick (§1, §2).
func TestAWriteInTheVaultWakesObsyncWithoutWaitingForATick(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "saved from the browser\n")
	env.awaitNoticedWithoutATick("Daily/2026-08-24.md", "saved from the browser\n")

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "saved from the browser\n" {
		t.Errorf("the remote holds %q, want the note the watcher woke obsync for (§1)", got)
	}
}

// A folder made after obsync started is watched like every other one: the set
// of watches is maintained as directories come and go, rather than taken once
// at startup (§1).
//
// The second write is what carries the assertion. inotify reports a file's
// modification against the watch on the directory holding it and nowhere else,
// so a note edited inside a folder obsync never watched produces no event at
// all — the vault root's watch saw the folder appear and has nothing to say
// about anything inside it afterwards.
func TestAFolderMadeAfterObsyncStartedIsWatchedToo(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Projects/plan.md", "the folder did not exist a moment ago\n")
	env.awaitNoticedWithoutATick("Projects/plan.md", "the folder did not exist a moment ago\n")

	env.writeNote("Projects/plan.md", "and now a note inside it changes\n")
	env.awaitNoticedWithoutATick("Projects/plan.md", "and now a note inside it changes\n")

	if got := env.remoteFile("Projects/plan.md"); got != "and now a note inside it changes\n" {
		t.Errorf("the remote holds %q, want the edit inside a folder that was made after obsync "+
			"started watching — the watches are maintained, not taken once (§1)", got)
	}
}

// `mkdir -p Projects/2026/Q3` usually creates all three directories before
// obsync can watch any of them, so the event for the first one is the only
// event that is ever coming — inotifywait -r has the same problem. A watcher
// that watched only the directory it was told about would leave the two below
// it unwatched forever.
//
// "Usually" is measured and is why this test is not the one that pins the walk:
// with the walk removed, obsync sometimes wins the race — it watches `Projects`
// in the gap before `2026` is made, hears that as its own Create, and recovers
// one directory at a time. The move-in test below is the deterministic half of
// this pair, because a folder that arrives by rename produces no event for
// anything inside it at all, ever. Both are kept: this is the shape a human
// makes far more often, and a watcher that can only pass by winning a race is
// one worth having a test fail on when it stops winning.
func TestAFolderTreeCreatedInOneActIsWatchedAllTheWayDown(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Projects/2026/Q3/plan.md", "three folders deep, made in one act\n")
	env.awaitNoticedWithoutATick("Projects/2026/Q3/plan.md", "three folders deep, made in one act\n")

	env.writeNote("Projects/2026/Q3/plan.md", "and the deepest one is watched\n")
	env.awaitNoticedWithoutATick("Projects/2026/Q3/plan.md", "and the deepest one is watched\n")

	if got := env.remoteFile("Projects/2026/Q3/plan.md"); got != "and the deepest one is watched\n" {
		t.Errorf("the remote holds %q, want the edit three folders down — every directory a walk "+
			"finds is watched, not just the one the event named (§1)", got)
	}
}

// A folder dragged into the vault from somewhere else arrives whole, and every
// directory inside it is watched. This is the same maintenance rule as the two
// above and a different fact about the kernel: a rename into a watched
// directory is `IN_MOVED_TO`, not `IN_CREATE`, and nothing inside the folder
// produces an event at all — it was never created here.
//
// It is worth pinning rather than assuming for two reasons. It is the ordinary
// way a person adds an existing set of notes to a vault, and obsync's whole
// risk surface is facts about somebody else's software. And it is the only
// deterministic guard on the walk: measured, a watcher stripped back to
// watching one directory per event still passes the `mkdir -p` test above by
// winning a race, and fails this one every time.
func TestAFolderMovedIntoTheVaultIsWatchedAllTheWayDown(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	// Built outside the vault and moved in one act, which is what makes the
	// arrival a rename: the folder and the note inside it both already exist
	// by the time obsync hears anything.
	elsewhere := filepath.Join(t.TempDir(), "Imported")
	if err := os.MkdirAll(filepath.Join(elsewhere, "2026"), 0o755); err != nil {
		t.Fatalf("building the folder to move in: %v", err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "2026", "plan.md"),
		[]byte("written before it was ever in the vault\n"), 0o644); err != nil {
		t.Fatalf("writing the note to move in: %v", err)
	}
	if err := os.Rename(elsewhere, filepath.Join(env.vault, "Imported")); err != nil {
		t.Fatalf("moving the folder into the vault: %v", err)
	}
	env.awaitNoticedWithoutATick("Imported/2026/plan.md", "written before it was ever in the vault\n")

	env.writeNote("Imported/2026/plan.md", "and now a note inside it changes\n")
	env.awaitNoticedWithoutATick("Imported/2026/plan.md", "and now a note inside it changes\n")

	if got := env.remoteFile("Imported/2026/plan.md"); got != "and now a note inside it changes\n" {
		t.Errorf("the remote holds %q, want the edit inside a folder that was moved into the "+
			"vault rather than created in it — a rename in is a directory arriving (§1)", got)
	}
}

// A folder renamed inside the vault is still watched afterwards, and so is
// everything made inside it after the rename. This is the same maintenance rule
// as the three tests above and the one act that takes a watch away without
// taking the directory away, which is why it is the row that was missing.
//
// Measured, and the reason the walk alone is not enough: inotify reports a
// watched directory moving but never says where to, so fsnotify gives that
// watch back rather than hold one whose path it can no longer state — and it
// does so *after* delivering the Create for the new name, so the walk that
// Create starts is undone a moment later. Without maintain answering the
// rename, `Work` ends up watched by nothing, silently and for the rest of the
// process's life.
//
// Renaming a folder is the most ordinary thing a person does to one, and the
// cost of getting it wrong is the state §1 refuses by name: a vault syncing at
// two speeds with nothing to tell them apart. The two writes below are the two
// halves of that — a note in the renamed folder, and a folder made inside it
// afterwards, which is the compounding half because with no watch on `Work`
// there is no Create for obsync to walk from either.
func TestAFolderRenamedInTheVaultIsStillWatchedAfterwards(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Projects/plan.md", "written before the folder was renamed\n")
	env.awaitNoticedWithoutATick("Projects/plan.md", "written before the folder was renamed\n")

	env.settle()
	if err := os.Rename(filepath.Join(env.vault, "Projects"), filepath.Join(env.vault, "Work")); err != nil {
		t.Fatalf("renaming the folder: %v", err)
	}
	env.awaitNoticedWithoutATick("Work/plan.md", "written before the folder was renamed\n")

	env.settle()
	env.writeNote("Work/plan.md", "and now a note in the renamed folder changes\n")
	env.awaitNoticedWithoutATick("Work/plan.md", "and now a note in the renamed folder changes\n")

	env.settle()
	env.writeNote("Work/2026/deep.md", "a folder made inside the renamed one\n")
	env.awaitNoticedWithoutATick("Work/2026/deep.md", "a folder made inside the renamed one\n")

	if got := env.remoteFile("Work/2026/deep.md"); got != "a folder made inside the renamed one\n" {
		t.Errorf("the remote holds %q, want the note inside a folder made after its parent was "+
			"renamed — a renamed folder keeps its watch, because inotify hands the old one back "+
			"and says nothing about where the folder went (§1)", got)
	}
}

// The vault's `.git` is not vault content and is not watched. It is outside the
// working tree by construction — which is exactly why §6 stages obsync's own
// writes inside it — so nothing that happens in there is ever a change git will
// report.
//
// It is also written by every git obsync runs, which is what this asserts: a
// run that committed and pushed leaves obsync waiting for a tick, not for a
// quiet window. Watching `.git` would make obsync the writer keeping its own
// vault hot — every commit deferring the next one by a quiet window it caused,
// and a bulk import never seeing a quiet vault at all.
//
// This is not a self-write ignore list, which there is none of: obsync's own
// writes into the working tree — the attention note, conflict copies — are
// watched like anyone else's, and the wake they cause finds a clean tree and
// does nothing (§4).
func TestObsyncsOwnCommitDoesNotWakeObsync(t *testing.T) {
	t.Parallel()

	env := newWatchedVault(t)
	env.turn()
	env.awaitIdle()

	// Everything the human's write has to say is said before the run that
	// commits it, so that what arrives afterwards can only be obsync's own.
	env.writeNote("Daily/2026-08-24.md", "one note, and then obsync writes .git\n")
	env.awaitWatcherWake()
	env.settle()

	env.advance(quietWindow)
	if got, held := env.remoteContentYet("Daily/2026-08-24.md"); !held {
		t.Fatalf("the remote does not hold the note this test needs committed and pushed; obsync "+
			"said:\n%s", env.log.String())
	} else if got != "one note, and then obsync writes .git\n" {
		t.Fatalf("the remote holds %q, want the note", got)
	}
	env.settle()

	if waited := env.lastDeadline(); waited < tick-tickJitter {
		t.Errorf("obsync is waiting %s after a run that committed and pushed, want a tick of at "+
			"least %s — a deadline inside the quiet window means obsync woke itself by writing "+
			"`.git`, and `.git` is not vault content (§6)", waited, tick-tickJitter)
	}
}

// settle gives the kernel time to deliver whatever inotify events it was going
// to. It is the one thing in this suite that waits on real time, and it does so
// because it is waiting on the OS rather than on obsync: there is no handshake
// for "nothing more is coming", which is the same reason §1 says silent
// non-delivery is documented rather than detected.
//
// It can only ever under-detect — a longer wait makes an assertion about an
// absent wake-up stronger, never flakier — so a loaded machine weakens this
// check rather than breaking it.
func (e *vaultEnv) settle() {
	e.t.Helper()

	time.Sleep(500 * time.Millisecond)
	// However many wake-ups arrived, they are all in the past now, and so are
	// the deadlines obsync took out for them. Only what happens after this
	// point is what the test is about.
	e.clock.drainWaits()
}

// lastDeadline is the wait obsync most recently took out, which is the one it
// is waiting on now: the loop takes out exactly one at a time (§2).
func (e *vaultEnv) lastDeadline() time.Duration {
	e.t.Helper()

	taken := e.clock.waitsTaken()
	if len(taken) == 0 {
		e.t.Fatal("obsync has taken out no deadline at all")
	}
	return taken[len(taken)-1]
}

// A watcher that cannot watch stands down wholesale and leaves obsync ticking.
// It never refuses to sync and never stops the process: latency degrades to the
// tick and what obsync commits does not change, because every run asks git what
// changed and the watcher never says (§1, §2).
//
// The vault the loop syncs is real and the path the watcher is pointed at is
// not, which is the composition rather than a fake: it is the one way to hold a
// syncable vault beside a watcher that has stood down. The state itself is the
// production one — `ENOSPC` is a vault that is perfectly fine and a kernel with
// no watches left to give — and it is reached here by the failure this sandbox
// can actually produce, because the watch budget is per-UID at 524,288 and
// `/proc/sys` is read-only to an unprivileged container.
//
// The second half is §1's other sentence about this mode, and it is a
// separate claim: **the tick does not shorten to compensate.** A watcher that
// went away is not a reason to poll harder.
func TestAVaultObsyncCannotWatchLeavesItTickingRatherThanStopped(t *testing.T) {
	t.Parallel()

	env := buildVault(t, nil)
	env.watching = true
	unwatchable := filepath.Join(t.TempDir(), "no vault was ever mounted here")
	stoodDown := watcher.Watch(unwatchable, env.logger)
	t.Cleanup(func() {
		if err := stoodDown.Close(); err != nil {
			t.Errorf("closing the watcher: %v", err)
		}
	})
	env.driveWith(stoodDown.Wakes())

	env.turn()
	// Twice, and deterministically so: obsync takes out a deadline, finds the
	// watcher's channel closed, and takes out the next one. That second
	// deadline is tick-only mode being entered, and it is the one the clock
	// below is moved against.
	env.awaitIdle()
	env.awaitIdle()
	env.writeNote("Daily/2026-08-24.md", "nothing is going to report this\n")

	env.advance(tick - tickJitter - time.Second)
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("obsync committed 53 seconds after a change nothing reported, want the tick " +
			"unchanged at 60s ± 10% — a watcher that stood down does not shorten it (§1)")
	}

	// On to 66s, which is the latest a 60s ± 10% tick can fall due.
	env.advance(2*tickJitter + time.Second)
	if got := env.remoteFile("Daily/2026-08-24.md"); got != "nothing is going to report this\n" {
		t.Errorf("the remote holds %q one tick after a change nothing reported, want the note — a "+
			"watcher that cannot watch is tick-only mode, not a stopped obsync (§1)", got)
	}

	said := env.said()
	if !strings.Contains(said, "level=WARN") || !strings.Contains(said, "tick-only mode") {
		t.Errorf("obsync said %q about a vault it cannot watch, want a WARN naming tick-only "+
			"mode: true, self-healing and worth knowing, which is what WARN is for (§9)", said)
	}
}

// obsync's own writes are not suppressed from the watcher, and there is no
// self-write ignore list: a self-triggered wake finds a clean tree and does
// nothing, at the cost of one `git status` (§4).
//
// This is what makes that cost affordable to have chosen. A wake-up over a
// clean vault produces no commit, no push and no log line — an ignore list
// would buy nothing here, and would buy a stale entry eating a real edit that
// landed during the write it was suppressing.
//
// Driven through the fake watcher because the wake-up is the subject: what
// obsync does with one that says nothing is true of every wake-up, whoever's
// write caused it.
func TestAWakeUpOverACleanVaultCommitsNothingAndSaysNothing(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.watcherWake()
	env.advance(quietWindow)

	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits after a wake-up over a clean tree, want %s — the "+
			"wake-up says something happened and git says nothing did (§2, §4)", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits after a wake-up over a clean tree, want %s", got, want)
	}
	if said := env.said(); said != "" {
		t.Errorf("obsync said %q about a wake-up that found nothing, want silence: only runs that "+
			"changed something are INFO, and an empty `docker logs --since 1h` is a designed "+
			"signal (§9)", said)
	}
}

// awaitNoticedWithoutATick advances the clock in quiet windows until the remote
// holds path, and fails if that took long enough for a tick to have been what
// noticed.
//
// Four windows is forty seconds and the earliest a tick can fall due is 54s —
// 60s less its ±10% jitter — so a note that arrives inside this loop arrived
// because obsync was woken. A tick-only obsync never gets here at all: with no
// wake-up the loop's next deadline is the tick, and advancing ten seconds at a
// time moves past nothing.
//
// The loop rather than a single advance is the kernel's, not obsync's: inotify
// delivers when it delivers, and a write can still be producing events after
// the bytes have landed. Every one of those restarts the quiet window, which is
// the quiet window doing its job rather than a race.
func (e *vaultEnv) awaitNoticedWithoutATick(path, want string) {
	e.t.Helper()

	e.awaitWatcherWake()

	const windows = 4
	arrived := false
	before := e.clock.elapsed()
	for range windows {
		if got, held := e.remoteContentYet(path); held && got == want {
			arrived = true
			break
		}
		e.advance(quietWindow)
	}
	if got, held := e.remoteContentYet(path); !arrived && (!held || got != want) {
		e.t.Fatalf("the remote holds %q at %q after %s of quiet, want %q — obsync woken by the "+
			"watcher rather than by a tick; it said:\n%s",
			got, path, windows*quietWindow, want, e.log.String())
	}
	if elapsed := e.clock.elapsed() - before; elapsed >= tick-tickJitter {
		e.t.Errorf("obsync took %s to notice %q, and the earliest a tick can fall due is %s — so a "+
			"tick could have been what noticed it (§2)", elapsed, path, tick-tickJitter)
	}
}

// awaitWatcherWake blocks until obsync takes out a deadline no longer than the
// quiet window, which it only does once a wake-up has moved what is due (§2).
// It is how a test waits for the kernel rather than sleeping until it probably
// has delivered.
func (e *vaultEnv) awaitWatcherWake() {
	e.t.Helper()

	limit := time.After(60 * time.Second)
	for {
		select {
		case waited := <-e.clock.waits:
			if waited > quietWindow {
				continue
			}
			return
		case <-limit:
			e.t.Fatalf("obsync took out no deadline inside the quiet window within 60s, so nothing "+
				"woke it; it said:\n%s", e.log.String())
		}
	}
}
