package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Conflicts (#30), driven at seam 1: what obsync does when both sides changed
// the same note, which in this design is the normal case rather than an
// anomaly.
//
// Every assertion here is about the two repositories and the one directory —
// which bytes the vault holds at which path, which of them reached the remote,
// and what obsync said. Never which git ran, and never which stage of a
// merge-tree said what.
//
// The clock is frozen at the instant every vault in this suite starts from, so
// the name a conflict copy lands under is a fact a test may state in full
// rather than a pattern it has to match loosely. That is the point of the name:
// the pattern *is* the recovery state.
const conflictAt = " (obsync conflict 2026-08-24 1403)"

// baseOf is what vaultAlreadyTracks leaves at a path: the version both sides
// start from, and therefore the merge base of every row below.
func baseOf(path string) string { return "what " + path + " held before obsync\n" }

// User stories 10, 11 and 12, and the whole of §4 in one run: someone edited
// the daily note on their laptop while the vault was being written to.
//
// The vault's version stays exactly where it is — that is story 11, the file
// not changing under a cursor — the remote's lands beside it as an ordinary
// note Obsidian renders and links, and both reach the remote in one commit.
func TestBothSidesChangedKeepsTheVaultsVersionAndACopyOfTheRemotes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	if got, want := env.vaultFile("Daily/2026-08-24.md"), "written in the vault\n"; got != want {
		t.Errorf("the vault holds %q at the canonical path, want %q: the vault's view of the path "+
			"wins, and the file does not change under the human's cursor (§4)", got, want)
	}
	copy := "Daily/2026-08-24" + conflictAt + ".md"
	if got, want := env.theConflictCopy(), copy; got != want {
		t.Errorf("obsync wrote the conflict copy at %q, want %q — beside the canonical path, in the "+
			"same folder, with the extension preserved so Obsidian renders and links it (§4)", got, want)
	}
	if got, want := env.vaultFile(copy), "written on the laptop\n"; got != want {
		t.Errorf("the conflict copy holds %q, want %q byte for byte: no injected frontmatter and no "+
			"provenance header, because mutating the content is how marker corruption sticks (§4)", got, want)
	}
	if got, want := env.remoteFile("Daily/2026-08-24.md"), "written in the vault\n"; got != want {
		t.Errorf("the remote holds %q at the canonical path, want %q", got, want)
	}
	if got, want := env.remoteFile(copy), "written on the laptop\n"; got != want {
		t.Errorf("the remote holds %q at the conflict copy, want %q — the copy is committed inside "+
			"the merge commit, because untracked would leave the remote's bytes on one box (§4)", got, want)
	}
}

// The one thing this policy exists to prevent: the merge is computed out of
// tree, so a conflicted state never exists in the vault at all and no note ever
// holds a marker — not at the canonical path, not in the copy, and not
// anywhere else in the vault or in the commit that reached the remote.
func TestConflictMarkersNeverReachANote(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "line one\nwritten on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "line one\nwritten in the vault\n")

	env.wake()

	for path, content := range env.everyNote() {
		for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
			if strings.Contains(content, marker) {
				t.Errorf("the vault's %q holds a conflict marker (%s). The merge is computed out of "+
					"tree precisely so that a conflicted state never exists in the vault at all (§4)",
					path, marker)
			}
		}
	}
	for _, path := range env.remoteTree() {
		content := env.remoteFile(path)
		if strings.Contains(content, "<<<<<<<") {
			t.Errorf("the remote's %q holds a conflict marker; the tree merge-tree hands back is "+
				"never committed as it stands (§4)", path)
		}
	}
}

// §4's table, row by row. The table is closed and it is the whole of obsync's
// conflict policy, so a row added to the spec without a row added here is an
// incomplete change.
//
// One rule underlies every line of it: the vault's view of the canonical path
// wins — including absence — and the remote's losing bytes become a conflict
// copy.
func TestEveryRowOfTheConflictTableKeepsBothSides(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name string
		// base is what both sides start from, tracked and pushed.
		base []string
		// inTheVault and onTheRemote are the two edits that diverge.
		inTheVault  func(*vaultEnv)
		onTheRemote func(*vaultEnv)
		// holds is what the vault must hold afterwards, path by path, and
		// absent is what it must not hold at all.
		holds  map[string]string
		absent []string
		// copies is every conflict copy the row expects, by full name.
		copies map[string]string
	}{
		{
			name: "content: the vault keeps the path and the remote becomes a copy",
			base: []string{"Daily/2026-08-24.md"},
			inTheVault: func(e *vaultEnv) {
				e.writeNote("Daily/2026-08-24.md", "written in the vault\n")
			},
			onTheRemote: func(e *vaultEnv) {
				e.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
			},
			holds:  map[string]string{"Daily/2026-08-24.md": "written in the vault\n"},
			copies: map[string]string{"Daily/2026-08-24" + conflictAt + ".md": "written on the laptop\n"},
		},
		{
			name: "add/add: as content, with no base to merge against",
			inTheVault: func(e *vaultEnv) {
				e.writeNote("Notes/Bicameral mind.md", "started in the vault\n")
			},
			onTheRemote: func(e *vaultEnv) {
				e.remoteCommit("Notes/Bicameral mind.md", "started on the laptop\n")
			},
			holds:  map[string]string{"Notes/Bicameral mind.md": "started in the vault\n"},
			copies: map[string]string{"Notes/Bicameral mind" + conflictAt + ".md": "started on the laptop\n"},
		},
		{
			name: "modify/delete: the file stays and there is no copy",
			base: []string{"Daily/2026-08-24.md"},
			inTheVault: func(e *vaultEnv) {
				e.writeNote("Daily/2026-08-24.md", "still being written in the vault\n")
			},
			onTheRemote: func(e *vaultEnv) {
				e.onTheLaptop(func(laptop string) {
					if err := os.Remove(filepath.Join(laptop, "Daily/2026-08-24.md")); err != nil {
						e.t.Fatalf("deleting the note on the laptop: %v", err)
					}
				})
			},
			holds:  map[string]string{"Daily/2026-08-24.md": "still being written in the vault\n"},
			copies: map[string]string{},
		},
		{
			name: "delete/modify: the deletion stands and the remote becomes a copy",
			base: []string{"Daily/2026-08-24.md"},
			inTheVault: func(e *vaultEnv) {
				e.deleteNote("Daily/2026-08-24.md")
			},
			onTheRemote: func(e *vaultEnv) {
				e.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
			},
			absent: []string{"Daily/2026-08-24.md"},
			copies: map[string]string{"Daily/2026-08-24" + conflictAt + ".md": "written on the laptop\n"},
		},
		{
			name: "rename/rename: both names exist, each with its own side's content",
			base: []string{"Daily/2026-08-24.md"},
			inTheVault: func(e *vaultEnv) {
				e.renameNote("Daily/2026-08-24.md", "Daily/named in the vault.md")
			},
			onTheRemote: func(e *vaultEnv) {
				e.onTheLaptop(func(laptop string) {
					if err := os.Rename(filepath.Join(laptop, "Daily/2026-08-24.md"),
						filepath.Join(laptop, "Daily/named on the laptop.md")); err != nil {
						e.t.Fatalf("renaming the note on the laptop: %v", err)
					}
				})
			},
			holds: map[string]string{
				"Daily/named in the vault.md":  baseOf("Daily/2026-08-24.md"),
				"Daily/named on the laptop.md": baseOf("Daily/2026-08-24.md"),
			},
			absent: []string{"Daily/2026-08-24.md"},
			copies: map[string]string{},
		},
		{
			name: "file/directory: the remote put a folder where the vault has a note",
			base: []string{"Notes/thing"},
			inTheVault: func(e *vaultEnv) {
				e.writeNote("Notes/thing", "still a note in the vault\n")
			},
			onTheRemote: func(e *vaultEnv) {
				e.onTheLaptop(func(laptop string) {
					if err := os.Remove(filepath.Join(laptop, "Notes/thing")); err != nil {
						e.t.Fatalf("removing the note on the laptop: %v", err)
					}
					if err := os.MkdirAll(filepath.Join(laptop, "Notes/thing"), 0o755); err != nil {
						e.t.Fatalf("making the folder on the laptop: %v", err)
					}
					if err := os.WriteFile(filepath.Join(laptop, "Notes/thing/inner.md"),
						[]byte("a folder on the laptop\n"), 0o644); err != nil {
						e.t.Fatalf("writing inside the folder on the laptop: %v", err)
					}
				})
			},
			holds:  map[string]string{"Notes/thing/inner.md": "a folder on the laptop\n"},
			copies: map[string]string{"Notes/thing" + conflictAt: "still a note in the vault\n"},
		},
		{
			name: "file/directory: the vault put a folder where the remote has a note",
			base: []string{"Notes/thing"},
			inTheVault: func(e *vaultEnv) {
				e.deleteNote("Notes/thing")
				e.writeNote("Notes/thing/inner.md", "a folder in the vault\n")
			},
			onTheRemote: func(e *vaultEnv) {
				e.remoteCommit("Notes/thing", "still a note on the laptop\n")
			},
			holds:  map[string]string{"Notes/thing/inner.md": "a folder in the vault\n"},
			copies: map[string]string{"Notes/thing" + conflictAt: "still a note on the laptop\n"},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			if len(row.base) > 0 {
				env.vaultAlreadyTracks(row.base...)
			} else {
				// Something has to be in the remote for the other device to
				// clone, and add/add starts from a base holding neither side's
				// new note.
				env.vaultAlreadyTracks("Notes/Index.md")
			}
			row.onTheRemote(env)
			row.inTheVault(env)

			env.wake()

			for path, want := range row.holds {
				if got := env.vaultFile(path); got != want {
					t.Errorf("the vault holds %q at %q, want %q (§4)", got, path, want)
				}
				if got := env.remoteFile(path); got != want {
					t.Errorf("the remote holds %q at %q, want %q — one merge commit, one atomic "+
						"state (§4)", got, path, want)
				}
			}
			for _, path := range row.absent {
				if env.vaultHoldsYet(path) {
					t.Errorf("the vault still holds %q, want it gone: the vault's view of the path "+
						"wins, and absence is a view like any other (§4)", path)
				}
				if env.remoteHolds(path) {
					t.Errorf("the remote still holds %q, want the vault's view of it (§4)", path)
				}
			}
			for path, want := range row.copies {
				if got := env.vaultFile(path); got != want {
					t.Errorf("the conflict copy %q holds %q, want %q byte for byte (§4)", path, got, want)
				}
				if got := env.remoteFile(path); got != want {
					t.Errorf("the remote's copy of %q holds %q, want %q — copies are committed "+
						"inside the merge commit (§4)", path, got, want)
				}
			}
			if got, want := len(env.conflictCopies()), len(row.copies); got != want {
				t.Errorf("obsync left %d conflict copies in the vault (%v), want %d: %v", got,
					env.conflictCopies(), want, row.copies)
			}
		})
	}
}

// User story 14, and the one way this design could actually lose bytes: a copy
// that already exists is never overwritten, so a half-finished resolution
// survives the next conflict at the same path in the same minute.
//
// The human here is mid-resolution — they have started editing the copy obsync
// left them and have not deleted it yet — and a second conflict lands at the
// same path inside the same minute, so the name obsync would reach for is
// exactly the one holding their work. The counter is what makes that a second
// file rather than a lost one.
func TestAConflictCopyIsNeverOverwritten(t *testing.T) {
	t.Parallel()

	first := "Daily/2026-08-24" + conflictAt + ".md"
	env := newVault(t)
	env.writeNote(first, "half of my resolution\n")
	env.vaultAlreadyTracks("Daily/2026-08-24.md", first)
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	if got, want := env.vaultFile(first), "half of my resolution\n"; got != want {
		t.Errorf("the copy the human was resolving now holds %q, want %q: an existing copy is never "+
			"overwritten, because that is the one way this design could lose bytes (§4)", got, want)
	}
	second := "Daily/2026-08-24" + conflictAt + " 2.md"
	if got, want := env.vaultFile(second), "written on the laptop\n"; got != want {
		t.Errorf("the new conflict copy is at %q holding %q, want %q — a collision appends a "+
			"counter (§4)", second, got, want)
	}
	if got, want := env.remoteFile(first), "half of my resolution\n"; got != want {
		t.Errorf("the remote holds %q at the copy the human was resolving, want %q", got, want)
	}
}

// The tree check the copy's name is looked up in is not the same check as the
// one on disk, and this is the case that needs it: another device resolved a
// conflict and pushed the copy, so the name obsync is about to reach for exists
// in the merge it is building and does not exist in the vault at all.
//
// Writing over it would put obsync's bytes at a path whose bytes the remote
// already holds, in the very commit that is supposed to be keeping both sides.
func TestACopyArrivingFromTheRemoteInTheSameMergeIsNotWrittenOver(t *testing.T) {
	t.Parallel()

	arriving := "Daily/2026-08-24" + conflictAt + ".md"
	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.onTheLaptop(func(laptop string) {
		if err := os.WriteFile(filepath.Join(laptop, "Daily/2026-08-24.md"),
			[]byte("written on the laptop\n"), 0o644); err != nil {
			env.t.Fatalf("writing on the laptop: %v", err)
		}
		if err := os.WriteFile(filepath.Join(laptop, arriving),
			[]byte("the copy the laptop already resolved\n"), 0o644); err != nil {
			env.t.Fatalf("writing the laptop's own conflict copy: %v", err)
		}
	})
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	if got, want := env.vaultFile(arriving), "the copy the laptop already resolved\n"; got != want {
		t.Errorf("the copy that arrived from the remote holds %q, want %q: an existing copy is "+
			"never overwritten, and one arriving in the same merge exists in the tree before it "+
			"exists on disk (§4)", got, want)
	}
	second := "Daily/2026-08-24" + conflictAt + " 2.md"
	if got, want := env.vaultFile(second), "written on the laptop\n"; got != want {
		t.Errorf("obsync's own copy is at %q holding %q, want %q — the collision was answered by a "+
			"counter (§4)", second, got, want)
	}
}

// The table is closed, and a conflict with no row in it gets no improvised
// resolution: obsync applies nothing at all rather than guessing which side's
// bytes survive. A symlink where the other side has a file is one such shape,
// named in §4 along with submodules and mode-only conflicts.
//
// What tier that lands in is #31's — a network freeze, alongside the two merge
// ceilings — so what is asserted here is the part that is #30's and does not
// move: the vault is untouched and nothing invented reaches the remote.
func TestAConflictWithNoRowInTheTableIsNeverImprovised(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Notes/thing.md")
	env.remoteCommit("Notes/thing.md", "still a note on the laptop\n")
	env.deleteNote("Notes/thing.md")
	if err := os.Symlink("elsewhere.md", filepath.Join(env.vault, "Notes/thing.md")); err != nil {
		t.Fatalf("making the symlink in the vault: %v", err)
	}
	env.turn()
	env.awaitIdle()
	committed := env.vaultTip()

	env.advance(70 * time.Second)

	if got, err := os.Readlink(filepath.Join(env.vault, "Notes/thing.md")); err != nil || got != "elsewhere.md" {
		t.Errorf("the vault holds %q (%v) at the symlink, want it untouched: a conflict outside "+
			"§4's closed table is never resolved by guessing at bytes (§4)", got, err)
	}
	if copies := env.conflictCopies(); len(copies) != 0 {
		t.Errorf("obsync wrote %v for a conflict it has no rule for, want none (§4)", copies)
	}
	if got := env.vaultTip(); got != committed {
		t.Errorf("the vault's branch moved to %s, want it still at %s — nothing was applied (§4)",
			got, committed)
	}
}

// The name is the interface, so it is asserted as one: the folder, the marker,
// the minute, the extension, and none of the characters Obsidian forbids.
func TestAConflictCopyIsNamedForItsFolderItsMinuteAndItsExtension(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Attachments/diagram.png")
	env.remoteCommit("Attachments/diagram.png", "the laptop's picture\n")
	env.writeNote("Attachments/diagram.png", "the vault's picture\n")

	env.wake()

	got := env.theConflictCopy()
	if want := "Attachments/diagram" + conflictAt + ".png"; got != want {
		t.Errorf("obsync named the conflict copy %q, want %q: same folder, extension preserved, and "+
			"a UTC timestamp at minute precision (§4)", got, want)
	}
	for _, forbidden := range []string{"#", "^", "[", "]", "|", ":"} {
		if strings.Contains(strings.TrimPrefix(got, "Attachments/diagram"), forbidden) {
			t.Errorf("the conflict copy's name %q holds %q, which Obsidian forbids in a filename "+
				"or a filesystem may (§4)", got, forbidden)
		}
	}
}

// Clean line-level merges are kept: two devices appending to one daily note is
// the common case here, and whole-file granularity would spray copies for edits
// that never collided.
func TestACleanLineLevelMergeIsKeptRatherThanCopied(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "morning\n\n\n\n\n\n\n\n\n\nevening\n")
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "morning\nfrom the laptop\n\n\n\n\n\n\n\n\nevening\n")
	env.writeNote("Daily/2026-08-24.md", "morning\n\n\n\n\n\n\n\n\nfrom the vault\nevening\n")

	env.wake()

	got := env.vaultFile("Daily/2026-08-24.md")
	if !strings.Contains(got, "from the laptop") || !strings.Contains(got, "from the vault") {
		t.Errorf("the merged note holds %q, want both sides' lines: a clean line-level merge is "+
			"kept rather than turned into a copy (§4)", got)
	}
	if copies := env.conflictCopies(); len(copies) != 0 {
		t.Errorf("obsync wrote %v for a merge that had no conflict at all, want none — whole-file "+
			"granularity would spray copies for edits that never collided (§4)", copies)
	}
}

// The merge commit is a merge: two parents, git's own default message, and
// obsync's identity on it. Provenance lives in the identity, so there is no
// obsync sentence in the message to keep in step with git.
func TestTheMergeCommitCarriesBothParentsGitsOwnMessageAndObsyncsIdentity(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	if got, want := env.mergeParents(), 2; got != want {
		t.Errorf("the commit obsync pushed has %d parents, want %d: both sides survive because both "+
			"histories do (§4)", got, want)
	}
	if got, want := env.remoteMessage(), "Merge remote-tracking branch 'origin/main'"; got != want {
		t.Errorf("the merge commit's message is %q, want %q — merge commits keep git's default "+
			"message (§2)", got, want)
	}
	if got, want := env.remoteAuthor(), "obsync <obsync@obsync.invalid>"; got != want {
		t.Errorf("the merge commit's author is %q, want %q: the author identity stays obsync's, "+
			"which is where provenance lives (§2)", got, want)
	}
}

// One commit, one atomic state. The copy is inside the merge commit rather than
// in a follow-up, because a copy left untracked would leave the remote's bytes
// on one box only — and a second commit would be a state the vault was never
// in.
func TestTheConflictCopyIsInsideTheMergeCommitRatherThanAFollowUp(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.wake()

	copy := "Daily/2026-08-24" + conflictAt + ".md"
	if got, want := env.mergeParents(), 2; got != want {
		t.Fatalf("the remote's tip has %d parents, want %d — the merge itself should be the tip. "+
			"obsync said:\n%s", got, want, env.said())
	}
	if got, want := env.remoteFile(copy), "written on the laptop\n"; got != want {
		t.Errorf("the merge commit holds %q at the conflict copy, want %q: the tree is built by "+
			"hand anyway, so the copy is committed inside it (§4)", got, want)
	}
	if got := env.remoteSubjects()[0]; got != "Merge remote-tracking branch 'origin/main'" {
		t.Errorf("the newest commit on the remote is %q, want the merge itself — there is no "+
			"follow-up commit that writes the copy (§4)", got)
	}
}

// The apply is never forced. `reset --keep` would refuse a vault dirty at a
// path the merge touches, so obsync asks first and abandons the run instead —
// not an error, not a freeze, nothing above debug. The next run recomputes the
// merge against the new HEAD and applies it.
func TestAMergeTheVaultIsBeingWrittenUnderIsRecomputedOnTheNextRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.turn()
	env.awaitIdle()

	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Notes/elsewhere.md", "an unrelated note, so the vault is ahead too\n")
	// The third writer, landing on the path the merge is about between the
	// commit and the apply.
	env.duringSettle(func() {
		env.writeNote("Daily/2026-08-24.md", "and ignis is still writing this one\n")
	})
	env.advance(70 * time.Second)

	if copies := env.conflictCopies(); len(copies) != 0 {
		t.Errorf("obsync applied a merge over a path the vault was being written at and left %v, "+
			"want the run abandoned: the apply is never forced (§4)", copies)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync reported a refused apply above debug, want nothing — it is an aborted "+
			"run, and the abort tier reports nothing (§4, §7). It said:\n%s", said)
	}

	// The writer stops, and the next wake-up recomputes the merge against the
	// HEAD the first run left behind.
	env.duringSettle(nil)
	env.advance(70 * time.Second)

	if got, want := env.vaultFile("Daily/2026-08-24.md"), "and ignis is still writing this one\n"; got != want {
		t.Errorf("the vault holds %q, want %q: the run that was abandoned lost nothing, and the "+
			"next one merged what the vault actually held (§4)", got, want)
	}
	if got, want := env.vaultFile(env.theConflictCopy()), "written on the laptop\n"; got != want {
		t.Errorf("the conflict copy holds %q, want %q — the merge was recomputed and applied on the "+
			"following run (§4)", got, want)
	}
}

// §6's write side gates §4's apply, and it is all-or-nothing: one unsettled
// path among the ones the merge touches abandons the whole apply, because a
// partial apply leaves the vault holding a tree obsync never computed.
//
// The path being written here is one the merge would land on but which is
// otherwise not in conflict at all, so nothing but the guard stands in the way.
func TestAMergeIsNotAppliedOverAPathTheRemoteChangedAndTheVaultIsWriting(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md", "Notes/incoming.md")
	env.turn()
	env.awaitIdle()

	env.onTheLaptop(func(laptop string) {
		if err := os.WriteFile(filepath.Join(laptop, "Notes/incoming.md"),
			[]byte("changed on the laptop\n"), 0o644); err != nil {
			env.t.Fatalf("writing on the laptop: %v", err)
		}
	})
	env.writeNote("Daily/2026-08-24.md", "written in the vault, so both sides moved\n")
	env.duringSettle(func() {
		env.writeNote("Notes/incoming.md", "and something is writing this one\n")
	})
	env.advance(70 * time.Second)

	if got, want := env.vaultFileYet("Notes/incoming.md"), "and something is writing this one\n"; got != want {
		t.Errorf("the vault holds %q at the path something was writing, want %q: the write side is "+
			"all-or-nothing and the apply did not happen (§6)", got, want)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync reported an unsettled path on the write side above debug; it is an "+
			"aborted run (§6, §7). It said:\n%s", said)
	}
}

// Recovery is stateless: the filename glob is the state. The human edits the
// two notes together and deletes the copy, and the ordinary loop commits that
// like any other edit — no command to remember, and nothing left over for the
// next run to reconcile.
func TestResolvingAConflictIsEditingTheTwoNotesAndDeletingTheCopy(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.turn()
	env.awaitIdle()

	copy := "Daily/2026-08-24" + conflictAt + ".md"
	if !env.vaultHoldsYet(copy) {
		t.Fatalf("obsync wrote no conflict copy to resolve. It said:\n%s", env.saidSoFar())
	}

	env.writeNote("Daily/2026-08-24.md", "written in the vault\nwritten on the laptop\n")
	env.deleteNote(copy)
	env.advance(70 * time.Second)

	if got, want := env.remoteFile("Daily/2026-08-24.md"), "written in the vault\nwritten on the laptop\n"; got != want {
		t.Errorf("the remote holds %q, want %q: the resolution is an ordinary edit and the ordinary "+
			"loop commits it (§4)", got, want)
	}
	if env.remoteHolds(copy) {
		t.Error("the remote still holds the conflict copy after the human deleted it; deleting the " +
			"copy is the whole of the recovery, and there is no state left to clear (§4)")
	}
	if copies := env.conflictCopies(); len(copies) != 0 {
		t.Errorf("the vault still holds %v, want no conflict copies: the pattern is the state, so a "+
			"resolved conflict is one with no file matching it (§4)", copies)
	}
}

// A vault is a hostile place to build a filename in. A note title carries
// spaces, unicode and glob characters, and every one of them travels into the
// copy's own name, into a pathspec, and into a tree obsync builds by hand.
func TestAConflictedNoteWhoseNameHoldsASpaceAUnicodeQuoteAndAGlobIsCopiedToo(t *testing.T) {
	t.Parallel()

	note := "Notes/Zettel/a [draft] plan\nfür “quoted” note.md"
	env := newVault(t)
	env.vaultAlreadyTracks(note)
	env.remoteCommit(note, "written on the laptop\n")
	env.writeNote(note, "written in the vault\n")

	env.wake()

	if got, want := env.vaultFile(note), "written in the vault\n"; got != want {
		t.Errorf("the vault holds %q at a note whose name holds a glob character, want %q (§4)", got, want)
	}
	copy := "Notes/Zettel/a [draft] plan\nfür “quoted” note" + conflictAt + ".md"
	if got, want := env.vaultFile(copy), "written on the laptop\n"; got != want {
		t.Errorf("the conflict copy %q holds %q, want %q — `[draft]` is a name, not a character "+
			"class (§1)", copy, got, want)
	}
	if got, want := env.remoteFile(copy), "written on the laptop\n"; got != want {
		t.Errorf("the remote holds %q at that copy, want %q", got, want)
	}
}

// The vault's version does not change under the human's cursor, and the check
// is the file itself rather than its bytes: `reset --keep` updates only the
// paths that differ, so a note whose content the merge did not change is not
// rewritten at all.
func TestTheNoteTheHumanHasOpenIsNotRewrittenByTheMerge(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Daily/2026-08-24.md")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	before, err := os.Stat(filepath.Join(env.vault, "Daily/2026-08-24.md"))
	if err != nil {
		t.Fatalf("reading the note the human has open: %v", err)
	}

	env.wake()

	after, err := os.Stat(filepath.Join(env.vault, "Daily/2026-08-24.md"))
	if err != nil {
		t.Fatalf("reading the note the human has open: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the note the human has open was rewritten by the merge (mtime %s became %s), want "+
			"it left exactly where it is: the vault's version stays at the path, so the file does "+
			"not change under their cursor (§4, story 11)", before.ModTime(), after.ModTime())
	}
}

// everyNote is every file in the vault outside .git, by path, which is how a
// test asserts about the vault as a whole rather than about a path it guessed.
func (e *vaultEnv) everyNote() map[string]string {
	e.t.Helper()
	e.stop()

	notes := map[string]string{}
	err := filepath.WalkDir(e.vault, func(entry string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(e.vault, entry)
		if err != nil {
			return err
		}
		if relative == ".git" {
			return fs.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		content, err := os.ReadFile(entry)
		if err != nil {
			return err
		}
		notes[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		e.t.Fatalf("reading the vault: %v", err)
	}
	return notes
}
