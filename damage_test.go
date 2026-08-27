package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A damaged local repository (§7, #34). Seam 1 throughout: a real vault, a real
// bare remote over file://, real git, and every assertion about what obsync did
// or did not do to the two repositories.
//
// The damage is real rather than simulated. A test rots the bytes of a loose
// object on disk, or truncates the index, and what git then does about it is
// git's own business — which is the whole point, because the exit codes damage
// produces were measured while this design was made and a faked git would be
// testing obsync's beliefs about them.
//
// Where a test needs a local git to fail for a reason that is *not* damage, it
// installs a hook in the vault's own repo. That is the human's file in the
// human's repository, the same precedent settle_test.go and verify_test.go set,
// and it is what makes "whatever the stated reason" assertable: the streak is
// about persistence, not about what went wrong.

// The two lines the index rebuild and a low disk are said in. Neither is part
// of the declared surface — the wording of obsync's log is not a promise
// (docs/interface.md) — but that obsync *says* both is: §9 puts the cost of the
// rebuild and free space in what a human is told, and a cost obsync does not
// mention is one it hid.
const (
	indexRebuilt  = "obsync has discarded the vault's .git/index"
	freeSpaceSaid = "the filesystem holding the vault's repository has "
)

// The damage obsync repairs, and the only one: the index is derived state — a
// cache of HEAD, holding no history — so at five failed runs obsync discards it
// and builds it again from HEAD, unconditionally. The run after that finds a
// repository that works, and the vault goes on syncing with nothing frozen and
// nobody called (§7).
func TestATruncatedIndexIsRebuiltAtFiveRunsAndTheVaultGoesOnSyncing(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "written just before the index went wrong\n")
	env.theVaultsIndexIsTruncated()

	// Four runs is not yet evidence of anything: the threshold is five,
	// because that is where this design stops believing in bad luck.
	for range 4 {
		env.advance(70 * time.Second)
	}
	if said := env.saidSoFar(); strings.Contains(said, indexRebuilt) {
		t.Errorf("obsync rebuilt the index after four failed runs, want it to wait for five: the "+
			"threshold is the only thing permitted to call a failure permanent (§7). It said:\n%s", said)
	}
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote holds the note while every local half was failing, want nothing: a run " +
			"whose local half failed stops rather than pushing on with the network one (§7)")
	}

	// The fifth. The rebuild happens before the freeze rather than instead of
	// it, and the run it belongs to is still a failed run.
	env.advance(70 * time.Second)

	if said := env.saidSoFar(); !strings.Contains(said, indexRebuilt) {
		t.Errorf("obsync said %q after five failed runs, want it to have discarded and rebuilt the "+
			"vault's index: the index is derived state and this is the one repair obsync makes "+
			"(§7)", said)
	}

	// And the one more run §7 asks for, which is the one that finds a
	// repository that works again.
	env.advance(70 * time.Second)

	if got, held := env.remoteContentYet("Daily/2026-08-24.md"); !held || got != "written just before the index went wrong\n" {
		t.Errorf("the remote holds %q once the index was rebuilt, want the note obsync had been "+
			"unable to commit: obsync repairs derived state so that the work can go on (§7)", got)
	}
	if said := env.said(); strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync froze over an index it had just repaired, want no freeze at all: the "+
			"streak is reset by any run whose local half completes (§7). It said:\n%s", said)
	}
}

// The half of the rebuild that is not the discard, and the reason it is two
// commands rather than one: a rebuilt index is a cache of HEAD, and an absent
// one is an *empty* index, which git reports as every tracked path deleted.
//
// A run against a live vault always has a path the settle guard is holding out
// of this commit (§6). Against an index that is a cache of HEAD it commits
// nothing at all; against an empty one it would commit the deletion of the note
// somebody is typing — the settle guard manufacturing the exact harm it exists
// to prevent, and obsync publishing it.
//
// Both ways obsync can reach that state are here, because they are different
// code: the rebuild at five leaves an index behind when it can read HEAD, and
// leaves nothing at all when the object HEAD names is itself the damage, so a
// repository that works again is the second one's first chance to go wrong.
func TestARepositoryThatWorksAgainDoesNotPublishTheDeletionOfAPathLeftOut(t *testing.T) {
	t.Parallel()

	for _, damage := range []struct {
		name    string
		inflict func(*vaultEnv)
		repair  func(*vaultEnv)
	}{
		{
			name:    "a truncated index, which the rebuild puts back from HEAD",
			inflict: (*vaultEnv).theVaultsIndexIsTruncated,
		},
		{
			name:    "a rotted object, which leaves the rebuild nothing to read",
			inflict: (*vaultEnv).theDiskRotsTheObjectGitNeedsMost,
			repair:  (*vaultEnv).theRottedObjectIsRestored,
		},
	} {
		t.Run(damage.name, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			env.writeNote("Daily/2026-08-24.md", "the version the remote holds\n")
			env.turn()
			env.awaitIdle()
			if !env.remoteHoldsYet("Daily/2026-08-24.md") {
				t.Fatal("the remote does not hold the note this test is about, so it is measuring nothing")
			}

			// Somebody starts typing into that note and does not stop, so
			// every run from here finds it moving across the settle interval
			// and leaves it out of the commit.
			env.writeNote("Daily/2026-08-24.md", "a version being typed")
			typed := 0
			env.duringSettle(func() {
				typed++
				env.writeNote("Daily/2026-08-24.md", strings.Repeat("still typing ", typed))
			})
			damage.inflict(env)

			// Five failed runs, the rebuild, and the one more run §7 asks for.
			for range 6 {
				env.advance(70 * time.Second)
			}
			if damage.repair != nil {
				// The damage the rebuild could not repair: a human recovers
				// the object, the probe releases the freeze on the next tick,
				// and the tick after that is the run that works again.
				damage.repair(env)
				env.advance(70 * time.Second)
				env.advance(70 * time.Second)
			}

			if !env.remoteHoldsYet("Daily/2026-08-24.md") {
				t.Error("the remote lost the note the settle guard was holding out of the commit, " +
					"want it untouched: an index obsync rebuilt is a cache of HEAD, and a run " +
					"against an empty one would publish the deletion of every path it did not " +
					"stage (§6, §7)")
			}
			if said := env.said(); strings.Contains(said, "Delete") {
				t.Errorf("obsync composed a deletion after the repository worked again, want none "+
					"at all: nothing was deleted from the vault. It said:\n%s", said)
			}
		})
	}
}

// The damage obsync does not repair, and the shape of what it does instead:
// five failed runs, one index rebuild, one more run, and then a full freeze
// that says what git said rather than what obsync guessed.
func TestADamagedRepositoryFreezesOnceRebuildingTheIndexHasNotHelped(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	soundAt := env.vaultTip()

	env.theDiskRotsTheObjectGitNeedsMost()
	env.writeNote("Daily/2026-08-24.md", "written after the disk went wrong\n")

	// Five failed runs, then the run after the rebuild. Nothing is frozen
	// until that last one has failed too.
	for range 5 {
		env.advance(70 * time.Second)
	}
	if said := env.saidSoFar(); strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync froze before it had tried the run that follows the index rebuild, want it "+
			"to attempt one more first (§7). It said:\n%s", said)
	}

	env.advance(70 * time.Second)

	said := env.saidSoFar()
	if !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q after six failed runs, want the one ERROR a full freeze enters "+
			"with: a local failure streak reaching five is §7's own full-freeze row", said)
	}
	// What the freeze carries, and every part of it is evidence rather than a
	// diagnosis: the count of runs, the argv a human can run themselves, git's
	// own first line of stderr, and what that prose looks like (§9).
	for _, wanted := range []string{
		"6 sync runs in a row",
		"status --porcelain=v2",
		"inflate",
		"this looks like a corrupt object",
	} {
		if !strings.Contains(said, wanted) {
			t.Errorf("the damage freeze does not say %q, want the streak count, the failing argv, "+
				"git's own words and what they look like (§9). obsync said:\n%s", wanted, said)
		}
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned over a damaged repository, want it parked alive: " +
			"obsync never exits on a sync failure, because a crash-looping container buries the " +
			"one message that matters (§7)")
	}
	if got := env.vaultTip(); got != soundAt {
		t.Errorf("the vault's branch moved to %q while its repository was damaged, want it left at "+
			"%q: obsync never repairs a repository beyond the index, and never re-clones (§7)", got, soundAt)
	}
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote gained a note written while the repository was damaged, want nothing " +
			"published by an obsync that cannot read its own vault (§7)")
	}
}

// The one freeze that self-clears by *retrying the work* rather than by
// re-checking a gate, and the honest exception §7 names rather than papers over.
// While it stands obsync runs exactly one read-only probe a tick and does
// nothing else at all; the probe succeeding is the whole of the way out.
func TestAFrozenObsyncProbesOnceATickAndTheProbeReleasesTheFreeze(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.theDiskRotsTheObjectGitNeedsMost()
	for range 6 {
		env.advance(70 * time.Second)
	}
	frozenAt := env.vaultTip()
	env.writeNote("Daily/2026-08-24.md", "written while obsync was frozen\n")

	// Three more ticks under the freeze. A probe writes nothing — `git status`
	// under GIT_OPTIONAL_LOCKS=0 does not even write the refreshed index back
	// (§1) — so the repository is untouched, and the freeze is said once
	// rather than once a tick (§9).
	for range 3 {
		env.advance(70 * time.Second)
	}

	said := env.saidSoFar()
	if got := strings.Count(said, frozenAndTouchingNothing); got != 1 {
		t.Errorf("obsync entered the damage freeze %d times over four ticks, want once: state "+
			"entry is said exactly once (§9). It said:\n%s", got, said)
	}
	if got := env.vaultTip(); got != frozenAt {
		t.Errorf("the vault's branch moved to %q while obsync was frozen, want it left at %q: a "+
			"full freeze stops everything, and the probe is read-only (§7)", got, frozenAt)
	}
	// The rebuild at five left no index, because the object it had to read to
	// write one back is the damage. That obsync has not put one back over three
	// ticks is the sharpest thing there is to say about "one read-only probe
	// and nothing else": putting one back is the first write a run makes.
	if env.vaultHasAnIndex() {
		t.Error("the vault has an index again while obsync was frozen, want obsync to have done " +
			"nothing but its one read-only probe each tick: a full freeze stops everything (§7)")
	}

	// The human recovers the object. Nothing tells obsync so, and nothing has
	// to: the probe finds the repository readable and the freeze goes.
	env.theRottedObjectIsRestored()
	env.advance(70 * time.Second)

	if said := env.saidSoFar(); !strings.Contains(said, freezeCleared) {
		t.Errorf("obsync said %q once the repository was sound again, want one line saying the "+
			"freeze cleared: the probe succeeding releases it, with no restart (§7, §9)", said)
	}

	env.advance(70 * time.Second)

	if got, held := env.remoteContentYet("Daily/2026-08-24.md"); !held || got != "written while obsync was frozen\n" {
		t.Errorf("the remote holds %q after the freeze cleared, want the note obsync had deferred: "+
			"a freeze that clears leaves obsync syncing again (§7)", got)
	}
	if !env.vaultHasAnIndex() {
		t.Error("the vault still has no index after obsync went back to work, want one built from " +
			"HEAD: git reads a missing index as an empty one, and a run against that would " +
			"commit the deletion of every path obsync did not stage (§6, §7)")
	}
}

// The probe is the only thing a frozen run does, and this is where that is
// visible: a freeze whose cause `git status` cannot see clears on the very next
// tick, because obsync's evidence was five runs of the work failing and the
// probe says the work is possible again.
//
// That is the freeze being keyed on evidence rather than latched. obsync goes
// back to work as soon as the repository reads, and if the work still fails it
// counts to five and concludes again — which is what the mutant that lets a
// frozen run carry on into its local half cannot do, because the commit it
// would attempt is the thing that keeps failing.
func TestTheProbeIsAllAFrozenRunDoesAndItReleasesTheFreezeOnItsOwn(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// A hook is the local half failing at something `git status` cannot see:
	// the repository is perfectly readable and obsync's commit is refused.
	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "a note the hook would not let obsync commit\n")
	for range 6 {
		env.advance(70 * time.Second)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Fatalf("obsync did not freeze after six failed runs, so this test never reached the "+
			"freeze it is about. It said:\n%s", said)
	}

	stagedAt := env.vaultIndexWrittenAt()
	env.advance(70 * time.Second)

	if said := env.saidSoFar(); !strings.Contains(said, freezeCleared) {
		t.Errorf("obsync said %q on the first tick under the freeze, want the probe to have found "+
			"the repository readable and released it: this freeze self-clears by retrying the "+
			"work rather than by re-checking a gate (§7)", said)
	}
	if got := env.vaultIndexWrittenAt(); !got.Equal(stagedAt) {
		t.Errorf("the vault's index was written at %s under the freeze, want it untouched since "+
			"%s: a frozen run is one read-only `git status` and never the staging the local half "+
			"would have done (§7)", got, stagedAt)
	}
}

// Runs, not commands, and consecutive ones: one run failing three commands over
// the same damaged object is one piece of evidence, and any run whose local half
// completes puts the count back to nothing (§7).
func TestALocalFailureStreakIsResetByARunWhoseLocalHalfCompletes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// A hook is a local git failing for a reason that is not damage at all,
	// which is what "whatever the stated reason" has to hold against.
	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "a note the hook would not let obsync commit\n")
	for range 4 {
		env.advance(70 * time.Second)
	}

	env.removeVaultHook("pre-commit")
	env.advance(70 * time.Second)

	if !env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Fatal("the remote does not hold the note once the hook was gone, so this test never " +
			"reached the run whose local half completes")
	}

	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-25.md", "a second note the hook would not let obsync commit\n")
	for range 4 {
		env.advance(70 * time.Second)
	}

	said := env.said()
	if strings.Contains(said, indexRebuilt) {
		t.Errorf("obsync rebuilt the index after eight failed runs that were never five in a row, "+
			"want the streak reset by the run whose local half completed (§7). It said:\n%s", said)
	}
	if strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync froze after eight failed runs that were never five in a row, want no "+
			"freeze: the streak counts consecutive runs (§7). It said:\n%s", said)
	}
}

// git's own words may *name* a failure; only persistence may *escalate* one.
// The two halves of the same vault, failing at the same command for the same
// number of runs, differing only in what git printed — and what differs in
// obsync is the sentence a human reads and nothing else (§7).
func TestGitsWordsNameAFailureAndNeverChangeWhatObsyncDoes(t *testing.T) {
	t.Parallel()

	// The prose git writes for a corrupt loose object, measured at both matrix
	// points, against prose no git has ever written. Neither is damage: both
	// are a hook refusing obsync's commit, so the only difference between the
	// two vaults is the words.
	for _, said := range []struct {
		name, stderr, looksLike string
	}{
		{
			name:      "words git uses for a corrupt object",
			stderr:    "error: inflate: data stream error (incorrect header check)",
			looksLike: "this looks like a corrupt object",
		},
		{
			name:   "words no git has written",
			stderr: "error: the mnemonic circuits are inverted",
		},
	} {
		t.Run(said.name, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			env.turn()
			env.awaitIdle()

			env.installVaultHook("pre-commit", "#!/bin/sh\necho '"+said.stderr+"' >&2\nexit 1\n")
			env.writeNote("Daily/2026-08-24.md", "a note obsync could not commit\n")
			for range 6 {
				env.advance(70 * time.Second)
			}

			log := env.said()
			if !strings.Contains(log, frozenAndTouchingNothing) {
				t.Errorf("obsync said %q after six failed runs, want the full freeze the streak "+
					"reaches whatever the stated reason: time is the classifier, never prose "+
					"(§7)", log)
			}
			if !strings.Contains(log, indexRebuilt) {
				t.Errorf("obsync said %q, want the index rebuilt at five runs whatever git said: "+
					"the rebuild is unconditional, and letting a stderr match choose an action "+
					"is the line this design does not cross (§7)", log)
			}
			if !strings.Contains(log, said.stderr) {
				t.Errorf("obsync said %q, want git's own first line of stderr relayed verbatim "+
					"(§9)", log)
			}
			switch {
			case said.looksLike != "" && !strings.Contains(log, said.looksLike):
				t.Errorf("obsync said %q, want it to name what git's words look like: the prose "+
					"makes the message say %q instead of just what failed (§7)", log, said.looksLike)
			case said.looksLike == "" && strings.Contains(log, "this looks like"):
				t.Errorf("obsync said %q of prose no git has written, want it to say nothing "+
					"extra: an unrecognised failure gets no guess (§7)", log)
			}
		})
	}
}

// statfs labels, never gates. There is no free-space gate anywhere in obsync
// and no threshold to configure: obsync reads free space only once a local
// command has already failed, and adds a sentence to what it was going to say
// anyway (§7).
//
// The threshold is the size ceiling — the largest single file obsync would ever
// hand git — so the config surface is what puts this vault under it. That is
// also the only honest way to reach a nearly-full disk at seam 1: a test cannot
// fill the filesystem it is running on.
func TestFreeSpaceIsSaidWhenALocalCommandFailsAndNeverGatesAnything(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1000GB")
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "a note written on a disk obsync reads as nearly full\n")
	env.advance(70 * time.Second)

	if !env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote does not hold a note written under a size ceiling larger than the " +
			"disk, want it synced: free space labels a failure and never gates one, and there " +
			"is no free-space gate (§7)")
	}
	if said := env.saidSoFar(); strings.Contains(said, freeSpaceSaid) {
		t.Errorf("obsync said %q on a run that worked, want free space mentioned only where a "+
			"local command has failed: it is a label on a failure, not a report (§7)", said)
	}

	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-25.md", "a note the hook would not let obsync commit\n")
	env.advance(70 * time.Second)

	if said := env.said(); !strings.Contains(said, freeSpaceSaid) {
		t.Errorf("obsync said %q when a local command failed with almost no room left, want it to "+
			"say how much: that is the difference between a five-minute fix and a bug report "+
			"(§7)", said)
	}
}

// The other side of the same rule, and the one that matters for noise: a
// failure on a disk with room on it says nothing about the disk.
func TestFreeSpaceIsNotMentionedWhenThereIsRoomOnTheDisk(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "a note the hook would not let obsync commit\n")
	env.advance(70 * time.Second)

	if said := env.said(); strings.Contains(said, freeSpaceSaid) {
		t.Errorf("obsync said %q about a local failure on a disk with room on it, want nothing "+
			"about free space: a label that fires when it is not true is one nobody reads "+
			"(§7). ", said)
	}
}

// The cost of the index rebuild, said out loud rather than hidden: a human's
// staged-but-uncommitted work is dropped. It is near-invisible because their
// files are untouched and the next run commits them anyway — which is what this
// asserts, both halves of it (§7).
func TestTheIndexRebuildDropsStagedWorkAndSaysSo(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// A human's own half-finished work, staged in their vault and not
	// committed. obsync's commit is what the hook stops, so nothing here
	// reaches HEAD until the hook goes.
	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")
	env.writeNote("Notes/half-done.md", "staged by a human and not committed\n")
	env.mustGit(env.vault, "add", "--", ":(literal)Notes/half-done.md")
	if !env.vaultStages("Notes/half-done.md") {
		t.Fatal("this test did not manage to stage anything, so it is measuring nothing")
	}

	for range 5 {
		env.advance(70 * time.Second)
	}

	if env.vaultStages("Notes/half-done.md") {
		t.Error("the human's staged work survived the index rebuild, want it dropped: the index " +
			"is a cache of HEAD and obsync rebuilds it from HEAD (§7)")
	}
	if got := env.vaultFileYet("Notes/half-done.md"); got != "staged by a human and not committed\n" {
		t.Errorf("the vault holds %q after the index rebuild, want the human's bytes untouched: "+
			"obsync may discard derived state and never history, and their file is not derived "+
			"state (§7)", got)
	}
	if said := env.saidSoFar(); !strings.Contains(said, "no longer staged") {
		t.Errorf("obsync said %q when it rebuilt the index, want it to say what that cost: a cost "+
			"obsync does not mention is one it hid (§7)", said)
	}

	// And the half that makes the cost near-invisible rather than merely
	// acceptable: the next run commits those bytes like any other change.
	env.removeVaultHook("pre-commit")
	env.advance(70 * time.Second)

	if got, held := env.remoteContentYet("Notes/half-done.md"); !held || got != "staged by a human and not committed\n" {
		t.Errorf("the remote holds %q, want the work the rebuild unstaged: their files were "+
			"untouched and the next run commits them anyway (§7)", got)
	}
}

// obsync ships no `recover` subcommand, and there is no automated repair of a
// damaged repository behind any name. Re-cloning `.git` discards exactly the
// commits obsync exists to have made, and obsync cannot tell whether a damaged
// object is one the remote already holds or one only this disk ever had — so
// the recipe is a human's, and there is nothing to type at obsync (§7).
func TestObsyncShipsNoRecoverSubcommand(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"recover", "repair", "fsck"} {
		_, stderr, code := runObsync(t, nil, name)
		if code == 0 {
			t.Errorf("obsync %s exited 0, want it refused: obsync never self-repairs a damaged "+
				"repo and ships no subcommand that does (§7). It said: %s", name, stderr)
		}
	}
}

// vaultIndexWrittenAt is when the vault's index was last written. It is how "a
// frozen run writes nothing" is asserted about the one file a run that got past
// the probe would have written: `git add` rewrites the index, and `git status`
// under GIT_OPTIONAL_LOCKS=0 does not (§1).
func (e *vaultEnv) vaultIndexWrittenAt() time.Time {
	e.t.Helper()

	info, err := os.Stat(filepath.Join(e.vault, ".git", "index"))
	if err != nil {
		e.t.Fatalf("reading when the vault's index was last written: %v", err)
	}
	return info.ModTime()
}

// vaultHasAnIndex reports whether the vault's repository has an index at all.
//
// It is real state rather than an implementation detail: a rebuild leaves
// nothing there when the object HEAD names is itself the damage, git reads a
// missing index as an empty one, and putting one back is the first write obsync
// makes when it can work in the repository again.
func (e *vaultEnv) vaultHasAnIndex() bool {
	e.t.Helper()

	_, err := os.Stat(filepath.Join(e.vault, ".git", "index"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.t.Fatalf("looking for the vault's index: %v", err)
	}
	return err == nil
}
