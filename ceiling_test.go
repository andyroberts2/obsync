package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The two merge ceilings and the closed table's fallback (#31), driven at seam
// 1: the three merge outcomes obsync stops the network half for rather than
// improvising into a commit.
//
// All three are network freezes (§7), and the whole of what that means is
// asserted here as behaviour rather than as a category: nothing is applied to
// the vault, nothing reaches the remote, the local half goes on committing, and
// the freeze clears on its own once the merge stops tripping it.

// §4's table is closed, and this is the act that reaches its fallback without
// anybody doing anything unusual: someone renames a folder in the vault while
// another device adds a note inside it. git resolves that itself — it puts the
// new note in the renamed folder and records a stage so a human can confirm —
// and inheriting git's answer is exactly what "never an improvised resolution"
// forbids.
//
// The tier is the point. A failed run reports an ERROR a tick and stops
// nothing; a network freeze stops the half that would publish a guess and
// leaves the vault being captured.
func TestAConflictOutsideTheTableStopsTheNetworkHalfAndNotTheLocalOne(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Projects/one.md", "Projects/two.md", "Projects/three.md",
		"Projects/four.md", "Projects/five.md", "Projects/six.md")
	env.remoteCommit("Projects/from the laptop.md", "written on the laptop\n")
	theirs := env.remoteTip()
	env.renameNote("Projects/one.md", "Work/one.md")
	env.renameNote("Projects/two.md", "Work/two.md")
	env.renameNote("Projects/three.md", "Work/three.md")
	env.renameNote("Projects/four.md", "Work/four.md")
	env.renameNote("Projects/five.md", "Work/five.md")
	env.renameNote("Projects/six.md", "Work/six.md")

	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-25.md", "written while obsync was network-frozen\n")
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if env.vaultHoldsYet("Work/from the laptop.md") {
		t.Error("the vault holds the laptop's note in the folder git chose for it, want nothing " +
			"applied: a conflict §4's closed table has no row for is never resolved by inheriting " +
			"git's own answer to it (§4)")
	}
	if got := env.remoteTip(); got != theirs {
		t.Errorf("the remote's branch moved to %s, want it still at %s — the network half is "+
			"frozen and nothing leaves (§7)", got, theirs)
	}
	// State entry and state exit each log exactly once (§9). Four runs into a
	// freeze there is one line about it; four runs of a failed run is four,
	// which is the noise a tier exists to prevent.
	if got := strings.Count(env.saidSoFar(), "level=ERROR"); got != 1 {
		t.Errorf("obsync said %d ERROR lines over four runs, want exactly one: a conflict outside "+
			"§4's table is a network freeze, said once on entry, rather than a run that fails "+
			"again every tick (§7, §9). obsync said:\n%s", got, env.saidSoFar())
	}
	if got, want := env.commitsSoFar(env.vault), "4"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a network freeze leaves the local half "+
			"committing, so the folder rename and the note written after it are both captured (§7)",
			got, want)
	}
}

// conflictStormCeiling is §4's first ceiling, restated here on purpose: a test
// that asserts fifty by importing the constant that sets it asserts nothing.
// It is a judgement about human attention rather than a fact about git, which
// is why it is a constant in obsync and a literal here.
const conflictStormCeiling = 50

// Past the storm ceiling, "keep both sides" stops being a kindness: fifty-one
// conflicted paths is not fifty-one people disagreeing about fifty-one notes,
// it is one structural act — a folder moved, a bulk edit, a vault restored over
// itself — and it deserves human eyes before it is baked into a commit.
//
// So nothing is applied at all: no copies, no merge commit, and nothing to the
// remote. The alternative obsync is refusing here is a vault that gains a
// hundred and two notes where it had fifty-one, in one commit, unasked.
func TestAConflictStormFreezesTheNetworkHalfRatherThanBakingItIntoACommit(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	notes := aFolderOf(conflictStormCeiling + 1)
	env.vaultAlreadyTracks(notes...)
	env.bothSidesEdit(notes)
	theirs := env.remoteTip()

	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-25.md", "written while obsync was network-frozen\n")
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if copies := env.conflictCopies(); len(copies) != 0 {
		t.Errorf("obsync wrote %d conflict copies for a conflict storm (%v), want none: past the "+
			"ceiling nothing is applied at all (§4)", len(copies), copies)
	}
	if got, want := env.vaultFileYet(notes[0]), "written in the vault\n"; got != want {
		t.Errorf("the vault holds %q at %q, want %q: nothing was applied, so every note is still "+
			"the one its human left (§4)", got, notes[0], want)
	}
	if got := env.remoteTip(); got != theirs {
		t.Errorf("the remote's branch moved to %s, want it still at %s — a storm stops the network "+
			"half and applies nothing (§4, §7)", got, theirs)
	}
	if got := strings.Count(env.saidSoFar(), "level=ERROR"); got != 1 {
		t.Errorf("obsync said %d ERROR lines over three runs, want exactly one: a conflict storm is "+
			"a network freeze, said once on entry (§7, §9). obsync said:\n%s", got, env.saidSoFar())
	}
	if got, want := env.commitsSoFar(env.vault), "4"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a network freeze leaves the local half "+
			"committing (§7)", got, want)
	}
}

// And the other way round, which is the direction that keeps the ceiling from
// being a cliff: a merge exactly at it is an ordinary merge, kept both sides,
// copies and all.
func TestAMergeAtTheStormCeilingIsKeptBothSidesLikeAnyOther(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	notes := aFolderOf(conflictStormCeiling)
	env.vaultAlreadyTracks(notes...)
	env.bothSidesEdit(notes)

	env.wake()

	if got, want := len(env.conflictCopies()), conflictStormCeiling; got != want {
		t.Errorf("obsync wrote %d conflict copies at the storm ceiling, want %d: the ceiling is "+
			"more than fifty, so fifty is merged like any other divergence (§4)", got, want)
	}
	if got, want := env.remoteFile(notes[0]), "written in the vault\n"; got != want {
		t.Errorf("the remote holds %q at %q, want %q — the merge was pushed (§4)", got, notes[0], want)
	}
}

// aFolderOf is a folder of notes named so that a bulk act on all of them reads
// as one act in the vault rather than as a list — which is what the storm
// ceiling is a judgement about, and what its two directions are both built on.
func aFolderOf(count int) []string {
	notes := make([]string, count)
	for i := range notes {
		notes[i] = fmt.Sprintf("Projects/note %02d.md", i)
	}
	return notes
}

// bothSidesEdit is the divergence itself: the other device writes every one of
// these notes and pushes, and then the vault writes every one of them too. One
// commit on each side, which is what a folder-wide edit or a restore actually
// looks like.
func (e *vaultEnv) bothSidesEdit(notes []string) {
	e.t.Helper()

	e.onTheLaptop(func(laptop string) {
		for _, note := range notes {
			e.writeNoteOnTheLaptop(laptop, note, "written on the laptop\n")
		}
	})
	for _, note := range notes {
		e.writeNote(note, "written in the vault\n")
	}
}

// The storm ceiling counts the paths git recorded stages for, and that is not
// the same as the paths git wrote a message about. `merge-tree` says
// `Auto-merging` for every path it merged *cleanly* — measured at both matrix
// points, fifty-five of them beside a single conflicted path — so a ceiling
// asked of messages would call an ordinary bulk edit a storm and stop a vault
// with nothing wrong with it.
//
// Fifty-five notes edited at opposite ends on the two sides, and one note
// rewritten on both. That is one conflict, one copy, and an ordinary merge.
func TestManyCleanlyMergedNotesBesideOneConflictIsNotAStorm(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	clean := aFolderOf(conflictStormCeiling + 5)
	const rewritten = "Notes/one both sides rewrote.md"
	for _, note := range clean {
		env.writeNote(note, planOf(30, "", ""))
	}
	env.vaultAlreadyTracks(append(append([]string{}, clean...), rewritten)...)

	env.onTheLaptop(func(laptop string) {
		for _, note := range clean {
			env.writeNoteOnTheLaptop(laptop, note, planOf(30, "", "the laptop's own last line"))
		}
		env.writeNoteOnTheLaptop(laptop, rewritten, "written on the laptop\n")
	})
	for _, note := range clean {
		env.writeNote(note, planOf(30, "the vault's own first line", ""))
	}
	env.writeNote(rewritten, "written in the vault\n")

	env.wake()

	if got, want := len(env.conflictCopies()), 1; got != want {
		t.Errorf("obsync wrote %d conflict copies, want %d: only one note was conflicted, and the "+
			"%d others merged cleanly at opposite ends (§4)", got, want, len(clean))
	}
	want := planOf(30, "the vault's own first line", "the laptop's own last line")
	if got := env.remoteFile(clean[0]); got != want {
		t.Errorf("the remote holds %q at a cleanly-merged note, want %q — both sides' edits, merged "+
			"and pushed rather than stopped as a storm (§4)", got, want)
	}
	if got := env.said(); strings.Contains(got, "conflict storm") {
		t.Errorf("obsync said %q, want no storm: the ceiling is a count of conflicted paths, and a "+
			"path git merged cleanly is not one of them (§4)", got)
	}
}

// The second ceiling, and the subtler one. A clean auto-merge blob existed on
// neither side, so it is the only source of new bytes a merge can introduce to
// the remote, and the only route through the merge path to content the remote
// has never accepted. Both sides' own versions passed the ceiling — the vault's
// at the `git add`, the remote's before it ever arrived — and what they merge
// to does not.
//
// Nothing is applied: the vault keeps the version its human is looking at, the
// remote keeps its own, and a human is told rather than a doomed push being
// attempted.
func TestACleanAutoMergeBlobOverTheSizeCeilingFreezesTheNetworkHalf(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeNote("Notes/the plan.md", planOf(30, "", ""))
	env.vaultAlreadyTracks("Notes/the plan.md")
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, "Notes/the plan.md", planOf(30, "", strings.Repeat("t", 299)))
	})
	theirs := env.remoteTip()
	inTheVault := planOf(30, strings.Repeat("v", 299), "")
	env.writeNote("Notes/the plan.md", inTheVault)

	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-25.md", "written while obsync was network-frozen\n")
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if got := env.vaultFileYet("Notes/the plan.md"); got != inTheVault {
		t.Errorf("the vault's note is %d bytes, want the %d the human left: a merge over the size "+
			"ceiling applies nothing at all (§4)", len(got), len(inTheVault))
	}
	if got := env.remoteTip(); got != theirs {
		t.Errorf("the remote's branch moved to %s, want it still at %s — the one blob a merge can "+
			"invent is over the ceiling, so the network half stopped (§4, §7)", got, theirs)
	}
	if got := strings.Count(env.saidSoFar(), "level=ERROR"); got != 1 {
		t.Errorf("obsync said %d ERROR lines over three runs, want exactly one: this is a network "+
			"freeze, said once on entry (§7, §9). obsync said:\n%s", got, env.saidSoFar())
	}
	if got, want := env.commitsSoFar(env.vault), "4"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a network freeze leaves the local half "+
			"committing (§7)", got, want)
	}
}

// planOf is a note two devices edit at opposite ends, which is the shape a
// clean auto-merge has: git takes both edits and the result is bytes neither
// side ever held.
//
// The lines are 20 bytes each, so the arithmetic against a 1KB ceiling is
// stated rather than guessed: 30 lines is 600 bytes, one 300-byte edit takes a
// side to 880, and both edits together take the merge to 1,160.
func planOf(lines int, head, tail string) string {
	var plan strings.Builder
	for i := range lines {
		switch {
		case i == 0 && head != "":
			plan.WriteString(head + "\n")
		case i == lines-1 && tail != "":
			plan.WriteString(tail + "\n")
		default:
			fmt.Fprintf(&plan, "line %02d of the plan\n", i)
		}
	}
	return plan.String()
}

// And the other way round for the second ceiling, which is what keeps it from
// being a cliff either: a merge whose invented blob is *exactly* the ceiling is
// an ordinary merge and is pushed. The ceiling is the largest blob a merge may
// invent, so the comparison is `>` and not `>=` — the same spelling the refusal
// layer applies to the same number at the `git add`, and two spellings of one
// number is one number a human has to reconcile.
//
// The arithmetic is stated rather than guessed, the way planOf's is: 28 middle
// lines is 560 bytes, and a 231-byte line at each end takes each side to 812
// and the merge to exactly 1,024.
func TestAMergeInventingABlobExactlyAtTheSizeCeilingIsMergedLikeAnyOther(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeNote("Notes/the plan.md", planOf(30, "", ""))
	env.vaultAlreadyTracks("Notes/the plan.md")
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, "Notes/the plan.md", planOf(30, "", strings.Repeat("t", 231)))
	})
	env.writeNote("Notes/the plan.md", planOf(30, strings.Repeat("v", 231), ""))

	env.wake()

	want := planOf(30, strings.Repeat("v", 231), strings.Repeat("t", 231))
	if len(want) != 1024 {
		t.Fatalf("the two sides merge to %d bytes, and this test says something about the ceiling "+
			"only while that is exactly the 1024 the ceiling is set to", len(want))
	}
	if got := env.remoteFile("Notes/the plan.md"); got != want {
		t.Errorf("the remote holds %d bytes at the plan, want the %d the two sides merge to: a blob "+
			"exactly at the ceiling is not over it, and is merged and pushed like any other (§4, §5)",
			len(got), len(want))
	}
	if got := env.said(); strings.Contains(got, "level=ERROR") {
		t.Errorf("obsync said %q, want nothing at ERROR: nothing here is over the ceiling", got)
	}
}

// The invented blob is found by intersecting two `diff-tree -r -z` runs and
// matching their answers up **by path**, so a note title a vault may legally
// hold has to survive that intersection intact. A newline is the one that a
// line-split or a `\t`-split loses, and it is a title someone gets by pasting a
// heading into Obsidian's rename box.
//
// It matters in the direction that fails open: a path obsync cannot match
// against the other diff never reaches the intersection at all, and the merge
// that invents an over-ceiling blob there is pushed rather than stopped.
func TestACleanAutoMergeBlobOverTheCeilingIsCaughtAtAPathHoldingANewline(t *testing.T) {
	t.Parallel()

	note := "Notes/Zettel/a [draft] plan\nfür “quoted” note.md"
	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeNote(note, planOf(30, "", ""))
	env.vaultAlreadyTracks(note)
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, note, planOf(30, "", strings.Repeat("t", 299)))
	})
	theirs := env.remoteTip()
	inTheVault := planOf(30, strings.Repeat("v", 299), "")
	env.writeNote(note, inTheVault)

	env.wake()

	if got := env.remoteTip(); got != theirs {
		t.Errorf("the remote's branch moved to %s, want it still at %s — the blob this merge invents "+
			"is over the ceiling, and a note title holding a newline does not hide it from the "+
			"intersection (§4)", got, theirs)
	}
	if got := env.vaultFileYet(note); got != inTheVault {
		t.Errorf("the vault's note is %d bytes, want the %d the human left: a merge over the size "+
			"ceiling applies nothing at all (§4)", len(got), len(inTheVault))
	}
	// Named, rather than merely stopped: a merge obsync could not *read* also
	// leaves the remote where it was, and that is a failed run rather than this
	// freeze. The assertion is the difference between the two (§7, §9).
	if got := env.said(); !strings.Contains(got, `freeze="merged tree over the size ceiling"`) {
		t.Errorf("obsync said %q, want the freeze named: the merged tree was read and the blob it "+
			"invents measured, at a path holding a newline (§4, §9)", got)
	}
}

// A conflict copy is exempt from the ceiling at any size, and that is a
// positive decision rather than an omission (§4). The copy's bytes are the
// losing version of a path, which the remote already holds — so pack
// negotiation never re-sends them and nothing is doubled on either side — and
// obsync already writes over-ceiling files into the vault on any ordinary pull.
// The ceiling has never gated what obsync *receives*.
func TestAConflictCopyIsExemptFromTheSizeCeilingAtAnySize(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.vaultAlreadyTracks("Attachments/scan.png")
	onTheLaptop := strings.Repeat("t", 2048) + "\n"
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, "Attachments/scan.png", onTheLaptop)
	})
	env.writeNote("Attachments/scan.png", "the vault's picture\n")

	env.wake()

	copy := env.theConflictCopy()
	if got := env.vaultFile(copy); got != onTheLaptop {
		t.Errorf("the conflict copy is %d bytes, want the %d the remote holds: a copy is exempt "+
			"from the ceiling at any size, because those bytes are already the remote's (§4)",
			len(got), len(onTheLaptop))
	}
	if got := env.remoteFile(copy); got != onTheLaptop {
		t.Errorf("the remote holds %d bytes at the copy, want %d — the merge was pushed rather "+
			"than stopped by a ceiling that does not apply to it (§4)", len(got), len(onTheLaptop))
	}
	if got, want := env.remoteFile("Attachments/scan.png"), "the vault's picture\n"; got != want {
		t.Errorf("the remote holds %q at the canonical path, want %q (§4)", got, want)
	}
}

// The storm ceiling is checked first, so a merge tripping both is reported as a
// storm. It is the more useful thing to tell a human: a merge that conflicts at
// fifty-one paths *and* invents an oversized blob has one cause, and the count
// is what names it.
func TestAMergeTrippingBothCeilingsIsReportedAsAStorm(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	notes := aFolderOf(conflictStormCeiling + 1)
	env.writeNote("Notes/the plan.md", planOf(30, "", ""))
	env.vaultAlreadyTracks(append(notes, "Notes/the plan.md")...)
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, "Notes/the plan.md", planOf(30, "", strings.Repeat("t", 299)))
	})
	env.bothSidesEdit(notes)
	env.writeNote("Notes/the plan.md", planOf(30, strings.Repeat("v", 299), ""))

	env.wake()

	said := env.said()
	if !strings.Contains(said, `freeze="conflict storm"`) {
		t.Errorf("obsync said %q, want the freeze named as the conflict storm: the storm ceiling is "+
			"checked first, and a merge tripping both is a storm (§4)", said)
	}
	if strings.Contains(said, "merged tree over the size ceiling") {
		t.Errorf("obsync said %q, want one freeze rather than the size ceiling as well: past the "+
			"storm ceiling the merge is not resolved at all (§4)", said)
	}
}

// Every freeze self-clears when its cause is repaired, without exception and
// without a restart (§7) — including one whose cause is not a fact obsync can
// re-check but a merge it has to compute again.
//
// Here the human does what the remedy says: they make their side of the note
// smaller, so what the two sides merge to fits under the ceiling. The next tick
// merges and pushes, and obsync says once that it is back.
func TestTheMergeCeilingFreezeClearsWhenTheMergeStopsTrippingIt(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeNote("Notes/the plan.md", planOf(30, "", ""))
	env.vaultAlreadyTracks("Notes/the plan.md")
	env.onTheLaptop(func(laptop string) {
		env.writeNoteOnTheLaptop(laptop, "Notes/the plan.md", planOf(30, "", strings.Repeat("t", 299)))
	})
	env.writeNote("Notes/the plan.md", planOf(30, strings.Repeat("v", 299), ""))

	env.turn()
	env.awaitIdle()

	env.writeNote("Notes/the plan.md", planOf(30, "a shorter first line", ""))
	env.advance(70 * time.Second)

	got, _ := env.remoteContentYet("Notes/the plan.md")
	if want := planOf(30, "a shorter first line", strings.Repeat("t", 299)); got != want {
		t.Errorf("the remote holds %d bytes at the plan, want the %d the two sides now merge to: a "+
			"freeze whose cause the human repaired releases obsync on the next tick, with no "+
			"restart (§7). obsync said:\n%s", len(got), len(want), env.saidSoFar())
	}
	if !strings.Contains(env.saidSoFar(), `msg="the freeze cleared`) {
		t.Errorf("obsync said %q, want one line saying the freeze cleared: state entry and state "+
			"exit each log exactly once (§9)", env.saidSoFar())
	}
}
