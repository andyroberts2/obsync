package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The attention note (§9, #38): the one channel this user actually reads,
// because it is a note in the vault they already have open.
//
// Seam 1 throughout, and every assertion here is about a real file in a real
// vault — that it is there, what it says, that git never sees it, and that it
// goes away on its own when the thing it describes does. The note's exact
// wording is not on the declared surface; what is asserted below is what §9
// obliges it to carry.

// attentionNote is what the vault's attention note says.
//
// It does not stop obsync the way remoteFile and vaultFile do, and it does not
// need to: every caller below reads it after wake or advance, both of which
// return only once obsync has finished reacting and is waiting on the clock
// again. Stopping here instead would make the commonest shape of test — assert
// the note, repair the cause, assert it is gone — need a restart in the middle
// of it.
func (e *vaultEnv) attentionNote() string {
	e.t.Helper()

	content, err := os.ReadFile(filepath.Join(e.vault, attentionNoteName))
	if err != nil {
		e.t.Fatalf("the vault holds no %s: %v. obsync said:\n%s", attentionNoteName, err, e.log.String())
	}
	return string(content)
}

// hasAttentionNote is whether the note exists at all, which is the whole of the
// signal: its presence is the message, so a note that is empty and a note that
// is absent are two different states and only one of them is allowed (§9).
func (e *vaultEnv) hasAttentionNote() bool {
	e.t.Helper()

	_, err := os.Stat(filepath.Join(e.vault, attentionNoteName))
	return err == nil
}

// attentionNoteName is the path §10 declares, spelled here rather than imported
// so that renaming it in the code is visibly a change to the declared surface
// rather than a refactor the tests follow along with.
const attentionNoteName = "obsync-attention.md"

// selfClearing is the highest-value sentence obsync writes anywhere, and the
// freeze section is obliged to close on it: self-clearing design is worth
// nothing if the operator's reflex is to restart the container and destroy the
// diagnosis (§9, §11).
const selfClearing = "This clears on its own once fixed; no restart needed"

// A healthy obsync writes no note at all. Its presence alone is the signal, so
// a vault that is syncing has nothing at its root that a human has to read and
// then decide to ignore (§9).
func TestAHealthyVaultHasNoAttentionNote(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "an ordinary note\n")

	env.wake()

	if env.hasAttentionNote() {
		t.Errorf("the vault holds an attention note after a run that committed and pushed, want "+
			"none: the note's presence alone is the signal, so a healthy vault has no file to "+
			"read (§9). It says:\n%s", env.attentionNote())
	}
}

// A full freeze writes one, and the note carries what the log line carried:
// the freeze's name, the conclusive fact behind it, and the remedy closing on
// the sentence that stops an operator restarting the container.
//
// It also proves the two claims that make the note safe to write at all —
// writing it touches the vault rather than the repo, so a full freeze can still
// write one, and it is in the ignore floor, so it is never tracked and never
// reaches anybody's clone (§9, §5).
func TestAFullFreezeWritesTheNoteAndRepairingItDeletesTheNoteRatherThanEmptyingIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// Gate 4: a human ran a merge in their own vault and walked away from it.
	env.theHumanLeavesAMergeHalfFinished()
	env.advance(70 * time.Second)

	note := env.attentionNote()
	for _, wanted := range []string{
		"an interrupted git operation is in progress",
		"finish it or abort it",
		selfClearing,
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("the attention note does not say %q, want the freeze's name, the conclusive "+
				"fact behind it and the remedy — closing on the one sentence that keeps an "+
				"operator from restarting the container and destroying the diagnosis (§9). It "+
				"says:\n%s", wanted, note)
		}
	}
	// Writing it touches the vault, not the repo. Nothing about it is tracked,
	// so a warning nobody else can act on reaches nobody else.
	if env.vaultTracks(attentionNoteName) {
		t.Error("the vault tracks the attention note, want it untracked: it is in the ignore " +
			"floor because it is reconstructible where it is meaningful and meaningless where it " +
			"is not (§5, §9)")
	}
	if dirty := env.mustGit(env.vault, "status", "--porcelain"); strings.Contains(dirty, attentionNoteName) {
		t.Errorf("`git status` reports the attention note (%q), want git never to see it — it is "+
			"in the ignore floor, so it cannot be committed and cannot conflict (§5)", dirty)
	}

	// The human finishes what they left. Nothing tells obsync so, and nothing
	// has to.
	env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "merge", "--abort")...)
	env.advance(70 * time.Second)

	if env.hasAttentionNote() {
		t.Errorf("the vault still holds an attention note a tick after the freeze cleared, want it "+
			"**deleted rather than emptied**: an empty note is still a note, and the whole signal "+
			"is that the file is there (§9). It says:\n%s", env.attentionNote())
	}
	if !env.remoteHoldsYet("Daily/contested.md") {
		t.Error("the remote did not gain what the human committed once the freeze cleared, want " +
			"obsync back to syncing with no restart (§7)")
	}
}

// §9 fixes the four sections and their order, and the order is the surface:
// live freezes, then outstanding conflict copies, then refused paths, then the
// paths that have stopped looking transient. This drives a vault into all four
// at once.
func TestTheNoteCarriesTheFourSectionsInTheOrderTheSpecFixes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.turn()
	env.awaitIdle()

	if got := env.theConflictCopy(); got == "" {
		t.Fatal("no conflict copy was written, so this test is not standing on the state it is about")
	}

	// A remote that starts refusing, a path obsync will not commit whatever its
	// state, and a note something rewrites every time obsync looks at it.
	env.installHook("pre-receive", "#!/bin/sh\necho 'this repository is frozen' >&2\nexit 1\n")
	env.writeNote("Daily/2026-08-25.md", "this one wants to go out\n")
	env.writeNote("id_rsa", "a key somebody dropped in the vault\n")
	rewrites := 0
	env.writeNote("Daily/hot.md", "rewritten faster than obsync can see it still")
	env.duringSettle(func() {
		rewrites++
		env.writeNote("Daily/hot.md", strings.Repeat("rewritten faster than obsync can see it still ", rewrites))
	})

	// Twelve ticks, which is past the ten minutes a path must keep moving
	// before its exclusion stops looking transient (§6).
	for range 12 {
		env.advance(70 * time.Second)
	}

	note := env.attentionNote()
	sections := []struct {
		heading string
		names   string
	}{
		{heading: "## Freezes", names: "remote rejection"},
		{heading: "## Conflict copies", names: "Daily/2026-08-24" + conflictAt},
		{heading: "## Refused paths", names: "id_rsa"},
		{heading: "## Paths that will not settle", names: "Daily/hot.md"},
	}
	at := -1
	for _, section := range sections {
		found := strings.Index(note, section.heading)
		if found < 0 {
			t.Fatalf("the attention note has no %q section, want all four of §9's: it says:\n%s",
				section.heading, note)
		}
		if found <= at {
			t.Errorf("the attention note's %q section is out of order, want §9's fixed order — "+
				"live freezes, conflict copies, refused paths, paths that have stopped looking "+
				"transient. It says:\n%s", section.heading, note)
		}
		at = found
		if !strings.Contains(note, section.names) {
			t.Errorf("the attention note's %q section does not name %q. It says:\n%s",
				section.heading, section.names, note)
		}
	}
}

// A remote rejection is the one freeze whose conclusive fact was written by
// somebody other than obsync, so its line carries the remote's own words
// **verbatim in a fenced block, labelled as the remote's rather than obsync's**,
// and a remedy that sends the operator to the remote rather than to the vault
// (§7, §9).
func TestARemoteRejectionsLineCarriesTheRemotesOwnWordsInAFencedBlock(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive",
		"#!/bin/sh\necho 'GH001: this vault is over the plan quota' >&2\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "a note the remote will not take\n")

	env.wake()

	note := env.attentionNote()
	if !strings.Contains(note, "GH001: this vault is over the plan quota") {
		t.Errorf("the attention note does not carry the remote's own sentence, want it verbatim: "+
			"obsync relays and never diagnoses, and the remote's words are the only thing an "+
			"operator can act on (§7). It says:\n%s", note)
	}
	// Fenced, so that whatever the remote said is rendered rather than read as
	// markdown, and labelled so that nobody mistakes it for obsync's diagnosis.
	fenced := false
	inside := false
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inside = !inside
			continue
		}
		if inside && strings.Contains(line, "GH001: this vault is over the plan quota") {
			fenced = true
		}
	}
	if !fenced {
		t.Errorf("the remote's words are not inside a fenced block in the attention note, want "+
			"them fenced so they are rendered rather than interpreted (§9). It says:\n%s", note)
	}
	if !strings.Contains(note, "The remote's own words, verbatim") {
		t.Errorf("the attention note does not label the remote's words as the remote's, want it "+
			"to: an operator who reads a guess as a diagnosis goes looking in the wrong place "+
			"(§7). It says:\n%s", note)
	}
	if !strings.Contains(note, "look at the remote rather than at the vault") {
		t.Errorf("the attention note does not say the repair lives on the remote, want it to — an "+
			"operator who goes looking in the vault will find nothing wrong there (§7). It "+
			"says:\n%s", note)
	}
	if !strings.Contains(note, selfClearing) {
		t.Errorf("the rejection's line does not close on %q. It says:\n%s", selfClearing, note)
	}
}

// Gate 9 is the one freeze with a different remedy, and the note is where a
// human reads it: the ref holding the tree obsync meant to apply, and the
// `git update-ref -d` that clears the gate once they have recovered what they
// need (§7, §9).
func TestGate9sLineSaysWhereTheIntendedTreeIsAndToDeleteTheRef(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Daily/from the laptop.md", "the version the laptop pushed\n")
	env.installVaultHook("post-merge",
		"#!/bin/sh\n[ -f 'Daily/from the laptop.md' ] && printf '%s' 'not what obsync applied\n' > 'Daily/from the laptop.md'\nexit 0\n")

	env.advance(70 * time.Second)

	note := env.attentionNote()
	for _, wanted := range []string{
		"refs/obsync/failed-apply",
		"git update-ref -d refs/obsync/failed-apply",
		"obsync attempts no repair of its own",
		selfClearing,
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("gate 9's line in the attention note does not say %q, want the ref holding "+
				"the tree obsync meant to apply and the command that clears the gate — this is "+
				"the one freeze a restart cannot clear and the one a human clears themselves "+
				"(§7, §9). It says:\n%s", wanted, note)
		}
	}
}

// A damage freeze's line is an account of evidence rather than a diagnosis, and
// §9 names every part of it: the failing git argv, git's own first line of
// stderr, the streak count, and free space when it is low.
func TestADamageFreezesLineCarriesTheArgvTheStderrAndTheStreakCount(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1000GB")
	env.turn()
	env.awaitIdle()

	env.theDiskRotsTheObjectGitNeedsMost()
	for range 6 {
		env.advance(70 * time.Second)
	}

	note := env.attentionNote()
	for _, wanted := range []string{
		"6 sync runs in a row",
		"git read-tree HEAD exited 128",
		"inflate",
		"this looks like a corrupt object",
		// The size ceiling is set above the disk this suite runs on, which is
		// the only honest way to reach a nearly-full filesystem at seam 1: the
		// threshold is the ceiling, because below the largest single file
		// obsync would ever hand git it cannot be confident one ordinary commit
		// would fit.
		"less than the largest single file obsync would commit",
		"keep the old .git rather than deleting it",
		selfClearing,
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("the damage freeze's line in the attention note does not say %q, want the "+
				"streak count, the failing argv, git's own words, free space when it is low, and "+
				"the remedy (§9). It says:\n%s", wanted, note)
		}
	}
}

// §9's second section, and story 13: the copy and its canonical partner are
// wikilinked so a human resolves the pair from inside Obsidian, under one line
// of instruction. Resolving it — editing the two together and deleting the copy
// — is the whole of the recovery state, and the note follows them.
func TestEachConflictCopyWikilinksThePairAndResolvingItDeletesTheNote(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.turn()
	env.awaitIdle()

	copied := env.theConflictCopy()
	note := env.attentionNote()
	for _, wanted := range []string{
		"[[" + strings.TrimSuffix(copied, ".md") + "]]",
		"[[Daily/2026-08-24]]",
		"delete the copy",
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("the attention note does not carry %q, want each copy wikilinked beside the "+
				"note it is a copy of, under one line saying to edit the two together and delete "+
				"the copy (§9). It says:\n%s", wanted, note)
		}
	}
	// A conflict is never a freeze and never unhealthy: under the keep-both
	// rule it is normal operation, so the note is the whole of the signal.
	if strings.Contains(note, "## Freezes") {
		t.Errorf("the attention note has a freeze section over an ordinary conflict, want none — "+
			"a conflict is normal operation under the keep-both rule (§9). It says:\n%s", note)
	}

	// The human does what the note asks. The ordinary loop commits the
	// deletion, and the note goes with it in the same run.
	env.deleteNote(copied)
	env.advance(70 * time.Second)

	if env.hasAttentionNote() {
		t.Errorf("the vault still holds an attention note after the human deleted the last "+
			"conflict copy, want it deleted: every section is derived, so the note cannot outlive "+
			"what it describes by more than one run (§9). It says:\n%s", env.attentionNote())
	}
	if env.remoteHoldsYet(copied) {
		t.Error("the remote still holds the conflict copy the human deleted, want the deletion " +
			"committed and pushed like any other edit — the filename is the whole of the recovery " +
			"state, and there is no command to remember (§4)")
	}
	if env.remoteHoldsYet(attentionNoteName) {
		t.Error("the remote holds obsync's attention note, want it never to leave this vault: it " +
			"is derived where it is meaningful and meaningless anywhere else, and tracking it " +
			"would push a warning to readers who cannot act on it (§9)")
	}
}

// The pairing is conflict-copy naming read backwards, and it has to be exact:
// a copy obsync can write and cannot read back is a conflict a human is never
// told about, and a note a human happened to name that way is theirs rather
// than obsync's to claim.
//
// Three things in one vault: a copy carrying the counter a collision inside one
// minute appends, a copy of an attachment rather than a note — where the
// extension stays on the link, because that is what makes Obsidian open the
// image — and a note whose name looks like a copy and is not.
func TestTheNotePairsEveryCopyWithItsOwnPartnerWhateverTheNameLooksLike(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	resolving := "Daily/2026-08-24" + conflictAt + ".md"
	notACopy := "Notes/not (obsync conflict never).md"
	env.writeNote(resolving, "half of my resolution\n")
	env.writeNote(notACopy, "a note a human named to look like one\n")
	env.vaultAlreadyTracks("Daily/2026-08-24.md", "Attachments/diagram.png", resolving, notACopy)
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.remoteCommit("Attachments/diagram.png", "the laptop's image bytes\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.writeNote("Attachments/diagram.png", "the vault's image bytes\n")

	env.wake()

	note := env.attentionNote()
	for _, wanted := range []string{
		// The counter is stripped to find the partner, so a second copy of one
		// note points at that note rather than at a name with a " 2" on it.
		"[[Daily/2026-08-24" + conflictAt + " 2]] — the other version of [[Daily/2026-08-24]]",
		"[[Daily/2026-08-24" + conflictAt + "]] — the other version of [[Daily/2026-08-24]]",
		// An attachment keeps its extension on both halves of the pair.
		"[[Attachments/diagram" + conflictAt + ".png]] — the other version of " +
			"[[Attachments/diagram.png]]",
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("the attention note does not carry %q. It says:\n%s", wanted, note)
		}
	}
	if strings.Contains(note, "obsync conflict never") {
		t.Errorf("the attention note names %q as a conflict copy, want it left alone: the pattern "+
			"is obsync's, and a note a human named that way is theirs (§4). It says:\n%s",
			notACopy, note)
	}
}

// The note is reconciled against live state and written only where the two
// differ. That is not tidiness: obsync's own vault writes are not suppressed
// from the watcher (§4), so a note rewritten on every wake-up would wake the
// loop on every wake-up, for ever, over a state nobody changed.
func TestAnUnchangedNoteIsNotRewritten(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.theHumanLeavesAMergeHalfFinished()
	env.advance(70 * time.Second)

	written := env.attentionNoteWrittenAt()
	for range 3 {
		env.advance(70 * time.Second)
	}
	env.stop()

	if got := env.attentionNoteWrittenAt(); !got.Equal(written) {
		t.Errorf("the attention note was rewritten at %s over three ticks of a freeze that never "+
			"moved, want it left at %s: the note is reconciled against live state and written "+
			"only where they differ, and obsync's own writes are not hidden from the watcher "+
			"(§4, §9)", got, written)
	}
}

// The fourth section's own version of the same rule, and the one place it is
// easiest to lose: a path that has been unsettled for hours is a state that is
// not changing, so the note that names it must not change either.
//
// A line counting upwards would differ on every wake-up, which defeats the byte
// comparison from inside the note's own content. It costs more than a wasted
// write: obsync's own writes are not suppressed from the watcher (§4), so the
// note's write is itself a wake — and on the vault this section is most likely
// to be about, one on an NFS or SMB mount whose churn is a writer on another
// host, inotify sees nothing of that writer and the note's own write is the
// only event there is. A 60s tick would become a permanent quiet-window cycle
// over a vault nobody local is touching.
func TestAPathUnsettledForHoursDoesNotRewriteTheNoteEveryRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	rewrites := 0
	env.writeNote("Daily/hot.md", "rewritten faster than obsync can see it still")
	env.duringSettle(func() {
		rewrites++
		env.writeNote("Daily/hot.md", strings.Repeat("rewritten faster than obsync can see it still ", rewrites))
	})
	env.turn()
	env.awaitIdle()

	// Twelve ticks, which is past the ten minutes a path must keep moving
	// before its exclusion stops looking transient (§6).
	for range 12 {
		env.advance(70 * time.Second)
	}
	if note := env.attentionNote(); !strings.Contains(note, "Daily/hot.md") {
		t.Fatalf("the attention note does not name the path that has been moving for twelve "+
			"ticks, so this test is not standing on the state it is about. It says:\n%s", note)
	}

	written := env.attentionNoteWrittenAt()
	for range 3 {
		env.advance(70 * time.Second)
	}
	env.stop()

	if got := env.attentionNoteWrittenAt(); !got.Equal(written) {
		t.Errorf("the attention note was rewritten at %s over three further ticks of a path that "+
			"has been unsettled throughout, want it left at %s: how long a path has been moving "+
			"is not a change of state, and a note that says it as a number counting upwards wakes "+
			"the loop over and over on a vault nobody changed (§4, §6, §9). It says:\n%s",
			got, written, env.attentionNote())
	}
}

// attentionNoteWrittenAt is when the note last changed on disk, which is what a
// write obsync did not need to make would move.
//
// mtime rather than the file's identity, though every owned path goes through
// write-then-rename (§6) and a rewrite is therefore a new file: inode numbers
// are reused, and the temp file the next run renames into place is very often
// the one the last run's note just freed — measured, so os.SameFile answers
// "yes" across a rewrite that really happened.
func (e *vaultEnv) attentionNoteWrittenAt() time.Time {
	e.t.Helper()

	info, err := os.Stat(filepath.Join(e.vault, attentionNoteName))
	if err != nil {
		e.t.Fatalf("the vault holds no %s: %v. obsync said:\n%s", attentionNoteName, err, e.log.String())
	}
	return info.ModTime()
}

// §9's two log-only carve-outs. Gate 2 is a refusal to manage the directory at
// all, and writing a file into it anyway would be presumptuous: there is no
// ignore floor to keep the note out of a repo that does not exist, and obsync
// has just said it cannot reason about what is in there.
func TestAVaultObsyncRefusesToAdoptGetsNoAttentionNote(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.writeNote("someone else's file.txt", "this folder is not a vault\n")

	env.wake()

	if env.hasAttentionNote() {
		t.Errorf("obsync wrote an attention note into a non-empty directory it refused to adopt, "+
			"want none: gate 2 is a log-only carve-out, because writing into a folder obsync has "+
			"just said it cannot reason about is presumptuous and there is no ignore floor there "+
			"to keep the note out of a repository that does not exist (§9). It says:\n%s",
			env.attentionNote())
	}
	if !strings.Contains(env.saidSoFar(), "the vault holds no repository") {
		t.Errorf("obsync did not say in the log that it refused the directory, want the log to be "+
			"the whole channel for the two carve-outs that have no note (§9). It said:\n%s",
			env.saidSoFar())
	}
}

// A vault whose `.git` is a *file* is a submodule or a linked worktree, and
// obsync attaches to those deliberately — `resolveOwnedPaths` asks git where
// the repository is rather than joining `.git` onto the vault path for exactly
// that reason, and it names the attention note as one of the two writers
// standing on the answer.
//
// The conflict copies section is derived by walking the vault (§4: recovery is
// stateless and the filename pattern *is* the state), so the walk has to
// survive the shape too. A human on a submodule-checked-out vault whose notes
// conflicted is either told about the copies or never told at all, and there is
// no other channel that would tell them.
func TestConflictCopiesAreNamedInAVaultWhoseGitIsAFile(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.theVaultBecomesASubmoduleCheckout()
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	copied := env.theConflictCopy()
	note := env.attentionNote()
	for _, wanted := range []string{
		"## Conflict copies",
		"[[" + strings.TrimSuffix(copied, ".md") + "]]",
		"[[Daily/2026-08-24]]",
	} {
		if !strings.Contains(note, wanted) {
			t.Errorf("the attention note does not carry %q in a vault whose `.git` is a file, "+
				"want the copies found there like anywhere else: the vault is walked for them, "+
				"and a walk that stops at a `.git` it cannot descend into stops at the vault root "+
				"— which is every note in the vault (§4, §9). It says:\n%s", wanted, note)
		}
	}
}

// A vault path may hold spaces, unicode and — legally — a newline (§1), and the
// suite already commits and pushes one. A conflict copy of such a note carries
// the newline into its own name, and a newline in the middle of a markdown list
// item ends the list item: the pair a human is told to edit together stops being
// one line, and the second half reads as prose obsync wrote about nothing.
//
// This asserts the note's structure rather than its wording, which is the
// promise §9 makes: one copy is one line, naming both halves of the pair.
func TestAConflictCopyAtAPathHoldingANewlineIsStillOneLine(t *testing.T) {
	t.Parallel()

	note := "Notes/two\nlines.md"
	env := newVault(t)
	env.writeNote(note, "what it held before obsync\n")
	env.vaultAlreadyTracks(note)
	env.remoteCommit(note, "written on the laptop\n")
	env.writeNote(note, "written in the vault\n")

	env.wake()

	copied := env.theConflictCopy()
	items := conflictCopyItems(t, env.attentionNote())
	if len(items) != 1 {
		t.Fatalf("the attention note's conflict copies section holds %d list items (%q), want "+
			"exactly one for the one copy in the vault: a path holding a newline written into a "+
			"list item ends it, and the rest of the pair becomes a line of its own (§1, §9). The "+
			"note says:\n%s", len(items), items, env.attentionNote())
	}
	// Quoted rather than raw, which is the only way both names survive on one
	// line at all — and quoted with Go's own spelling of the escape git uses
	// for the same reason, so a human can still read the name back.
	for _, half := range []string{strconv.Quote(copied), strconv.Quote(note)} {
		if !strings.Contains(items[0], half) {
			t.Errorf("the attention note's one conflict copy line is %q, want it to name %s — the "+
				"copy and the note it is a copy of are the two names a human edits together, and "+
				"a name a line cannot carry is written so that it can (§1, §9)", items[0], half)
		}
	}
}

// conflictCopyItems is the list items in the note's conflict copies section,
// and it insists on the section being a heading, some prose and then nothing
// but list items — which is what makes it able to catch a line that a path
// broke in half rather than only one that is missing.
func conflictCopyItems(t *testing.T, note string) []string {
	t.Helper()

	_, section, found := strings.Cut(note, "## Conflict copies")
	if !found {
		t.Fatalf("the attention note has no conflict copies section. It says:\n%s", note)
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}

	var items []string
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "- "):
			items = append(items, line)
		case len(items) > 0:
			t.Errorf("the attention note's conflict copies section holds %q after its list has "+
				"started, want the section to be a heading, one line of instruction and then one "+
				"list item per copy: anything else is a list item something ended early (§9)", line)
		}
	}
	return items
}

// The note is the whole of what a human is told about a conflict, so a name it
// cannot pair is a conflict nobody is ever told about — and a vault is a
// hostile place to name things. These are the paths
// TestPathsAVaultReallyHoldsCommitAndPush already commits and pushes, driven
// into a conflict each so that every one of them reaches §9's second section.
//
// Two properties, and they are the two halves of one format: every copy is
// paired with the note it is a copy of — `canonicalOf` is `conflictCopyName`
// read backwards — and every pair is one line, whatever the name holds.
func TestEveryConflictCopyAVaultCanNameIsPairedOnOneLine(t *testing.T) {
	t.Parallel()

	// linkable is whether Obsidian can follow a link to this name at all. Where
	// it cannot, the pair is written as plain paths instead — a broken link
	// reads as obsync having got the human's name wrong, which is worse than a
	// name they have to find themselves.
	hostile := []struct {
		path     string
		linkable bool
	}{
		{path: "Notes/space and ünïcode.md", linkable: true},
		{path: "Notes/[draft] plan.md"},
		{path: "Notes/#tag index.md"},
		{path: "Notes/a ^block ref.md"},
		{path: "Notes/either|or.md"},
		{path: "Notes/*star.md", linkable: true},
		{path: "Notes/trailing space .md", linkable: true},
		{path: "-dash note.md", linkable: true},
		{path: "Attachments/diagram.png", linkable: true},
	}

	env := newVault(t)
	var paths []string
	for i, note := range hostile {
		paths = append(paths, note.path)
		env.writeNote(note.path, fmt.Sprintf("what %d held before obsync\n", i))
	}
	env.vaultAlreadyTracks(paths...)
	env.onTheLaptop(func(laptop string) {
		for i, note := range hostile {
			env.writeNoteOnTheLaptop(laptop, note.path, fmt.Sprintf("note %d on the laptop\n", i))
		}
	})
	for i, note := range hostile {
		env.writeNote(note.path, fmt.Sprintf("note %d in the vault\n", i))
	}

	env.wake()

	copies := env.conflictCopies()
	if len(copies) != len(hostile) {
		t.Fatalf("the vault holds %d conflict copies (%q), want one per conflicted note (%q)",
			len(copies), copies, paths)
	}
	items := conflictCopyItems(t, env.attentionNote())
	if len(items) != len(hostile) {
		t.Fatalf("the attention note's conflict copies section holds %d list items, want one per "+
			"copy in the vault (%d): a copy the note cannot name is a conflict a human is never "+
			"told about (§4, §9). The items are:\n%s", len(items), len(hostile),
			strings.Join(items, "\n"))
	}
	// Every note's own stem, which is in the line whichever way the name is
	// written — a wikilink drops a note's extension and a code span keeps the
	// whole path, and the stem is inside both.
	for _, note := range hostile {
		stem := strings.TrimSuffix(note.path, path.Ext(note.path))
		line := theItemNaming(t, items, stem+conflictAt)
		copied, of, paired := strings.Cut(line, " — the other version of ")
		if !paired || !strings.Contains(copied, stem+conflictAt) || !strings.Contains(of, stem) {
			t.Errorf("the attention note's line for a copy of %q is %q, want the copy beside the "+
				"note it is a copy of: the pairing is the conflict-copy name read backwards, and "+
				"a copy paired with the wrong note sends a human to edit two notes that have "+
				"nothing to do with each other (§4, §9)", note.path, line)
		}
		if linked := strings.Contains(line, "[["); linked != note.linkable {
			t.Errorf("the attention note writes the pair for %q as %q, want it %s: a name "+
				"Obsidian cannot follow is written as a plain path instead, because a link that "+
				"goes nowhere reads as obsync having got the human's own name wrong (§9)",
				note.path, line, map[bool]string{true: "wikilinked", false: "as plain paths"}[note.linkable])
		}
	}
}

// theItemNaming is the one list item naming a copy, and fails when the note
// names it twice or not at all.
func theItemNaming(t *testing.T, items []string, copied string) string {
	t.Helper()

	var found []string
	for _, item := range items {
		if strings.Contains(item, copied) {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the attention note holds %d list items naming %q, want exactly one. The items "+
			"are:\n%s", len(found), copied, strings.Join(items, "\n"))
	}
	return found[0]
}

// The note tells a human to delete the copy, and Obsidian's own way of deleting
// a note is to move it into the vault's `.trash/` — which is in the ignore
// floor, precisely because it is not vault content (§5).
//
// So the copy is still a file in the vault matching the pattern, and §4 says
// the pattern *is* the state. It cannot also be the state inside a folder
// obsync has declared is not part of the vault: the human did exactly what they
// were asked, obsync committed the deletion, and the note has to agree with
// them — otherwise the one channel that reports a conflict never stops
// reporting one that is over, and the file at the vault root that means
// "something needs you" never goes away again.
func TestACopyDeletedTheWayObsidianDeletesItLeavesNoNoteBehind(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.turn()
	env.awaitIdle()

	copied := env.theConflictCopy()
	if !env.hasAttentionNote() {
		t.Fatal("no attention note was written for the conflict, so this test is not standing on " +
			"the state it is about")
	}

	env.theHumanDeletesInObsidian(copied)
	env.advance(70 * time.Second)

	if env.hasAttentionNote() {
		t.Errorf("the vault still holds an attention note after the human deleted the last "+
			"conflict copy the way Obsidian deletes a note, want it gone: `.trash/` is in the "+
			"ignore floor because it is not vault content, so a copy in there is one the human "+
			"has already dealt with — and a note that goes on naming it is a signal that never "+
			"clears over a conflict that is over (§4, §5, §9). It says:\n%s", env.attentionNote())
	}
	if !env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote lost the note the conflict was about, want the deletion of the copy " +
			"committed and nothing else touched (§4)")
	}
	if env.remoteHoldsYet(copied) {
		t.Error("the remote still holds the conflict copy the human deleted, want the deletion " +
			"committed and pushed like any other edit (§4)")
	}
}
