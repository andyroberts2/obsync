package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
)

// The safety interlocks (§7, #32): nine gates, three tiers, and an obsync that
// parks alive rather than exiting. Every test here is seam 1 — a real vault, a
// real bare remote over file://, real git — and every assertion is about what
// obsync did or did not do to the two repositories.

// The two lines a freeze is entered and left by. State entry and state exit each
// log exactly once (§9), and they are what tells a full freeze apart from a run
// that failed: a failed run applies nothing either, and what an operator can see
// is what they were told to look at and whether they were told it cleared.
const (
	frozenAndTouchingNothing = "level=ERROR msg=\"obsync is frozen and is touching nothing until this is repaired\""
	freezeCleared            = "level=INFO msg=\"the freeze cleared"
)

// The vault sentinel is the interlock that matters most: the mount drops, `git
// status` reports every tracked file deleted, and a fail-open local half would
// faithfully commit the deletion of the entire vault (§7). The gate is
// `.obsidian/` — its absence means the vault is not there.
func TestTheVaultSentinelMissingIsAFullFreezeAndNoDeletionIsCommitted(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "a note the vault had\n")
	env.turn()
	env.awaitIdle()
	before := env.commitsSoFar(env.vault)

	// The mount drops: the vault directory is still there and everything in it
	// is gone, which is exactly what git reports as every tracked file deleted.
	env.theVaultGoesEmpty()
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits after the mount dropped, want %s: a full freeze stops "+
			"obsync touching the repo at all, and a mass-deletion commit pollutes history whether "+
			"or not it is ever pushed (§7)", got, before)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when the vault sentinel went missing, want it parked " +
			"alive and re-checking (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q with the vault sentinel missing, want the one ERROR a full freeze "+
			"enters with (§9)", said)
	}

	// The mount comes back, and with it the vault. Every freeze self-clears
	// when its cause is repaired, without a restart.
	env.theVaultComesBack()
	env.writeNote("Daily/2026-08-25.md", "written once the vault was back\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-25.md"); got != "written once the vault was back\n" {
		t.Errorf("the remote holds %q once the vault was back, want the note obsync deferred: "+
			"every freeze clears when its cause is repaired, with no restart (§7)", got)
	}
	if !strings.Contains(env.said(), freezeCleared) {
		t.Errorf("obsync said %q, want one line saying the freeze cleared (§9)", env.said())
	}
}

// The other half of the same rule, and the reason it needs no threshold to
// defend: any amount of note deletion with `.obsidian/` intact is a human
// editing their vault, which obsync syncs without comment (§7).
func TestEveryNoteDeletedWithTheVaultSentinelIntactSyncsWithoutComment(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	for _, path := range []string{"Daily/one.md", "Daily/two.md", "Daily/three.md"} {
		env.writeNote(path, "a note\n")
	}
	env.turn()
	env.awaitIdle()

	for _, path := range []string{"Daily/one.md", "Daily/two.md", "Daily/three.md"} {
		env.deleteNote(path)
	}
	env.advance(70 * time.Second)

	for _, path := range []string{"Daily/one.md", "Daily/two.md", "Daily/three.md"} {
		if env.remoteHoldsYet(path) {
			t.Errorf("the remote still holds %q, want the human's deletion synced: any amount of "+
				"note deletion with .obsidian/ intact is a human editing their vault (§7)", path)
		}
	}
	if said := env.said(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q deleting every note in a vault that is still there, want it "+
			"synced without comment (§7)", said)
	}
}

// Gate 2 on every run after the first. obsync never re-clones and never repairs
// a repository by replacing it, so a `.git` that has gone is a freeze — which
// is what §7's tier table names in its own right.
func TestTheRepositoryDisappearingIsAFullFreezeAndObsyncNeverReClones(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.theRepositoryGoes()
	env.writeNote("Daily/2026-08-24.md", "written while there was no repository\n")
	env.advance(70 * time.Second)

	if env.vaultHoldsYet(".git") {
		t.Error("obsync put a .git back in the vault, want nothing: obsync never re-clones and " +
			"never repairs a repository by replacing it, because a re-clone discards exactly the " +
			"commits obsync exists to have made (§7)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when the repository went, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q when the repository went, want the one ERROR a full freeze enters "+
			"with (§9): a failed run applies nothing either, and the difference an operator can "+
			"see is what they are told to look at", said)
	}

	env.theRepositoryComesBack()
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while there was no repository\n" {
		t.Errorf("the remote holds %q once the repository was back, want the note obsync deferred "+
			"(§7)", got)
	}
}

// Gate 3. A detached HEAD has no branch to commit to, and obsync never checks
// one out — so the remedy is the human's own checkout and the gate is what
// makes their doing it enough.
func TestADetachedHeadIsAFullFreezeUntilABranchIsCheckedBackOut(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	before := env.commitsSoFar(env.vault)

	env.mustGit(env.vault, "checkout", "--quiet", "--detach", "HEAD")
	env.writeNote("Daily/2026-08-24.md", "written while HEAD was detached\n")
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits with HEAD detached, want %s: a full freeze stops "+
			"obsync touching the repo at all (§7)", got, before)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned on a detached HEAD, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q on a detached HEAD, want the one ERROR a full freeze enters with "+
			"(§9)", said)
	}

	env.mustGit(env.vault, "checkout", "--quiet", "main")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while HEAD was detached\n" {
		t.Errorf("the remote holds %q once a branch was checked back out, want the note obsync "+
			"deferred: every freeze clears when its cause is repaired (§7)", got)
	}
	if !strings.Contains(env.said(), freezeCleared) {
		t.Errorf("obsync said %q, want one line saying the freeze cleared (§9)", env.said())
	}
}

// Gate 4. A human's half-finished merge is theirs to finish, and committing
// into one would finish it for them with obsync's own idea of the answer.
func TestAnInterruptedMergeIsAFullFreezeUntilTheHumanFinishesIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.theHumanLeavesAMergeHalfFinished()
	before := env.commitsSoFar(env.vault)
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits mid-merge, want %s: obsync committing here would "+
			"finish a human's merge for them with obsync's own idea of the answer (§7)", got, before)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned on an interrupted merge, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q mid-merge, want the one ERROR a full freeze enters with (§9)", said)
	}

	env.mustGit(env.vault, "merge", "--abort")
	env.writeNote("Daily/2026-08-24.md", "written once the merge was settled\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written once the merge was settled\n" {
		t.Errorf("the remote holds %q once the human settled their merge, want the note obsync "+
			"deferred (§7)", got)
	}
	if !strings.Contains(env.said(), freezeCleared) {
		t.Errorf("obsync said %q, want one line saying the freeze cleared (§9)", env.said())
	}
}

// Gate 6. An unborn tracked branch is what a clone killed halfway leaves
// behind, and obsync cannot tell that from a human having broken HEAD in a repo
// that holds history — so the safe reading of the pair is the one that touches
// nothing.
func TestATrackedBranchThatNamesNoCommitIsAFullFreeze(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	tip := env.vaultTipYet()

	env.mustGit(env.vault, "update-ref", "-d", "refs/heads/main")
	env.writeNote("Daily/2026-08-24.md", "written while the branch named nothing\n")
	env.advance(70 * time.Second)

	if env.vaultHoldsBranchYet("main") {
		t.Error("obsync put refs/heads/main back, want nothing: obsync refuses a repo whose " +
			"tracked branch names no commit rather than repairing it (§7)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned on an unborn tracked branch, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q on an unborn tracked branch, want the one ERROR a full freeze "+
			"enters with (§9)", said)
	}

	env.mustGit(env.vault, "update-ref", "refs/heads/main", tip)
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while the branch named nothing\n" {
		t.Errorf("the remote holds %q once the branch named a commit again, want the note obsync "+
			"deferred (§7)", got)
	}
}

// Gate 9, and the reason §7's "every freeze self-clears" needs no footnote:
// keying the write-verify refusal on a *ref* rather than on process lifetime
// means a restart cannot clear it, and a human clears it deliberately.
func TestTheFailedApplyAnchorIsAFullFreezeARestartCannotClear(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	before := env.commitsSoFar(env.vault)

	env.mustGit(env.vault, "update-ref", "refs/obsync/failed-apply", env.vaultTipYet())
	env.writeNote("Daily/2026-08-24.md", "written while the anchor was there\n")
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits with the failed-apply anchor in place, want %s (§7)",
			got, before)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q with the failed-apply anchor in place, want the one ERROR a full "+
			"freeze enters with (§9)", said)
	}

	// A restart is the operator's reflex and it is the thing this gate is
	// keyed on a ref to survive.
	env.restart()
	env.turn()
	env.awaitIdle()

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits after a restart, want %s: a freeze a restart clears "+
			"is a freeze that restarting destroys the diagnosis of (§7, §9)", got, before)
	}

	env.mustGit(env.vault, "update-ref", "-d", "refs/obsync/failed-apply")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while the anchor was there\n" {
		t.Errorf("the remote holds %q once the human deleted the anchor, want the note obsync "+
			"deferred (§7)", got)
	}
}

// Gate 5. obsync silently adopting a new remote is the "confidently writes to
// the wrong place" failure, except worse, because the push *succeeds* and an
// entire vault lands somewhere nobody chose (§8).
func TestAnOriginPointingSomewhereElseIsAFullFreezeAndObsyncNeverRePointsIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	elsewhere := env.aSecondBareRemote()
	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, "file://"+elsewhere)
	env.writeNote("Daily/2026-08-24.md", "written while origin pointed elsewhere\n")
	env.advance(70 * time.Second)

	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("obsync pushed to a remote it was not pointed at, want nothing leaving (§8)")
	}
	if out, code := env.git(elsewhere, "rev-parse", "--verify", "--quiet", "refs/heads/main"); code == 0 {
		t.Errorf("the remote nobody pointed obsync at holds %q, want nothing at all: an entire "+
			"vault landing somewhere nobody chose is what gate 5 exists to prevent (§8)", out)
	}
	if got := strings.TrimSpace(env.mustGit(env.vault, "config", "--get", "remote."+config.RemoteName+".url")); got != "file://"+elsewhere {
		t.Errorf("the vault's origin is %q, want the URL the human set: obsync never runs "+
			"`git remote set-url` (§8)", got)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q with origin pointing elsewhere, want the one ERROR a full freeze "+
			"enters with (§9)", said)
	}

	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, env.repoURL)
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while origin pointed elsewhere\n" {
		t.Errorf("the remote holds %q once origin was put back, want the note obsync deferred (§7)", got)
	}
}

// The other side of gate 5, and the reason the comparison is a normalised pair
// rather than a string: https and ssh against the same host and path are the
// same repo, and freezing because an operator swapped a PAT for a deploy key
// protects nothing (§8).
func TestAnOriginSwappedFromHttpsToSshIsTheSameRepoAndNotAFreeze(t *testing.T) {
	t.Parallel()

	// A host that refuses instantly, so that what this test is about — the
	// comparison — is not waiting on a network. The remote being unreachable
	// is an aborted run, which reports nothing and leaves the local half
	// committing (§7).
	env := newVaultReachedBy(t, func(e *vaultEnv) (string, []string) {
		return "https://127.0.0.1:1/owner/vault.git",
			[]string{"OBSYNC_TOKEN_FILE=" + writeCredential(t, "ghp_the_operators_token\n")}
	})
	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, "ssh://git@127.0.0.1:1/owner/vault.git")
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "written against a deploy key\n")
	env.advance(70 * time.Second)

	if got := env.vaultFileYet("Daily/2026-08-24.md"); got != "written against a deploy key\n" {
		t.Fatalf("the vault holds %q, want the note the test wrote", got)
	}
	if got := env.commitsSoFar(env.vault); got != "2" {
		t.Errorf("the vault holds %s commits, want 2: scheme, credentials and a .git suffix are "+
			"not part of where bytes go, so swapping https for ssh against the same host and path "+
			"is not a freeze (§8)", got)
	}
	if said := env.saidSoFar(); strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q about an https-for-ssh swap, want no freeze at all (§8)", said)
	}
}

// credentialPathMismatch is the WARN an origin that authenticates differently
// from OBSYNC_REPO says, and the substring a test may hold on to.
const credentialPathMismatch = "the vault's origin authenticates differently from the remote obsync was given"

// The other side of *that*: not a freeze, and not silence either.
//
// The scheme is not where bytes go, so it is right that gate 5 ignores it — and
// it is the whole of *how obsync authenticates*, which is not a detail. An ssh
// origin under an https configuration reads key material obsync knows nothing
// about, while the token obsync was given, and required at startup (§8), is
// never read by anything. Both facts are true at once, and the deployment
// either works or fails silently on the abort tier depending on something
// obsync never looked at.
//
// So it is said once, at bootstrap, and nothing is refused: a deploy key that
// is mounted works perfectly, and freezing over it would refuse a deployment
// that syncs (§8).
func TestAnOriginThatAuthenticatesDifferentlyFromTheConfiguredRemoteIsSaidOnce(t *testing.T) {
	t.Parallel()

	// The same unreachable host the swap test uses: what this is about is what
	// obsync says about the pair, not what a network does with it.
	env := newVaultReachedBy(t, func(e *vaultEnv) (string, []string) {
		return "https://127.0.0.1:1/owner/vault.git",
			[]string{"OBSYNC_TOKEN_FILE=" + writeCredential(t, "ghp_the_operators_token\n")}
	})
	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, "ssh://git@127.0.0.1:1/owner/vault.git")
	env.turn()
	env.awaitIdle()

	said := env.saidSoFar()
	if !strings.Contains(said, credentialPathMismatch) {
		t.Errorf("obsync said %q about an ssh origin under an https configuration, want the one "+
			"WARN that says the credential it holds is never read — the alternative is a "+
			"deployment that fails on the abort tier and says nothing at all (§8, §9)", said)
	}
	if !strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q, want it at WARN: it is true, advisory, and refuses nothing (§9)", said)
	}
	if strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q about an origin that authenticates differently, want no freeze — "+
			"a mounted deploy key syncs perfectly, and only the host and the path decide where "+
			"bytes go (§8)", said)
	}

	env.advance(70 * time.Second)

	if got := strings.Count(env.saidSoFar(), credentialPathMismatch); got != 1 {
		t.Errorf("obsync said it %d times, want exactly once — it is a fact about the deployment, "+
			"said at bootstrap rather than once a tick (§9)", got)
	}
}

// Gate 1, and the exception §7 names: an unwritable vault is the one refusal
// where obsync cannot write an attention note either, so logs are the only
// channel. It is also what catches a UID mismatch conclusively, which is why
// §8 can leave UID/GID to documentation rather than to code.
func TestAVaultObsyncCannotWriteIsAFullFreezeSaidInTheLogAlone(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.theVaultBecomesUnwritable()
	env.turn()
	env.awaitIdle()

	if env.vaultHoldsYet("obsync-attention.md") {
		t.Error("obsync wrote into a vault it cannot write, want nothing: gate 1 is the one " +
			"refusal where the log is the only channel (§7)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned on an unwritable vault, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q about an unwritable vault, want the one ERROR a full freeze "+
			"enters with — and it is the only channel there is here (§7, §9)", said)
	}

	env.theVaultBecomesWritableAgain()
	env.writeNote("Daily/2026-08-24.md", "written once the vault was writable\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written once the vault was writable\n" {
		t.Errorf("the remote holds %q once the vault was writable, want the note obsync deferred: "+
			"every freeze clears when its cause is repaired, with no restart (§7)", got)
	}
}

// Gate 8. The advisory flock dies with the process, so a crash leaves no stale
// lock to clean up — unlike a PID file. It guards against a second obsync; it
// cannot guard against a human running git in the vault, which gate 4 catches.
func TestASecondObsyncOnTheSameVaultIsAFullFreezeUntilTheFirstLetsGo(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	second := env.aSecondObsyncOnTheSameVault()
	env.writeNote("Daily/2026-08-24.md", "written while two obsyncs were pointed at one vault\n")
	second.wake()

	if second.commitsSoFar(second.vault) != "1" {
		t.Errorf("the second obsync took the vault to %s commits, want it to have touched nothing: "+
			"the lock is what keeps one vault to one obsync (§7)", second.commitsSoFar(second.vault))
	}
	if said := second.log.String(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("the second obsync said %q, want the one ERROR a full freeze enters with (§9)", said)
	}

	// The first obsync is still the one syncing the vault, which is the half of
	// this that matters: the lock refuses the newcomer rather than the holder.
	env.advance(70 * time.Second)
	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while two obsyncs were pointed at one vault\n" {
		t.Errorf("the remote holds %q, want the first obsync still syncing normally (§7)", got)
	}

	// The lock dies with the process that holds it, so the second obsync
	// recovers on its own once the first has gone — no restart, and no stale
	// lock to clean up.
	env.stop()
	second.writeNote("Daily/2026-08-25.md", "written after the first obsync stopped\n")
	second.wake()

	if got := second.remoteFile("Daily/2026-08-25.md"); got != "written after the first obsync stopped\n" {
		t.Errorf("the remote holds %q once the first obsync let go, want the second syncing: an "+
			"advisory flock leaves nothing behind to clear (§7)", got)
	}
}

// §7's abort tier, and the row that makes it worth having: a remote that is
// unreachable is this pass giving up, not news. Making a transient loss news is
// how the signal becomes noise — and an unreachable remote is *healthy* until
// the backoff ceiling, so an ERROR here would be obsync asking for a human it
// does not need (§7, §9).
func TestAnUnreachableRemoteIsAnAbortedRunAndSaysNothingAboveDebug(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	env.writeNote("Daily/2026-08-24.md", "written while the remote was away\n")
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != "2" {
		t.Errorf("the vault holds %s commits with the remote away, want 2: the local half cannot "+
			"fail for network reasons (§2)", got)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q about an unreachable remote, want nothing above debug: the abort "+
			"tier reports nothing, and an unreachable remote is healthy until the backoff ceiling "+
			"(§7, §9)", said)
	}

	env.remoteBack()
	env.advance(16 * time.Minute)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while the remote was away\n" {
		t.Errorf("the remote holds %q once it was back, want the note obsync had committed (§2)", got)
	}
}

// The other abort-tier row a third writer produces: somebody else's git holds
// `index.lock`, so this pass cannot stage anything. It is a lost race rather
// than a fault, and the next tick retries against a vault that has stopped
// moving (§7).
func TestAnIndexLockHeldBySomeoneElseIsAnAbortedRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.someoneElseHoldsTheIndexLock()
	env.writeNote("Daily/2026-08-24.md", "written while another git held the index\n")
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != "1" {
		t.Errorf("the vault holds %s commits while another git held the index lock, want 1 (§7)", got)
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q about a lost race for the index, want nothing above debug: the "+
			"abort tier reports nothing (§7)", said)
	}

	env.theIndexLockIsReleased()
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while another git held the index\n" {
		t.Errorf("the remote holds %q once the index lock went, want the note the next tick "+
			"committed (§7)", got)
	}
}

// Where two freezes are live at once, full wins (§7). The network freeze is
// still there underneath — the vault's relationship to the remote has not been
// repaired — and it does not get to decide what obsync does, because a full
// freeze already means "touch nothing", which is strictly more.
func TestAFullFreezeWinsOverALiveNetworkFreeze(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// A network freeze first: the remote's history was rewritten under the tip
	// obsync last saw, and the local half goes on committing.
	env.writeNote("Notes/purged.md", "the note the rewrite removes\n")
	env.advance(70 * time.Second)
	env.remoteRewritesItsHistory()
	env.advance(70 * time.Second)
	if !strings.Contains(env.saidSoFar(), "obsync has stopped syncing with the remote until this is repaired") {
		t.Fatalf("obsync said %q, want the network freeze this test is built on (§3)", env.saidSoFar())
	}

	// And now a full one, on top of it.
	env.theVaultGoesEmpty()
	before := env.commitsSoFar(env.vault)
	env.advance(70 * time.Second)

	if got := env.commitsSoFar(env.vault); got != before {
		t.Errorf("the vault holds %s commits, want %s: the local half kept committing under the "+
			"network freeze and must stop under the full one — full wins (§7)", got, before)
	}
	if said := env.saidSoFar(); !strings.Contains(said, frozenAndTouchingNothing) {
		t.Errorf("obsync said %q, want the full freeze it entered on top of the network one (§7)", said)
	}
}

// State entry and state exit are each said exactly once (§9), and a freeze that
// stands is not a state that changed. This is the half of that rule a freeze
// which is *also* answered at bootstrap can break: gate 2 and gate 6 are both
// asked at bootstrap and again at the top of every run, so a run that took the
// bootstrap answer as news would announce the freeze cleared and re-enter it
// one line later, once a tick, for as long as the `.git` stayed gone.
//
// The cost is not noise. An operator reading `docker logs` would be told obsync
// recovered, every minute, while it had not — which is the one thing a log that
// exists to be believed may not say.
func TestARepositoryThatStaysGoneIsSaidOnceRatherThanOnceATick(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.theRepositoryGoes()
	for range 3 {
		env.advance(70 * time.Second)
	}

	said := env.saidSoFar()
	if got, want := strings.Count(said, frozenAndTouchingNothing), 1; got != want {
		t.Errorf("obsync announced the freeze %d times over three ticks with the repository gone, "+
			"want %d: state entry is said exactly once (§9)", got, want)
	}
	if got := strings.Count(said, freezeCleared); got != 0 {
		t.Errorf("obsync said the freeze cleared %d times while the repository was still gone, want "+
			"none: a log that says obsync recovered when it did not is worse than a silent one "+
			"(§7, §9)", got)
	}

	// And the freeze still clears on the run that repairs it, which is the
	// thing the count above must not be bought with.
	env.theRepositoryComesBack()
	env.advance(70 * time.Second)

	if got := strings.Count(env.saidSoFar(), freezeCleared); got != 1 {
		t.Errorf("obsync said the freeze cleared %d times once the repository was back, want 1 "+
			"(§7, §9)", got)
	}
}

// The same rule for gate 6, which bootstrap and the per-run interlocks also
// both answer: an unborn tracked branch stands until a human repairs it, and a
// freeze that stands is not a state that changed (§9).
func TestATrackedBranchThatStaysUnbornIsSaidOnceRatherThanOnceATick(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	tip := env.vaultTipYet()

	env.mustGit(env.vault, "update-ref", "-d", "refs/heads/main")
	for range 3 {
		env.advance(70 * time.Second)
	}

	said := env.saidSoFar()
	if got, want := strings.Count(said, frozenAndTouchingNothing), 1; got != want {
		t.Errorf("obsync announced the freeze %d times over three ticks with the tracked branch "+
			"naming nothing, want %d (§9)", got, want)
	}
	if got := strings.Count(said, freezeCleared); got != 0 {
		t.Errorf("obsync said the freeze cleared %d times while the branch still named nothing, "+
			"want none (§7, §9)", got)
	}

	env.mustGit(env.vault, "update-ref", "refs/heads/main", tip)
	env.advance(70 * time.Second)

	if got := strings.Count(env.saidSoFar(), freezeCleared); got != 1 {
		t.Errorf("obsync said the freeze cleared %d times once the branch named a commit again, "+
			"want 1 (§7, §9)", got)
	}
}

// A remote that is down is an aborted run wherever obsync met it, and the
// probes a frozen obsync re-asks every tick are where that matters most: the
// freeze is already correctly announced, so an ERROR here is obsync asking for
// a human twice about one fact, once a tick, for as long as the remote is away.
//
// The command behind this one is a `fetch --refmap=` rather than the run's own
// fetch, which is the whole of why it was outside the tier: the tier is a fact
// about which command failed, and both of these are obsync having been told
// nothing (§7).
func TestAnUnreachableRemoteUnderANetworkFreezeStillSaysNothingAboveDebug(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// The network freeze the re-check probe belongs to: the remote's history
	// was rewritten under the tip obsync last saw.
	env.writeNote("Notes/purged.md", "the note the rewrite removes\n")
	env.advance(70 * time.Second)
	env.remoteRewritesItsHistory()
	env.advance(70 * time.Second)

	entered := strings.Count(env.saidSoFar(), "level=ERROR")
	if entered != 1 {
		t.Fatalf("obsync said %q, want the one ERROR the network freeze this test is built on "+
			"enters with (§3, §9)", env.saidSoFar())
	}

	// And now the remote goes away underneath the freeze. The probe cannot
	// answer, so obsync stays frozen — the freeze clears on a fact, never on a
	// failure to establish one — and says nothing further.
	env.remoteAway()
	for range 3 {
		env.advance(16 * time.Minute)
	}

	said := env.saidSoFar()
	if got := strings.Count(said, "level=ERROR"); got != entered {
		t.Errorf("obsync said ERROR %d times with the remote away under a network freeze, want %d — "+
			"an unreachable remote is an aborted run whichever command met it, and it reports "+
			"nothing above debug (§7, §9)", got, entered)
	}
	if strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q with the remote away, want nothing above debug (§7)", said)
	}

	// The local half is untouched throughout, which is what a network freeze
	// means and what an unreachable remote must not change.
	env.remoteBack()
	env.writeNote("Notes/still-committing.md", "written while the remote was away\n")
	env.advance(70 * time.Second)

	if got := env.vaultFileYet("Notes/still-committing.md"); got != "written while the remote was away\n" {
		t.Errorf("the vault holds %q, want the note the test wrote", got)
	}
	if got := env.commitsSoFar(env.vault); got == "1" {
		t.Error("obsync stopped committing locally, want the local half still running under a " +
			"network freeze (§7)")
	}
}

// A second interlock failing while the first still stands is a *different*
// state, and state entry is said once per state rather than once ever (§9).
//
// The cost of holding the first name is not noise, it is the opposite: the
// operator does exactly what the log asked them to do, the fact they repaired
// stops being true, and obsync goes on naming it while standing frozen on
// something else it never mentioned. That is §7's "every freeze self-clears
// when its cause is repaired" failing in the one way an operator cannot see.
func TestASecondInterlockFailingIsSaidRatherThanHiddenBehindTheFirst(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.mustGit(env.vault, "checkout", "--quiet", "--detach", "HEAD")
	env.advance(70 * time.Second)
	if !strings.Contains(env.saidSoFar(), "freeze=\"the vault's HEAD is detached\"") {
		t.Fatalf("obsync said %q, want gate 3's freeze this test is built on (§7)", env.saidSoFar())
	}

	// A second fact arrives underneath the first: origin now points at a
	// remote obsync was never given.
	elsewhere := env.aSecondBareRemote()
	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, "file://"+elsewhere)
	// And the human does exactly what the log told them to.
	env.mustGit(env.vault, "checkout", "--quiet", "main")
	env.advance(70 * time.Second)

	said := env.saidSoFar()
	if !strings.Contains(said, "freeze=\"the vault's origin is not the remote obsync was given\"") {
		t.Errorf("obsync said %q after the human repaired the fact it named, want gate 5 — the "+
			"freeze standing now — said in its own right: an operator who has done what the log "+
			"asked and is told nothing has no way left to find out (§7, §9)", said)
	}

	// And it still clears, on the fact that is actually holding it.
	env.mustGit(env.vault, "remote", "set-url", config.RemoteName, env.repoURL)
	env.writeNote("Daily/2026-08-24.md", "written once both were repaired\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written once both were repaired\n" {
		t.Errorf("the remote holds %q once both facts were repaired, want the note obsync "+
			"deferred (§7)", got)
	}
	if got := strings.Count(env.saidSoFar(), freezeCleared); got != 1 {
		t.Errorf("obsync said the freeze cleared %d times, want 1 — state exit is said once (§9)", got)
	}
}
