package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The settle guard, at seam 1 (§6). Every test here is about a file being
// written while obsync looks at it, so every one of them writes a real file in
// a real vault and asserts on what reached a real remote.
//
// The one window a test cannot reach by driving the clock is the settle
// interval itself, because obsync spends it inside a sync run: duringSettle is
// how a writer gets inside it, and it is the third writer this design assumes
// and cannot see.

// settleInterval is §6's number, restated here rather than imported: a test
// that asserts a constant by reading the constant asserts nothing.
const settleInterval = time.Second

// Someone is mid-save when the loop wakes. Their note is left out of *this*
// commit rather than committed in half, and the rest of the vault commits
// around it — because a commit missing a file is a valid state and a commit
// holding torn bytes is not (§6).
func TestANoteStillBeingWrittenIsLeftOutOfTheCommitWhileTheRestOfTheVaultCommits(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "the first half of a sen")
	env.writeNote("Notes/Index.md", "a note nobody is touching\n")
	env.duringSettle(func() {
		env.writeNote("Daily/2026-08-24.md", "the first half of a sentence, and then the second\n")
	})

	env.wake()

	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		content, _ := env.remoteContentYet("Daily/2026-08-24.md")
		t.Errorf("the remote holds %q at a note that was being written while obsync looked at it, "+
			"want it left out of this commit — a torn commit of a note is what the settle guard "+
			"exists to prevent (§6)", content)
	}
	if got, want := env.remoteFile("Notes/Index.md"), "a note nobody is touching\n"; got != want {
		t.Errorf("the remote holds %q at a note nothing was writing, want %q — an unsettled path is "+
			"excluded from the commit and never an aborted run (§6)", got, want)
	}
}

// The other half of the contract, and the half that makes exclusion acceptable
// at all: a torn write self-heals on the following run. The writer finishes,
// the next run finds the path settled, and the whole note reaches the remote
// with nothing for a human to do (§6).
func TestANoteLeftOutOfOneCommitReachesTheRemoteOnTheFollowingRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "the first half of a sen")
	env.duringSettle(func() {
		env.writeNote("Daily/2026-08-24.md", "the first half of a sentence, and then the second\n")
	})
	env.watcherWake()
	env.advance(quietWindow)

	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Fatal("the remote holds a note that was being written while obsync looked at it, want it " +
			"left out of that commit (§6)")
	}

	// The writer stopped. Nothing else about the vault changed, and the next
	// run is an ordinary one.
	env.duringSettle(nil)
	env.advance(70 * time.Second)

	if got, want := env.remoteFile("Daily/2026-08-24.md"),
		"the first half of a sentence, and then the second\n"; got != want {
		t.Errorf("the remote holds %q one run after the writer finished, want %q — a torn write "+
			"self-heals on the following run (§6)", got, want)
	}
}

// The settle interval is one second, and it is a constant rather than a knob:
// more than 3× ignis's own 300ms stability threshold, and 1s against a 10s
// quiet window. A knob here would be a waiver with extra steps, because nothing
// waives the settle guard (§6, §8).
//
// It is the one number in the guard a test can see, because the guard's own
// answer is about the filesystem rather than about the clock.
func TestTheSettleIntervalIsOneSecond(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "one note to look at\n")

	env.wake()
	env.stop()

	spent := env.clock.settleIntervalsSpent()
	if len(spent) == 0 {
		t.Fatal("obsync put no gap between two readings of a note it was about to commit, want one " +
			"settle interval — the guard is two samples across it (§6)")
	}
	for _, gap := range spent {
		if gap != settleInterval {
			t.Errorf("obsync left %s between the settle guard's two samples, want %s (§6)",
				gap, settleInterval)
		}
	}
}

// Two samples, never `now - mtime`. Vaults sit on NFS, SMB and rclone mounts
// where mtime comes from the server's clock, and a path whose mtime is skewed
// into the future would read as unsettled *forever* under a freshness test —
// which, with nothing able to waive the guard, is a note that silently never
// commits (§6).
//
// Two samples are purely relative, so a clock nobody agrees about cannot
// produce one.
func TestANoteWhoseMtimeIsInTheFutureIsCommittedRatherThanUnsettledForEver(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "written by a server whose clock runs fast\n")

	skewed := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(env.vault, "Daily/2026-08-24.md"), skewed, skewed); err != nil {
		t.Fatalf("skewing the note's mtime into the future: %v", err)
	}

	env.wake()

	if got, want := env.remoteFile("Daily/2026-08-24.md"),
		"written by a server whose clock runs fast\n"; got != want {
		t.Errorf("the remote holds %q at a note whose mtime is an hour ahead, want %q — the guard "+
			"is two samples rather than a freshness test, so a skewed clock cannot make a path "+
			"unsettled for ever (§6)", got, want)
	}
}

// The guard is stat-driven rather than watcher-driven, and this is what that
// buys: with the watcher completely dead — tick-only mode, obsync's own
// degraded state (§1) — a note being written is still left out of the commit.
//
// A watcher-derived guard inherits every watcher failure mode and fails *open*,
// precisely when it is needed.
func TestANoteStillBeingWrittenIsLeftOutOfTheCommitWithNoWatcherAtAll(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.watcherGone()

	env.writeNote("Daily/2026-08-24.md", "the first half of a sen")
	env.writeNote("Notes/Index.md", "a note nobody is touching\n")
	env.duringSettle(func() {
		env.writeNote("Daily/2026-08-24.md", "the first half of a sentence, and then the second\n")
	})
	env.advance(70 * time.Second)

	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote holds a note that was being written, in a vault with no watcher at all, " +
			"want it excluded — the guard is stat-driven precisely so that it is correct with the " +
			"watcher dead (§6)")
	}
	if got, want := env.remoteFile("Notes/Index.md"), "a note nobody is touching\n"; got != want {
		t.Errorf("the remote holds %q at a note nothing was writing, want %q (§6)", got, want)
	}
}

// A file that disappears while obsync is looking at it is a path that moved,
// which is an answer the guard already has: it is excluded, silently, and the
// next run does not report it either because git no longer does.
//
// Before the guard this cost a failed run — git refuses an `add` naming a path
// that is not there, and refuses the whole add with it — so a note deleted, or
// an editor's own temporary file swapped away, between `git status` and the
// `git add` took the rest of the vault out of the same commit and said so as an
// error (§7's abort tier, which this is not one of only because there is
// nothing left to abort).
func TestANoteThatDisappearsWhileObsyncIsLookingAtItIsNotAFailedRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "here when git status looked\n")
	env.writeNote("Notes/Index.md", "a note nobody is touching\n")
	env.duringSettle(func() { env.deleteNote("Daily/2026-08-24.md") })

	env.wake()

	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync reported a run in which a note disappeared as a failure, want it excluded "+
			"like anything else that moved (§6); it said:\n%s", said)
	}
	if got, want := env.remoteFile("Notes/Index.md"), "a note nobody is touching\n"; got != want {
		t.Errorf("the remote holds %q at a note nothing touched, want %q — one path vanishing does "+
			"not take the rest of the vault out of the commit (§6)", got, want)
	}
}

// The guard is uniform across notes, attachments and `.obsidian/` config, with
// no duration-based special casing: a 40MB attachment stays unsettled for
// exactly as long as its copy takes, and a torn `appearance.json` is invalid
// JSON that fails silently at a distance on someone else's fresh clone.
//
// One table, three kinds of file, one rule.
func TestTheGuardIsTheSameForANoteAnAttachmentAndObsidianConfig(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		kind      string
		path      string
		beingCopy string
		whole     string
	}{
		{"a note", "Notes/Zettel/Bicameral mind.md", "# Bicam", "# Bicameral mind\n\nAll of it.\n"},
		{"an attachment", "Attachments/diagram.png", "\x89PNG\r\n", "\x89PNG\r\n\x1a\n and the rest of the bytes\n"},
		{"obsidian config", ".obsidian/appearance.json", `{"theme":`, "{\"theme\":\"obsidian\"}\n"},
	} {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			env.writeNote(row.path, row.beingCopy)
			env.writeNote("Notes/Index.md", "a note nobody is touching\n")
			env.duringSettle(func() { env.writeNote(row.path, row.whole) })

			env.wake()

			if env.remoteHoldsYet(row.path) {
				content, _ := env.remoteContentYet(row.path)
				t.Errorf("the remote holds %q at %s that was still being copied, want it excluded — "+
					"the guard is uniform across notes, attachments and .obsidian/ config, with no "+
					"duration-based special casing (§6)", content, row.kind)
			}
			if !env.remoteHoldsYet("Notes/Index.md") {
				t.Errorf("the remote holds nothing at a note nothing was writing, want the rest of "+
					"the vault to commit around %s (§6)", row.kind)
			}
			env.stop()
		})
	}
}

// Stage-verify. The third writer's writes are what no sampling window can
// anticipate: a path can be genuinely cold when sampled and hot during
// `git add`. So the staged paths are re-stat'd against the sample the guard
// finished on, and anything that moved aborts the run (§6).
//
// Aborting is safe here in a way it is not on the read side, because these
// paths were just verified stable across the settle interval — and the abort
// tier reports nothing, so the assertion is that nothing was committed and
// nothing was said.
func TestAPathThatMovesWhileObsyncIsStagingItAbortsTheRunRatherThanCommitting(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "cold when obsync sampled it\n")
	// A hook on the vault's own repo, which is the human's file in the human's
	// repo, and the one place a test can act between obsync's `git add` and
	// anything after it. It writes the note while the add is running, which is
	// exactly what a third writer does and what no sampling window can see
	// coming.
	env.installVaultHook("post-index-change",
		"#!/bin/sh\nprintf 'and hot while it was being staged\\n' >> 'Daily/2026-08-24.md'\n")
	env.watcherWake()
	env.advance(quietWindow)

	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote holds a note that moved on disk while obsync was staging it, want the " +
			"run aborted rather than the bytes committed (§6)")
	}
	if got, want := env.commitsSoFar(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits after a run stage-verify aborted, want %s — an aborted "+
			"run changes nothing (§7)", got, want)
	}
	// The abort tier reports nothing above debug: a transient loss is not news,
	// and making it news is how a signal becomes noise (§7).
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync reported an aborted run above debug, want nothing said (§7); it said:\n%s", said)
	}

	// And it self-heals: the writer is gone, the next run finds the note
	// settled, and the whole of it reaches the remote.
	env.removeVaultHook("post-index-change")
	env.advance(70 * time.Second)

	if got, want := env.remoteFile("Daily/2026-08-24.md"),
		"cold when obsync sampled it\nand hot while it was being staged\n"; got != want {
		t.Errorf("the remote holds %q one run after the writer stopped, want %q — an aborted run is "+
			"one the next tick retries (§7)", got, want)
	}
}

// Write side: all-or-nothing. If any path the incoming change overwrites is
// still being written, the apply does not happen and the run aborts (§6).
//
// Skipping the path instead would leave the vault holding a tree obsync never
// computed, which write-verify turns into a full freeze; applying anyway
// silently eats keystrokes, and write-verify would not catch it, because obsync
// wrote exactly what it intended and it is the *user's* write that is lost.
func TestAnIncomingChangeIsNotAppliedOverAPathThatIsStillBeingWritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// Someone else pushed an edit to the daily note, and the human here is
	// typing into it — but has not saved since obsync last looked, so the tree
	// is clean at the moment the fast-forward is decided.
	env.remoteCommit("Daily/2026-08-24.md", "the edit from the laptop\n")
	env.duringSettle(func() {
		env.writeNote("Daily/2026-08-24.md", "and the human is typing into it right now\n")
	})
	env.advance(70 * time.Second)

	if got, want := env.vaultFileYet("Daily/2026-08-24.md"),
		"and the human is typing into it right now\n"; got != want {
		t.Errorf("the vault holds %q at a path it was writing when the incoming change landed, want "+
			"%q — the apply is all-or-nothing and the run aborts rather than overwriting a file "+
			"being written (§6)", got, want)
	}

	// It abandons the run rather than reporting it, and recomputes next wake:
	// the human's write is committed like any other change and the incoming
	// commit merges into what it left.
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync reported an aborted run as an error, want nothing above debug (§7); it "+
			"said:\n%s", said)
	}

	env.duringSettle(nil)
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if got, want := env.vaultFile("Daily/2026-08-24.md"),
		"and the human is typing into it right now\n"; got != want {
		t.Errorf("the vault holds %q once the writer stopped, want %q — the vault's view of the "+
			"path is what stays at it (§4)", got, want)
	}
}

// The write side's scope is load-bearing: it is the paths the incoming change
// touches, never the whole tree. A vault with one note being continuously
// rewritten would otherwise block every incoming change indefinitely — and a
// vault that is never quiet is the case this design is built for (§6).
func TestAnIncomingChangeArrivesWhileAnUnrelatedNoteIsStillBeingWritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Notes/from the laptop.md", "the edit from the laptop\n")
	env.writeNote("Daily/hot.md", "a note somebody never stops typing into")
	env.duringSettle(func() {
		env.writeNote("Daily/hot.md", "a note somebody never stops typing into, still")
	})
	env.advance(70 * time.Second)

	if got, want := env.vaultFileYet("Notes/from the laptop.md"), "the edit from the laptop\n"; got != want {
		t.Errorf("the vault holds %q at the incoming note, want %q — the write-side guard is scoped "+
			"to the paths the incoming change touches, never the whole tree (§6)", got, want)
	}
	env.stop()
}

// Transient exclusion is silent, because it is latency rather than news.
// Persistent exclusion is news: a path continuously unsettled for ten minutes —
// 2× the max-wait cap, so a legitimately busy file never trips it — is said
// once, and the rest of the vault keeps syncing (§6, §9's WARN row).
func TestAPathUnsettledForTenMinutesIsReportedAndTheOnesBeforeItAreNot(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	rewrites := 0
	env.writeNote("Daily/hot.md", "rewritten every 500ms by a plugin")
	env.duringSettle(func() {
		rewrites++
		env.writeNote("Daily/hot.md", strings.Repeat("rewritten every 500ms by a plugin ", rewrites))
	})

	// Nine minutes of it, and obsync has said nothing: this is latency, and a
	// WARN a tick is how a signal becomes noise.
	for range 9 {
		env.advance(70 * time.Second)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=WARN") {
		t.Errorf("obsync reported a path unsettled for nine minutes, want silence until it stops "+
			"looking transient (§6); it said:\n%s", said)
	}

	// Past ten, and it is said — once, however many more runs turn.
	for range 3 {
		env.advance(70 * time.Second)
	}
	env.stop()

	if got := strings.Count(env.said(), "level=WARN"); got != 1 {
		t.Errorf("obsync said %d WARNs about a path unsettled for twelve minutes, want exactly one "+
			"— state entry is said once (§9); it said:\n%s", got, env.said())
	}
	if !strings.Contains(env.said(), "Daily/hot.md") {
		t.Errorf("obsync's warning does not name the path that is not reaching the remote; it "+
			"said:\n%s", env.said())
	}
}

// obsync's own writes go write-then-rename through `.git/obsync/tmp/`, and a
// crash between the write and the rename leaves debris there. It is swept at
// startup (§6).
//
// Inside `.git` on purpose: the vault's own file watcher already hardcodes
// ignoring it, so no phantom temp file flashes in the user's file tree, and it
// is outside the working tree, so `git status` can never see it and no ignore
// floor entry is needed.
func TestCrashDebrisInObsyncsStagingDirectoryIsSweptAtStartup(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	staging := filepath.Join(env.vault, ".git", "obsync", "tmp")

	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("creating obsync's staging directory: %v", err)
	}
	debris := filepath.Join(staging, "exclude.2748163910")
	if err := os.WriteFile(debris, []byte("half an exclude file"), 0o644); err != nil {
		t.Fatalf("leaving debris in obsync's staging directory: %v", err)
	}
	env.writeNote("Daily/2026-08-24.md", "an ordinary run\n")

	env.wake()
	env.stop()

	if _, err := os.Lstat(debris); err == nil {
		t.Error("obsync's staging directory still holds what a crashed write left in it, want it " +
			"swept at startup (§6)")
	}
	// And the run it happened in is an ordinary run: the sweep runs through
	// the same owned path every obsync write is renamed out of, so a sweep
	// that took the path with it would take the ignore floor and every later
	// write with it too.
	if got, want := env.remoteFile("Daily/2026-08-24.md"), "an ordinary run\n"; got != want {
		t.Errorf("the remote holds %q after a startup that swept crash debris, want %q — the sweep "+
			"clears the debris and leaves the owned path it sits in (§10)", got, want)
	}
}
