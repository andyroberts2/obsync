package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Reconciliation (#27), driven at seam 1: what a sync run does once it has
// fetched and asked git how the tracked branch stands against its upstream
// counterpart — nothing, push, fast-forward, or hand the divergence on — and
// the one answer that is not a sync state at all, a remote whose history has
// been rewritten underneath obsync.
//
// Every assertion here is about the two repositories and the one directory:
// which bytes the vault holds, which commit each side's branch points at, and
// what obsync said. Never which git ran.

// The other half of bidirectional sync, and user story 9: someone edits a note
// on their laptop, pushes it, and it is in the browser vault within a tick —
// merged, never rebased (§3).
//
// Nothing here diverged, so the merge is a fast-forward and the vault ends on
// the remote's own commit rather than on a merge of it.
func TestANotePushedFromAnotherDeviceArrivesInTheVault(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Notes/from the laptop.md", "written on the other device\n")
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the laptop.md"); got != "written on the other device\n" {
		t.Errorf("the vault holds %q one tick after someone else pushed, want the note they wrote (§3)", got)
	}
	if got, want := env.vaultTip(), env.remoteTip(); got != want {
		t.Errorf("the vault's branch is at %s and the remote's at %s, want the same commit: a vault "+
			"that is only behind fast-forwards, so nothing new is written on either side (§3)", got, want)
	}
}

// Divergence is the designed-for case rather than an anomaly: someone pushed
// from their laptop while the vault was being written to. The merge that keeps
// both sides is computed out of tree and is #30's; what this slice owes is that
// obsync does nothing destructive while it waits for it.
//
// Nothing is destructive here in either direction. The vault keeps committing —
// a diverged remote does not stop the local half — and the remote keeps the
// commit obsync could not fast-forward past, because every write to the remote
// is a fast-forward or it does not happen (§3).
func TestBothSidesChangedIsNeitherPushedNorOverwritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Notes/from the laptop.md", "written on the other device\n")
	theirs := env.remoteTip()
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.advance(70 * time.Second)

	if got, want := env.commitsSoFar(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a diverged remote does not stop the local "+
			"half committing (§2)", got, want)
	}
	if got := env.remoteTip(); got != theirs {
		t.Errorf("the remote's branch moved to %s, want it still at %s: obsync never force-pushes, "+
			"so a diverged branch is left for the merge rather than written over (§3)", got, theirs)
	}
	if env.vaultHoldsYet("Notes/from the laptop.md") {
		t.Error("obsync applied the remote's commit to a diverged vault, want it left for the " +
			"out-of-tree merge that keeps both sides (§4)")
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about a divergence, want no ERROR: both sides changing is normal "+
			"operation, not a failure a human is needed for (§9)", said)
	}
	// Divergence is where a design that pushed to a branch of its own would do
	// it. obsync has no device branch: it pushes straight to the tracked
	// branch, and its steady state when it cannot is one un-merged branch
	// behind rather than a second branch nobody asked for (§3).
	if got, want := env.remoteBranches(), []string{"refs/heads/main"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("the remote holds the branches %v, want %v — obsync pushes straight to the "+
			"tracked branch and never to a device branch (§3)", got, want)
	}
}

// User story 35, and the reason an upstream rewrite is a state of its own: an
// operator force-pushed to purge a leaked secret, and obsync must not put it
// back.
//
// This is the shape where nothing but the detection stands in the way. The
// remote's tip was rewound past a commit the vault holds, so the vault is
// simply *ahead* — classification alone would say push, the push would be an
// ordinary fast-forward, and the remote would accept it (§3).
//
// It is a network freeze rather than a full one: the vault is sound, and only
// its relationship to the remote is not. So the local half keeps committing
// while the human decides which history wins (§7).
func TestAPurgedCommitIsNeverPushedBackToTheRemote(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)
	if !env.remoteHoldsYet("Notes/secret.md") {
		t.Fatal("the remote does not hold the note obsync was meant to have pushed")
	}

	env.remotePurgesItsTip()
	purged := env.remoteTip()
	env.advance(70 * time.Second)

	if env.remoteHoldsYet("Notes/secret.md") {
		t.Error("obsync pushed the purged commit back to the remote, want it stopped: following a " +
			"rewrite is the mirror image of force-pushing, and obsync does neither (§3)")
	}
	if got := env.remoteTip(); got != purged {
		t.Errorf("the remote's branch moved to %s, want it still at %s — obsync writes nothing to a "+
			"remote whose history was rewritten under it (§3)", got, purged)
	}
	if said := env.saidSoFar(); !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about a rewritten remote, want a human told: this is a freeze that "+
			"no amount of waiting repairs (§9)", said)
	}

	// A network freeze, so the vault keeps being captured while nothing leaves
	// or enters (§7).
	env.writeNote("Daily/2026-08-25.md", "written while obsync was network-frozen\n")
	env.advance(70 * time.Second)

	if got, want := env.commitsSoFar(env.vault), "3"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a network freeze leaves the local half "+
			"committing (§7)", got, want)
	}
	if env.remoteHoldsYet("Daily/2026-08-25.md") {
		t.Error("obsync pushed to a remote it is network-frozen against, want nothing leaving (§7)")
	}
}

// The same act with a replacement commit, which is what a purge usually looks
// like: the vault is now diverged rather than ahead, and left alone it would
// classify as ordinary divergence — and the merge would resurrect every commit
// the rewrite removed (§3).
func TestARewrittenRemoteIsNotMistakenForOrdinaryDivergence(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)

	env.remoteRewritesItsHistory()
	rewritten := env.remoteTip()
	env.advance(70 * time.Second)

	if got := env.remoteTip(); got != rewritten {
		t.Errorf("the remote's branch moved to %s, want it still at %s (§3)", got, rewritten)
	}
	if !strings.Contains(env.saidSoFar(), "level=ERROR") {
		t.Errorf("obsync said %q about a rewritten remote, want it named as its own state rather "+
			"than treated as a divergence to merge (§3)", env.saidSoFar())
	}
}

// The record of what obsync last saw the remote hold has to outlive the process
// that saw it. A container restart is the operator's first reflex, and an
// obsync that forgot the rewrite would fetch, classify, and push the purged
// commit straight back — so what obsync knows here comes from the ref's own
// reflog rather than from anything it is holding in memory (§3).
func TestTheUpstreamRewriteFreezeSurvivesARestart(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)
	env.remotePurgesItsTip()
	purged := env.remoteTip()
	env.advance(70 * time.Second)

	mark := len(env.saidSoFar())
	env.restart()
	env.wake()

	if got := env.remoteTip(); got != purged {
		t.Errorf("a restarted obsync moved the remote's branch to %s, want it still at %s: the tip "+
			"obsync last saw survives a restart in that ref's reflog (§3)", got, purged)
	}
	if fresh := env.said()[mark:]; !strings.Contains(fresh, "level=ERROR") {
		t.Errorf("a restarted obsync said %q, want it telling the human about the rewrite again: a "+
			"freeze that a restart clears is a freeze that restarting destroys (§7)", fresh)
	}
}

// Every freeze self-clears when its cause is repaired, without exception and
// without a restart (§7) — including this one, which no amount of waiting
// repairs because the repair is a human deciding which history wins.
//
// Here they take the remote's: they reset the vault's branch onto it, which is
// the one thing obsync will not do for them. What obsync's branch then holds is
// what the remote holds, so there is nothing left for a merge to resurrect, and
// obsync resumes with no restart.
func TestTheRewriteFreezeClearsWhenTheHumanTakesTheRemotesHistory(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)
	env.remotePurgesItsTip()
	env.advance(70 * time.Second)

	env.mustGit(env.vault, "reset", "--hard", "--quiet", "refs/remotes/origin/main")
	env.writeNote("Daily/2026-08-26.md", "written after the history was settled\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-26.md"); got != "written after the history was settled\n" {
		t.Errorf("the remote holds %q once the human settled which history wins, want the note "+
			"obsync deferred: every freeze clears when its cause is repaired, with no restart (§7)", got)
	}
	if env.remoteHolds("Notes/secret.md") {
		t.Error("the purged note came back with the run that cleared the freeze, want it gone: " +
			"clearing the freeze is the human's history decision taking effect, not obsync's (§3)")
	}
	if !strings.Contains(env.said(), "level=INFO msg=\"the freeze cleared") {
		t.Errorf("obsync said %q, want one line saying the freeze cleared: state entry and state "+
			"exit each log exactly once (§9)", env.said())
	}
}

// The second repair obsync's own remedy names, and the one no local ref can
// see: the operator decides the vault's history was the one they meant and puts
// it back on the remote. §7 admits no exception — every freeze self-clears when
// its cause is repaired, with no restart — so a freeze whose remedy an operator
// followed to the letter and which then held anyway would be worse than one
// that never named the remedy at all.
//
// The remote holds what obsync's branch holds again, so there is nothing left
// for a merge to resurrect and nothing for a push to restore. obsync resumes.
func TestTheRewriteFreezeClearsWhenTheHumanPutsTheirHistoryBackOnTheRemote(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)
	env.remotePurgesItsTip()
	env.advance(70 * time.Second)

	env.vaultsHistoryIsForcedBackOntoTheRemote()
	env.writeNote("Daily/2026-08-26.md", "written after the history was settled\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-26.md"); got != "written after the history was settled\n" {
		t.Errorf("the remote holds %q once the human put their history back on it, want the note "+
			"obsync deferred: every freeze clears when its cause is repaired, with no restart and "+
			"without exception (§7)", got)
	}
	if !strings.Contains(env.said(), "level=INFO msg=\"the freeze cleared") {
		t.Errorf("obsync said %q, want one line saying the freeze cleared: an operator who did what "+
			"the remedy told them to do gets obsync back (§9)", env.said())
	}
}

// And the reason that re-check may never be an ordinary fetch: the rewritten
// remote carrying on being written to is exactly when a merge would resurrect
// everything the rewrite removed.
//
// What obsync last saw the remote hold is the remote-tracking ref's own reflog.
// A fetch that moved that ref would overwrite the record, the tip obsync froze
// on would become the tip it "last saw", and the freeze would clear on the
// remote's next commit — putting the purged note straight back. So the freeze
// holds here however far the rewritten history runs on.
func TestTheRewriteFreezeHoldsWhileTheRewrittenRemoteKeepsMoving(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/secret.md", "the token I pasted by mistake\n")
	env.advance(70 * time.Second)
	env.remotePurgesItsTip()
	env.advance(70 * time.Second)

	for run := range 3 {
		env.remoteCommit("Notes/from the laptop.md",
			fmt.Sprintf("the rewritten history carrying on, commit %d\n", run))
		env.advance(70 * time.Second)
	}

	if env.remoteHoldsYet("Notes/secret.md") {
		t.Error("the purged note is back on the remote, want it gone: a rewritten remote gaining " +
			"commits is when a merge would resurrect what the rewrite removed, not when the " +
			"freeze clears (§3)")
	}
	if env.vaultHoldsYet("Notes/from the laptop.md") {
		t.Error("obsync fast-forwarded the vault onto a remote it is frozen against, want the " +
			"network half stopped in both directions (§7)")
	}
	if strings.Contains(env.said(), "the freeze cleared") {
		t.Errorf("obsync said %q, want the freeze still held: nothing about the remote being "+
			"written to again undoes the rewrite (§3)", env.said())
	}
}

// §3's dirty tree, and the shape it really arrives in: someone is typing into
// the very note an incoming change overwrites. The run is abandoned — not an
// error, not a freeze — and the next wake-up starts fresh. There is no second
// commit inside the run and nothing is stashed: stashing would revert the
// working tree to HEAD, so the human's most recent edits would vanish out of
// their open vault for the duration of the merge.
//
// The runs here are ticks while the vault is hot, which is the only way a run
// reaches the merge with the tree still dirty: the quiet window gates the
// commit rather than the run, so a tick fetches and reconciles while the human
// is mid-sentence (§2).
func TestAnIncomingChangeIsNeverAppliedOverANoteBeingWritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.writeNote("Daily/2026-08-24.md", "the version both sides start from\n")
	env.advance(70 * time.Second)

	env.remoteCommit("Daily/2026-08-24.md", "their version of the day\n")
	env.writeNote("Daily/2026-08-24.md", "what I am typing right now\n")
	settled := env.vaultTip()

	// Two and a half minutes of unbroken typing: several ticks, and the quiet
	// window never clears, so no run may commit.
	for range 30 {
		env.watcherWake()
		env.advance(5 * time.Second)
	}

	if got := env.vaultFile("Daily/2026-08-24.md"); got != "what I am typing right now\n" {
		t.Errorf("the vault holds %q, want the bytes the human was typing: an incoming change is "+
			"never applied over a file being written (§3, §6)", got)
	}
	if got := env.vaultTip(); got != settled {
		t.Errorf("the vault's branch moved to %s, want it still at %s — the run is abandoned "+
			"whole rather than half applied (§3)", got, settled)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about abandoning the run, want nothing above debug: an aborted "+
			"run is a transient loss, and making it news is how the signal becomes noise (§7)", said)
	}
}

// And the other half of that rule, which is what keeps it from being a stall:
// the scope is the paths the incoming change touches, not the tree. Someone
// typing into one note does not hold up every change arriving from elsewhere —
// which is what checking the whole tree would do on a vault that is never quiet.
func TestAnIncomingChangeArrivesWhileAnotherNoteIsBeingWritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Notes/from the laptop.md", "written on the other device\n")
	env.writeNote("Daily/2026-08-24.md", "what I am typing right now\n")

	for range 30 {
		env.watcherWake()
		env.advance(5 * time.Second)
	}

	if got := env.vaultFile("Notes/from the laptop.md"); got != "written on the other device\n" {
		t.Errorf("the vault holds %q at the incoming path, want the note that arrived: a note being "+
			"written blocks the paths it is at, and nothing else (§6)", got)
	}
	if got := env.vaultFile("Daily/2026-08-24.md"); got != "what I am typing right now\n" {
		t.Errorf("the vault holds %q where the human was typing, want their bytes untouched", got)
	}
}

// The dirty-tree rule read against a path a vault may legally hold: a note
// title carries spaces and unicode and, on the filesystem obsync runs against,
// may contain a newline. Both sides of the comparison that decides an abort are
// git output — the incoming diff and `git status` — so a newline read as a
// record separator on either side splits one path into two names that match
// nothing, and the abort silently stops happening.
//
// What that costs is in this test's own failure: git refuses the merge anyway,
// so nothing is overwritten, and obsync reports the refusal as an ERROR every
// tick — the noise the abort tier exists to prevent (§7).
func TestTheDirtyTreeRuleHoldsForAPathWithASpaceAUnicodeQuoteAndANewline(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	path := "Notes/[draft] plan\nfür \u201ctea\u201d.md"
	env.writeNote(path, "the version both sides start from\n")
	env.advance(70 * time.Second)

	env.remoteCommit(path, "their version of the note\n")
	env.writeNote(path, "what I am typing right now\n")
	settled := env.vaultTip()

	for range 30 {
		env.watcherWake()
		env.advance(5 * time.Second)
	}

	if got := env.vaultFile(path); got != "what I am typing right now\n" {
		t.Errorf("the vault holds %q, want the bytes the human was typing: a note title is not a "+
			"line, and neither side of the comparison that decides this may read it as one (§3)", got)
	}
	if got := env.vaultTip(); got != settled {
		t.Errorf("the vault's branch moved to %s, want it still at %s — the run is abandoned "+
			"whole rather than half applied (§3)", got, settled)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about abandoning the run, want nothing above debug: git refuses "+
			"this merge either way, and an ERROR a tick is what the abort tier exists to prevent "+
			"(§7)", said)
	}
}

// The same rule against the third writer's own shape: a file the vault has
// never seen before, sitting exactly where an incoming change lands. It is not
// a modification git can compare, it is bytes that exist nowhere else — so
// applying over it would be the one loss this design has no history to undo.
func TestAnIncomingChangeIsNeverAppliedOverAnUntrackedFileAtItsPath(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Notes/from the laptop.md", "written on the other device\n")
	env.writeNote("Notes/from the laptop.md", "the note I had just started, never committed\n")

	for range 30 {
		env.watcherWake()
		env.advance(5 * time.Second)
	}

	if got := env.vaultFile("Notes/from the laptop.md"); got != "the note I had just started, never committed\n" {
		t.Errorf("the vault holds %q, want the human's own untracked bytes: a path git has never "+
			"seen is the one place the vault holds the only copy there is (§3, §6)", got)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q, want nothing above debug: this is an aborted run, and the next "+
			"wake-up commits the new note and reconciles against it (§7)", said)
	}
}

// The tracked branch is resolved once at bootstrap and fixed for the process
// lifetime (§3), so HEAD moving off it mid-life is a repo that has stopped
// making sense: a commit would land on a branch nobody chose while the push
// sent the one obsync resolved. It is a full freeze, which stops the local half
// too (§7).
//
// obsync never runs git checkout after bootstrap, so it does not put this
// right: checking the branch back out would rewrite files the human has open,
// and it is their checkout that clears the freeze — with no restart.
func TestHeadMovingOffTheTrackedBranchIsAFullFreezeUntilItIsCheckedBackOut(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.mustGit(env.vault, "checkout", "--quiet", "-b", "a-branch-the-human-made")
	env.writeNote("Daily/2026-08-24.md", "written while HEAD was elsewhere\n")
	env.advance(70 * time.Second)

	if got, want := env.commitsOnBranchYet(env.vault, "a-branch-the-human-made"), "1"; got != want {
		t.Errorf("the branch the human checked out holds %s commits, want %s: a full freeze stops "+
			"obsync touching the repo at all, on any branch (§7)", got, want)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when HEAD moved off the tracked branch, want it " +
			"parked alive and re-checking (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q entering a full freeze, want a human told (§9)", said)
	}

	env.mustGit(env.vault, "checkout", "--quiet", "main")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while HEAD was elsewhere\n" {
		t.Errorf("the remote holds %q once the human checked the tracked branch back out, want the "+
			"note obsync deferred: every freeze clears when its cause is repaired (§7)", got)
	}
	if !strings.Contains(env.said(), "level=INFO msg=\"the freeze cleared") {
		t.Errorf("obsync said %q, want one line saying the freeze cleared: state entry and state "+
			"exit each log exactly once, and a freeze nothing clears is one nothing can enter "+
			"after it (§9)", env.said())
	}
}
