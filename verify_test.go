package main

import (
	"strings"
	"testing"
	"time"
)

// Write-verify and the failed-apply anchor (§7, #33). Seam 1 throughout: a real
// vault, a real bare remote over file://, real git, and every assertion about
// what obsync did or did not do to the two repositories.
//
// The third writer these tests are driven by is a hook in the vault's own repo
// rather than a goroutine racing obsync, and that is what makes them
// deterministic: git runs the hook synchronously at the end of the apply, after
// the working tree has been written and before the command returns, which is
// the one moment a test can name. Measured at both matrix points, 2.38.5 and
// 2.52.0, and the two agree: `merge --ff-only` runs `post-merge` there, and
// `reset --keep` runs `post-index-change` there.

// mangled is what the third writer leaves at a path obsync had just applied
// something else to.
const mangled = "not the bytes obsync applied\n"

// Write-verify on the fast-forward apply: obsync brought the remote's tip into
// the vault, something else overwrote a path it had just written, and what
// obsync will not do is go on as though the vault held the tree it computed.
func TestWriteVerifyFailingAnchorsWhatObsyncComputedAndFreezes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// The other device pushes a note the vault does not have, so this run is
	// only behind: one fast-forward and nothing else.
	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	// The hook fires only once the apply has put the laptop's note in the
	// vault, so nothing it does can be mistaken for something that happened
	// before the apply.
	env.installVaultHook("post-merge",
		"#!/bin/sh\n[ -f 'Daily/from the laptop.md' ] && printf '%s' '"+mangled+"' > 'Daily/from the laptop.md'\nexit 0\n")

	env.advance(70 * time.Second)

	applied := env.remoteTip()
	if got := env.vaultRef("refs/obsync/failed-apply"); got != applied {
		t.Errorf("the failed-apply anchor names %q after write-verify failed, want the commit "+
			"obsync computed and applied, %q: the anchor is what keeps a later gc from pruning "+
			"the one artifact that explains the mess (§7)", got, applied)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q after write-verify failed, want the one ERROR a full freeze "+
			"enters with: write-verify is the only interlock whose failure means obsync can no "+
			"longer trust its own view of the vault (§7, §9)", said)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when write-verify failed, want it parked alive: " +
			"obsync never exits on a sync failure (§7)")
	}

	// Everything stops, including the local half. A run that has just proved it
	// cannot apply a tree correctly is the last thing that should carry on
	// committing the vault it can no longer see straight.
	frozenAt := env.vaultTip()
	env.writeNote("Daily/2026-08-25.md", "written while obsync was frozen\n")
	env.advance(70 * time.Second)

	if got := env.vaultTip(); got != frozenAt {
		t.Errorf("the vault's branch moved to %q while the failed-apply anchor stood, want it left "+
			"at %q: a full freeze stops everything (§7)", got, frozenAt)
	}
	if env.remoteHoldsYet("Daily/2026-08-25.md") {
		t.Error("the remote gained a note written while write-verify's freeze stood, want nothing " +
			"published by an obsync that cannot verify what it applies (§7)")
	}
}

// The other apply, and the one the anchor was designed around: §4's out-of-tree
// merge builds a commit with both parents and puts it in the vault with one
// `reset --keep`. A merge commit obsync could not verify is a real object that
// nothing reaches, so without the anchor a later gc would prune the one artifact
// that could explain and undo the mess (§7).
func TestWriteVerifyFailingOnAMergeAnchorsTheMergeCommitAndPushesNothing(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// Both sides move, which is the designed-for case: the vault edits one note
	// and the other device adds another, so the merge is clean and the only
	// thing wrong with it is what happens after it lands.
	env.writeNote("Daily/2026-08-24.md", "the vault's own note\n")
	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	// The apply is a `reset --keep`, which runs no post-merge hook; what it does
	// run, after the working tree is written, is post-index-change. That hook
	// also fires for the `git add` and the `git commit` of this run's local
	// half, so it is guarded on the one file that exists only once the merge
	// has been applied.
	env.installVaultHook("post-index-change",
		"#!/bin/sh\n[ -f 'Daily/from the laptop.md' ] && printf '%s' '"+mangled+"' > 'Daily/from the laptop.md'\nexit 0\n")

	remoteWas := env.remoteTip()
	env.advance(70 * time.Second)

	merged := env.vaultTip()
	if got := env.vaultRef("refs/obsync/failed-apply"); got != merged {
		t.Errorf("the failed-apply anchor names %q, want the merge commit obsync computed, %q (§7)",
			got, merged)
	}
	if got := len(strings.Fields(env.mustGit(env.vault, "rev-list", "--parents", "-n", "1", merged))); got != 3 {
		t.Errorf("the anchored commit has %d fields on its rev-list --parents line, want 3 — the "+
			"commit and both its parents, which is what makes it the merge obsync built (§4)", got)
	}
	if got := env.remoteTip(); got != remoteWas {
		t.Errorf("the remote's tip moved to %q, want it left at %q: write-verify is the only thing "+
			"between a botched apply and a *pushed* botched apply (§7)", got, remoteWas)
	}
	if got := env.obsyncRefsOnTheRemote(); len(got) != 0 {
		t.Errorf("the remote holds %v under refs/obsync/, want none: the anchor sits outside "+
			"refs/heads/ and obsync's refspec is one branch in each direction, so it is never "+
			"pushed (§3, §7)", got)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q after write-verify failed on a merge, want the one ERROR a full "+
			"freeze enters with (§9)", said)
	}
}

// The anchor obsync wrote itself, and the two things that make it worth writing:
// a restart cannot clear it, and obsync does not clear it either. A latch on
// process lifetime would let an operator's reflex — restart the container —
// clear the one refusal that must not be cleared by anything except a human who
// has looked (§7, §9).
func TestTheAnchorObsyncWroteSurvivesARestartAndOnlyAHumanClearsIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	env.installVaultHook("post-merge",
		"#!/bin/sh\n[ -f 'Daily/from the laptop.md' ] && printf '%s' '"+mangled+"' > 'Daily/from the laptop.md'\nexit 0\n")
	env.advance(70 * time.Second)

	anchored := env.vaultRef("refs/obsync/failed-apply")
	if anchored == "" {
		t.Fatal("write-verify did not anchor anything, so there is no latch to restart into (§7)")
	}
	frozenAt := env.vaultTip()

	// The third writer stops, so nothing about the vault is wrong any more —
	// and that is the point: obsync stays frozen on the ref rather than on
	// whether the fact happens to still be establishable.
	env.removeVaultHook("post-merge")
	env.restart()
	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)

	if got := env.vaultRef("refs/obsync/failed-apply"); got != anchored {
		t.Errorf("the failed-apply anchor names %q after a restart, want %q: obsync never deletes "+
			"it, and a freeze a restart clears is a freeze that restarting destroys the diagnosis "+
			"of (§7, §9)", got, anchored)
	}
	if got := env.vaultTip(); got != frozenAt {
		t.Errorf("the vault's branch is at %q after a restart, want it left at %q: obsync attempts "+
			"no corrective action of any kind after write-verify fails (§7)", got, frozenAt)
	}
	if got := env.vaultFileYet("Daily/from the laptop.md"); got != mangled {
		t.Errorf("the vault holds %q at the path write-verify failed on, want it left exactly as "+
			"obsync found it, %q: obsync does not re-apply, does not reset the vault back, and "+
			"does not repair (§7)", got, mangled)
	}

	// The human looks, recovers what they wanted, and deletes the ref. That is
	// the whole of the clearing mechanism, and obsync picks the vault back up
	// within a tick with no restart.
	env.mustGit(env.vault, "update-ref", "-d", "refs/obsync/failed-apply")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/from the laptop.md"); got != mangled {
		t.Errorf("the remote holds %q once the human deleted the anchor, want the vault's own "+
			"bytes, %q: what obsync does next is commit the tree the human left it, like any "+
			"other edit (§7)", got, mangled)
	}
	if !strings.Contains(env.said(), freezeCleared) {
		t.Errorf("obsync said %q, want one line saying the freeze cleared (§9)", env.said())
	}
}

// The scope, and the reason it is the paths the apply touched rather than the
// whole tree: a vault is a live directory, and a human's own edit at a path the
// incoming change never went near is ordinary. Write-verify firing on it would
// make the freeze that means "obsync cannot trust its own view of the vault"
// mean "somebody is using their vault".
func TestAnEditBesideWhatTheApplyTouchedIsNotAWriteVerifyFailure(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/the human's own note.md", "the note they have open\n")
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	// The same third writer, writing somewhere the fast-forward does not go.
	env.installVaultHook("post-merge",
		"#!/bin/sh\nprintf 'typed while the merge landed\\n' > \"Daily/the human's own note.md\"\nexit 0\n")

	env.advance(70 * time.Second)

	if got := env.vaultRef("refs/obsync/failed-apply"); got != "" {
		t.Errorf("write-verify anchored %q for an edit at a path the apply never touched, want "+
			"nothing: the scope is what the apply was going to change, and everything else in a "+
			"vault is a human using it (§6, §7)", got)
	}
	if said := env.saidSoFar(); strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q, want no freeze at all (§7)", said)
	}

	// And the edit is picked up as what it is: the next run commits it.
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/the human's own note.md"); got != "typed while the merge landed\n" {
		t.Errorf("the remote holds %q, want the human's edit committed like any other (§2)", got)
	}
}

// noteWithANewline is a note title a vault may legally hold, and the reason
// write-verify's listing is read NUL-separated: git C-quotes a name like this
// onto a single line without -z, so a line-splitting read would not recognise
// the path at all and would report the apply as verified.
const noteWithANewline = "Daily/a [draft] plan\nfür \"quoted\" note.md"

func TestWriteVerifyFailingAtAPathHoldingANewlineIsStillCaught(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit(noteWithANewline, "the version the laptop pushed\n")
	// Single quotes carry the newline, the brackets and the double quotes
	// through sh unaltered, which is the same thing the vault does with them.
	quoted := "'" + noteWithANewline + "'"
	env.installVaultHook("post-merge",
		"#!/bin/sh\n[ -f "+quoted+" ] && printf '%s' '"+mangled+"' > "+quoted+"\nexit 0\n")

	remoteWas := env.remoteTip()
	env.advance(70 * time.Second)

	if got := env.vaultRef("refs/obsync/failed-apply"); got != remoteWas {
		t.Errorf("the failed-apply anchor names %q for an apply that went wrong at a note title "+
			"holding a newline, want the commit obsync applied, %q: obsync never splits git's "+
			"output into lines, because a vault path may legally hold one (§1)", got, remoteWas)
	}
	if said := env.saidSoFar(); !strings.Contains(said, "für") {
		t.Errorf("obsync said %q, want the freeze to name the path it could not verify: a merge "+
			"obsync could not *read* also leaves the remote where it was, and that is a failed "+
			"run rather than this freeze (§7, §9)", said)
	}
}

// The other half of write-verify, and the one HEAD alone answers: an apply git
// reported as done that left the vault's branch somewhere other than the commit
// obsync put there. The working tree holds the applied tree and the branch does
// not, which is a vault obsync has no account of — and the account is exactly
// what the anchor preserves.
func TestAnApplyThatLeftHeadSomewhereElseIsAWriteVerifyFailure(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	was := env.vaultTip()

	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	// A third writer at a terminal in the vault, putting the branch back where
	// it was after git had moved it.
	env.installVaultHook("post-merge", "#!/bin/sh\ngit update-ref refs/heads/main "+was+"\nexit 0\n")

	applied := env.remoteTip()
	env.advance(70 * time.Second)

	if got := env.vaultRef("refs/obsync/failed-apply"); got != applied {
		t.Errorf("the failed-apply anchor names %q, want the commit obsync applied, %q: the vault's "+
			"branch is at %q, so the tree in the vault is not one obsync can account for (§7)",
			got, applied, env.vaultTip())
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q, want the one ERROR a full freeze enters with (§9)", said)
	}
}
