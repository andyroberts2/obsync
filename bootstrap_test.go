package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
)

// Bootstrap is the one decision obsync makes about a vault before it syncs it,
// and it has three answers (#26, §3, gate 2): clone into an empty directory,
// attach to a directory that is already a repo, refuse a directory that is
// neither. Each case is answered by the thing that has an opinion — which is
// why the tracked branch comes from the remote when obsync clones and from the
// vault when it attaches, and never from whichever of the two was easier to
// ask.
//
// Everything below is seam 1: a real vault directory, a real bare remote over
// file://, real git, and the loop driven end to end.

// An operator points obsync at an empty directory and it clones. The tracked
// branch is the remote's default branch, because a directory with no repo in it
// has no opinion about a branch and the remote does.
func TestAnEmptyVaultDirectoryIsClonedFromTheRemote(t *testing.T) {
	t.Parallel()

	env := newEmptyVault(t, "vault-live", "main")

	env.wake()

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q, want the note the remote holds: obsync clones into an empty "+
			"directory rather than refusing it (§3)", got)
	}
}

// What obsync was *told* is not what obsync will use. OBSYNC_REPO is read for
// the clone and for gate 5's comparison, and every fetch and every push after
// that goes to the vault's own `origin`, because obsync never writes a human's
// `.git/config` and never runs `git remote set-url` (§3, §8).
//
// So the pair is said rather than half of it: the configured remote on the
// startup line, and the origin obsync resolved here. An operator diagnosing a
// vault that is not syncing is otherwise reading a URL obsync may never have
// contacted.
//
// The normalised pair and the scheme, never the URL itself: an operator may put
// a token in an origin even though obsync never does, and gate 5 refuses to
// echo one for the same reason (§8).
func TestTheRepositoryObsyncResolvedIsSaidWithTheSchemeGitWillUse(t *testing.T) {
	t.Parallel()

	env := newVault(t)

	env.wake()

	said := env.said()
	if !strings.Contains(said, `msg="resolved the vault's repository"`) {
		t.Errorf("obsync said %q, want the line naming the repository it resolved — the startup "+
			"line says what obsync was told, and this says what obsync will use (§8, §9)", said)
	}
	if want := "origin=file://" + strings.TrimSuffix(env.remote, ".git"); !strings.Contains(said, want) {
		t.Errorf("obsync said %q, want it to carry %q: the transport is the half of the origin "+
			"gate 5 discards, and the half an operator needs to see (§8)", said, want)
	}
	if !strings.Contains(said, "branch=main") {
		t.Errorf("obsync said %q, want the tracked branch it resolved beside the origin (§3)", said)
	}
}

// The tracked branch is the remote's *default* branch and not whichever branch
// happens to be called main: taking main would silently start syncing a branch
// nobody chose on a remote whose vault lives on another one (§3).
func TestACloneTracksTheRemotesDefaultBranchRatherThanMain(t *testing.T) {
	t.Parallel()

	env := newEmptyVault(t, "vault-live", "main")

	env.wake()
	env.writeNote("Daily/2026-08-24.md", "written after the clone\n")
	env.wake()

	if got := env.remoteFileOn("vault-live", "Daily/2026-08-24.md"); got != "written after the clone\n" {
		t.Errorf("the remote's vault-live holds %q at the note's path, want the vault's bytes: the "+
			"tracked branch is the remote's default branch (§3)", got)
	}
	if _, code := env.git(env.remote, "cat-file", "-e", "refs/heads/main:Daily/2026-08-24.md"); code == 0 {
		t.Error("the remote's main gained the note too, want obsync to sync one branch and only the " +
			"one the remote calls default (§3)")
	}
}

// An operator points obsync at a non-empty directory that is not a repo and it
// refuses, because it must never adopt a folder it cannot reason about (§3,
// gate 2). Refusing is parking alive rather than exiting: the directory is left
// exactly as it was found, and emptying it releases obsync with no restart.
func TestADirectoryThatIsNotARepoIsNeverAdopted(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	env.writeNote("someone else's notes.md", "a folder that belongs to something else\n")

	env.turn()
	env.awaitIdle()

	if env.vaultHoldsYet(".git") {
		t.Error("obsync made the directory a repo, want it left alone: a non-empty directory that " +
			"is not a repo is refused rather than adopted (§3)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when it refused the directory, want it parked alive: a " +
			"refused obsync keeps re-checking, because exiting turns a diagnosable state into a " +
			"crash loop (§7)")
	}
	if env.vaultHoldsYet("Notes/from the remote.md") {
		t.Error("obsync brought the remote's notes into the directory it refused, want nothing taken " +
			"and nothing given")
	}

	// The cause repaired, and nothing restarted: the directory is emptied of
	// what obsync could not reason about, and the next tick bootstraps it.
	env.deleteNote("someone else's notes.md")
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q after the directory was emptied, want the remote's note: every "+
			"refusal clears when its cause is repaired, with no restart (§7)", got)
	}
	if said := env.said(); !strings.Contains(said, "someone else's notes.md") {
		t.Errorf("obsync said %q about the directory it refused, want the path that made it refuse "+
			"named, because a refusal states the conclusive fact behind it (§9)", said)
	}
}

// Ignore-floor cruft is not a folder obsync cannot reason about — gate 2 says
// so, and a .DS_Store left by a volume that was once mounted on a Mac should
// not cost anyone their deployment. git will not clone into a directory that
// holds anything at all, though, so obsync says which entry is in the way
// instead of leaving an operator to read a clone's refusal and guess.
func TestCruftInTheWayOfACloneIsNamedRatherThanTreatedAsAVault(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	env.writeNote(".DS_Store", "cruft\n")

	env.turn()
	env.awaitIdle()

	said := env.saidSoFar()
	if !strings.Contains(said, ".DS_Store") {
		t.Errorf("obsync said %q about a directory holding only cruft, want the entry in the way "+
			"named (§9)", said)
	}
	if strings.Contains(said, "will not adopt") {
		t.Errorf("obsync said %q, want it not to refuse cruft as a folder it cannot reason about: "+
			"the ignore floor is what tells those two apart (gate 2, §5)", said)
	}

	// One file moved, and no restart: the refusal names something an operator
	// can act on in one command.
	env.deleteNote(".DS_Store")
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q once the cruft was gone, want the remote's note (§7)", got)
	}
}

// An operator points obsync at a vault that is already a repo and it attaches,
// on the branch that vault is already sitting on. The remote's default branch
// is not consulted, and that is the whole reason bootstrap has three answers
// rather than one: always taking the remote's default would silently start
// syncing main on a vault sitting on vault-live (§3).
func TestAVaultThatIsAlreadyARepoIsAttachedOnTheBranchItIsOn(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	env.makeVaultARepoOn("vault-live")
	env.pushVaultTo("vault-live")
	env.writeNote("Daily/2026-08-24.md", "written in a vault that is already a repo\n")

	env.wake()

	if got := env.remoteFileOn("vault-live", "Daily/2026-08-24.md"); got != "written in a vault that is already a repo\n" {
		t.Errorf("the remote's vault-live holds %q, want the vault's bytes: the tracked branch is the "+
			"branch the vault is on (§3)", got)
	}
	if _, code := env.git(env.remote, "cat-file", "-e", "refs/heads/main:Daily/2026-08-24.md"); code == 0 {
		t.Error("the note reached the remote's main, want obsync to sync the branch the vault is on " +
			"and never the remote's idea of a default (§3)")
	}
}

// OBSYNC_BRANCH is the bootstrap override: it names the branch obsync clones
// and tracks, in place of the one the remote calls default (§3, §8).
func TestTheBranchOverrideDecidesWhatACloneTracks(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrapWith(t, "OBSYNC_BRANCH=vault-live")
	env.seedRemote("main", "vault-live")

	env.wake()
	env.writeNote("Daily/2026-08-24.md", "written after the clone\n")
	env.wake()

	if got := env.remoteFileOn("vault-live", "Daily/2026-08-24.md"); got != "written after the clone\n" {
		t.Errorf("the remote's vault-live holds %q, want the vault's bytes: OBSYNC_BRANCH overrides "+
			"the branch the remote calls default (§3)", got)
	}
	if _, code := env.git(env.remote, "cat-file", "-e", "refs/heads/main:Daily/2026-08-24.md"); code == 0 {
		t.Error("the note reached the remote's main, want the overridden branch and only it (§3)")
	}
}

// An override naming a branch the vault is not on is refused rather than
// checked out. obsync never runs git checkout after bootstrap — it would
// rewrite files a human has open in Obsidian — so the remedy is the human's own
// checkout, and it releases obsync with no restart.
func TestAnOverrideTheVaultIsNotOnIsRefusedRatherThanCheckedOut(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrapWith(t, "OBSYNC_BRANCH=vault-live")
	env.makeVaultARepoOn("main")
	env.mustGit(env.vault, "branch", "vault-live")
	env.pushVaultTo("main")
	env.pushVaultTo("vault-live")
	env.writeNote("Daily/2026-08-24.md", "written while obsync was refusing\n")

	env.turn()
	env.awaitIdle()

	if head := strings.TrimSpace(env.mustGit(env.vault, "symbolic-ref", "--short", "HEAD")); head != "main" {
		t.Errorf("the vault's HEAD is on %q, want main: obsync never checks a branch out (§3)", head)
	}
	if got, want := env.commitsSoFar(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a refused obsync commits nothing", got, want)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned when it refused the override, want it parked alive (§7)")
	}

	// The human checks the branch out themselves, which is the remedy the
	// refusal names, and obsync attaches on the next tick.
	env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "checkout", "--quiet", "vault-live")...)
	env.advance(70 * time.Second)

	if got := env.remoteFileOn("vault-live", "Daily/2026-08-24.md"); got != "written while obsync was refusing\n" {
		t.Errorf("the remote's vault-live holds %q once the human checked it out, want the vault's "+
			"bytes: a refusal clears when its cause is repaired, with no restart (§7)", got)
	}
}

// A .git left half-written by a killed clone has config and HEAD but an unborn
// branch. obsync refuses it rather than repairing it: it cannot tell that from
// a human having broken HEAD in a repo that holds commits, and the safe reading
// of the pair is the one that touches nothing (§7).
func TestAHalfWrittenRepoIsRefusedRatherThanRepaired(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	env.mustGit(env.vault, "init", "--quiet", "-b", "main")
	env.mustGit(env.vault, "remote", "add", "origin", env.repoURL)
	env.writeNote("Daily/2026-08-24.md", "written into the debris\n")

	env.turn()
	env.awaitIdle()

	// show-ref exits 1 on a repo holding no refs at all, which is the state
	// obsync must have left untouched.
	if refs, _ := env.git(env.vault, "show-ref"); strings.TrimSpace(refs) != "" {
		t.Errorf("the vault holds refs %q, want none: obsync neither completed the clone nor made a "+
			"commit in a repo it refused (§7)", refs)
	}
	if env.vaultHoldsYet("Notes/from the remote.md") {
		t.Error("obsync fetched the remote into the half-written repo, want it refused rather than " +
			"repaired: obsync never re-clones and never self-repairs (§7)")
	}
	// A refusal touches nothing, and the index is the cheapest place that shows
	// it: an obsync that attached to this repo instead would stage the vault
	// and only then discover that HEAD names no commit.
	if staged := strings.TrimSpace(env.mustGit(env.vault, "ls-files", "--cached")); staged != "" {
		t.Errorf("the half-written repo has %q staged, want an untouched index: obsync refuses a repo "+
			"whose tracked branch holds no commits rather than working in it (§7)", staged)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned over a half-written repo, want it parked alive (§7)")
	}
	if said := env.saidSoFar(); !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about a repo it refused, want a human told (§9)", said)
	}
}

// A brand-new empty remote is the documented first run: the remote has no refs
// at all, so the first push is what creates the tracked branch on it (§3).
func TestAFirstPushCreatesTheBranchOnARemoteWithNoRefsAtAll(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.makeVaultARepoOn("main")
	env.writeNote("Daily/2026-08-24.md", "the first thing this remote ever held\n")

	env.wake()

	if got := env.remoteFileOn("main", "Daily/2026-08-24.md"); got != "the first thing this remote ever held\n" {
		t.Errorf("the remote holds %q, want the vault's bytes: a remote with no refs at all is one "+
			"obsync may create the tracked branch on (§3)", got)
	}
}

// The sharpest rule in this slice: a remote that has refs but not the tracked
// branch is a full freeze, and obsync never creates the branch there. The
// branch name came from local HEAD, so a stray branch or a typo'd override
// would otherwise create a remote branch and cheerfully sync an entire vault
// into it — and the push would succeed (§3).
//
// Full freeze means the local half stops too, and the documented cost is one
// deliberate manual push, which releases obsync with no restart.
func TestARemoteHoldingRefsButNotTheTrackedBranchIsAFullFreeze(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	env.makeVaultARepoOn("vault-live")
	env.writeNote("Daily/2026-08-24.md", "never pushed anywhere nobody agreed on\n")

	env.turn()
	env.awaitIdle()

	if env.remoteHoldsBranchYet("vault-live") {
		t.Error("obsync created vault-live on a remote that holds other refs, want it frozen: the " +
			"branch name came from local HEAD, and creating it would sync an entire vault into a " +
			"branch nobody agreed on (§3)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned on a full freeze, want it parked alive and re-checking (§7)")
	}

	// A full freeze stops everything, including the local half, so the run
	// after the freeze commits nothing.
	committed := env.commitsOnBranchYet(env.vault, "vault-live")
	env.writeNote("Daily/2026-08-25.md", "written while obsync was frozen\n")
	env.advance(70 * time.Second)

	if got := env.commitsOnBranchYet(env.vault, "vault-live"); got != committed {
		t.Errorf("the vault holds %s commits while obsync is fully frozen, want it still at %s: a "+
			"full freeze stops obsync touching the repo at all (§7)", got, committed)
	}
	if said := env.saidSoFar(); !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q entering a full freeze, want a human told (§9)", said)
	}

	// The documented cost, paid: one deliberate manual push, and obsync picks
	// the branch up on its next run with no restart (§3, §7).
	env.mustGit(env.vault, "push", "--quiet", "file://"+env.remote, "refs/heads/vault-live:refs/heads/vault-live")
	env.advance(70 * time.Second)

	if got := env.remoteFileOn("vault-live", "Daily/2026-08-25.md"); got != "written while obsync was frozen\n" {
		t.Errorf("the remote's vault-live holds %q once a human created it, want the note obsync "+
			"deferred: every freeze clears when its cause is repaired, with no restart (§7)", got)
	}
}

// A remote with no refs at all has no default branch to resolve, so there is
// nothing to clone into an empty directory. obsync leaves the directory alone
// rather than cloning an empty repository into it — that would leave a repo
// whose HEAD names no commit, which is the state obsync refuses on every later
// run, in a directory that was empty a moment ago.
func TestAnEmptyRemoteLeavesAnEmptyDirectoryAloneUntilItHasSomethingToClone(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)

	env.turn()
	env.awaitIdle()

	if env.vaultHoldsYet(".git") {
		t.Error("obsync made a repo in the vault from a remote holding no refs, want the directory " +
			"left as it was: a repo whose HEAD names no commit is one obsync refuses forever (§7)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned over an empty remote, want it parked alive (§7)")
	}

	// The remote gains a vault, and obsync clones it on the next tick.
	env.seedRemote("main")
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q once the remote had something to clone, want the remote's "+
			"note (§7)", got)
	}
}

// Exactly one repo, one remote named origin, one tracked branch, and a refspec
// that is one branch in each direction — no tags, no other refs (§3). The
// remote here holds two branches and a tag, and the vault obsync built holds
// neither the other branch nor the tag.
func TestACloneBringsOneBranchAndNoTagsAndOneRemote(t *testing.T) {
	t.Parallel()

	env := newEmptyVault(t, "vault-live", "main")

	env.wake()

	if got, want := strings.Fields(env.mustGit(env.vault, "remote")), []string{config.RemoteName}; !slices.Equal(got, want) {
		t.Errorf("the vault has remotes %v, want exactly %v: the remote name is not a knob and there "+
			"is only ever one (§3, §8)", got, want)
	}
	if got := strings.TrimSpace(env.mustGit(env.vault, "tag", "--list")); got != "" {
		t.Errorf("the vault holds tags %q, want none: obsync's refspec is one branch in each "+
			"direction (§3)", got)
	}
	if _, code := env.git(env.vault, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); code == 0 {
		t.Error("the vault holds a remote-tracking ref for the remote's other branch, want only the " +
			"tracked branch's (§3)")
	}
	// The fetch that reads this refspec is #27's; what bootstrap promises is
	// that it names one branch when it arrives.
	if got, want := strings.TrimSpace(env.mustGit(env.vault, "config", "--get-all", "remote.origin.fetch")),
		"+refs/heads/vault-live:refs/remotes/origin/vault-live"; got != want {
		t.Errorf("the vault's fetch refspec is %q, want %q: one branch in each direction (§3)", got, want)
	}
}

// The push side of the same rule, and it is not obsync's own config that
// decides it: `push.followTags` in the vault's .git/config — which is the
// human's file, and outranks obsync's private one — otherwise carries an
// annotated tag along with obsync's push. Measured on both matrix points.
func TestNoTagTravelsWithAPushWhateverTheVaultsConfigSays(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.mustGit(env.vault, "config", "push.followTags", "true")
	env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "tag", "-a", "v1", "-m", "the human's tag")...)
	env.writeNote("Daily/2026-08-24.md", "one branch, in each direction\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "one branch, in each direction\n" {
		t.Errorf("the remote holds %q, want the vault's bytes", got)
	}
	if got := strings.TrimSpace(env.mustGit(env.remote, "tag", "--list")); got != "" {
		t.Errorf("the remote holds tags %q, want none: obsync pushes one branch and nothing else, "+
			"whatever the vault's own config says (§3)", got)
	}
}

// The tracked branch is resolved once at startup and fixed for the process
// lifetime (§3), and the reason is that following HEAD wherever it goes makes
// the sync target something a human changes by accident. So a human checking
// out another branch in the vault does not move obsync onto it — even when the
// remote holds that branch and pushing to it would have worked.
//
// What obsync commits meanwhile still goes to HEAD, which is a state the run's
// own gate closes rather than bootstrap: HEAD moving off the tracked branch
// mid-run is a full freeze, and that gate is #32's.
func TestTheTrackedBranchIsFixedForTheProcessLifetime(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.makeVaultARepoOn("main")
	env.mustGit(env.vault, "branch", "vault-live")
	env.pushVaultTo("main")
	env.pushVaultTo("vault-live")

	env.turn()
	env.awaitIdle()

	// The human moves HEAD under obsync, to a branch the remote holds.
	env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "checkout", "--quiet", "vault-live")...)
	env.writeNote("Daily/2026-08-24.md", "written after a human moved HEAD\n")
	env.advance(70 * time.Second)

	if _, code := env.git(env.remote, "cat-file", "-e", "refs/heads/vault-live:Daily/2026-08-24.md"); code == 0 {
		t.Error("obsync pushed the branch a human checked out under it, want the branch it resolved " +
			"at startup: the tracked branch is fixed for the process lifetime, because a sync target " +
			"that follows HEAD is one a human changes by accident (§3)")
	}
}

// A remote holding refs whose HEAD names a branch it does not hold is the same
// refusal as an empty one, and it is the case that makes the check worth doing
// before anything is written rather than after: cloning it succeeds — exit 0,
// measured on both matrix points — and leaves a repo with no refs and an unborn
// HEAD, which is the state obsync then refuses on every later run, in a
// directory that was empty a moment ago.
func TestARemoteWhoseHeadNamesNothingIsNotClonedIntoAnEmptyDirectory(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("vault-live")
	env.mustGit(env.remote, "symbolic-ref", "HEAD", "refs/heads/main")

	env.turn()
	env.awaitIdle()

	if env.vaultHoldsYet(".git") {
		t.Error("obsync cloned a remote whose HEAD names a branch it does not hold, want the " +
			"directory left as it was: that clone succeeds and leaves a repo obsync would refuse " +
			"forever (§3, §7)")
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned, want it parked alive and re-checking (§7)")
	}

	// The remote's HEAD pointed at a branch it holds, and obsync clones on the
	// next tick.
	env.mustGit(env.remote, "symbolic-ref", "HEAD", "refs/heads/vault-live")
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q once the remote's HEAD named a branch it holds, want the "+
			"remote's note (§7)", got)
	}
}

// Gate 2 refuses a non-empty directory that is not a repo, and tolerates
// ignore-floor cruft (§7, §5). Which of the two refusals an operator gets is
// the difference between moving one file and reconsidering their deployment,
// so the floor's reading has to be git's reading and not obsync's: #28 writes
// this same closed list into `.git/info/exclude`, where git is what applies it,
// and a floor with two readings is a floor that means two things.
//
// Each row below was measured against real git with the floor in an exclude
// file, and the assertion is on obsync's own words because the two refusals
// have no other difference — both leave the directory exactly as it was.
func TestGateTwoReadsTheIgnoreFloorTheWayGitDoes(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		path string
		// covered is git's answer, measured: does the floor cover this path?
		covered bool
	}{
		{"a Mac's cruft at the vault root", ".DS_Store", true},
		{"a Mac's cruft inside a folder", "Attachments/.DS_Store", true},
		{"Obsidian's workspace file", ".obsidian/workspace.json", true},
		{"a plugin's settings", ".obsidian/plugins/dataview/data.json", true},
		{"Obsidian's trash", ".trash/deleted note.md", true},
		// A trailing slash does not anchor a gitignore pattern, so `.trash/`,
		// `.vscode/` and `.idea/` name those folders wherever they sit.
		{"a trash folder further down", "Notes/.trash/deleted note.md", true},
		{"an editor's folder further down", "Notes/.vscode/settings.json", true},
		{"obsync's own attention note in a folder", "Notes/obsync-attention.md", true},
		// The entries that do carry a slash are anchored, and this is that rule
		// in the direction that makes it a rule: `.obsidian/workspace.json`
		// names the vault's own .obsidian, never a folder of that name further
		// down, which is a note someone wrote and obsync must not sweep past.
		{"a workspace file in a folder of that name further down", "Notes/.obsidian/workspace.json", false},
		{"a plugin's settings further down", "Notes/.obsidian/plugins/dataview/data.json", false},
		{"an ordinary note", "Notes/a note.md", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			env := newVaultToBootstrap(t, nil)
			env.seedRemote("main")
			env.writeNote(row.path, "whatever this is\n")

			env.turn()
			env.awaitIdle()

			said := env.saidSoFar()
			if adopting := strings.Contains(said, "will not adopt"); adopting == row.covered {
				t.Errorf("obsync said %q about a directory holding only %q, want the ignore floor to "+
					"cover it: %v (§5, gate 2)", said, row.path, row.covered)
			}
			if env.vaultHoldsYet(".git") {
				t.Errorf("obsync made a repo in a directory holding %q, want it refused and left "+
					"exactly as it was (§7)", row.path)
			}
		})
	}
}

// A directory holding nothing but an empty folder is one gate 2 tolerates and
// git will not clone into, so it gets the refusal that names the entry in the
// way — and that refusal has to state a fact rather than a guess (§9). The
// floor covers no folder by that name; what is true is that obsync would have
// adopted what is there and git will not clone into a directory holding
// anything at all.
func TestTheCruftRefusalStatesWhyGitWillNotCloneRatherThanGuessing(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.seedRemote("main")
	if err := os.MkdirAll(filepath.Join(env.vault, "Attachments"), 0o755); err != nil {
		t.Fatalf("creating the empty folder: %v", err)
	}

	env.turn()
	env.awaitIdle()

	said := env.saidSoFar()
	if !strings.Contains(said, "Attachments") {
		t.Errorf("obsync said %q about a directory holding one empty folder, want the entry in the "+
			"way named (§9)", said)
	}
	if strings.Contains(said, "ignore floor covers it") {
		t.Errorf("obsync said %q, want it not to claim the ignore floor covers a folder the floor "+
			"has never heard of: a refusal states the conclusive fact behind it (§9)", said)
	}

	// The one entry moved, and no restart.
	if err := os.Remove(filepath.Join(env.vault, "Attachments")); err != nil {
		t.Fatalf("removing the empty folder: %v", err)
	}
	env.advance(70 * time.Second)

	if got := env.vaultFile("Notes/from the remote.md"); got != remoteSeedNote {
		t.Errorf("the vault holds %q once the folder was gone, want the remote's note (§7)", got)
	}
}

// A branch ref obsync cannot read is damage rather than bootstrap debris — it
// is what an unclean shutdown leaves (§7) — and the two are told apart by what
// obsync says, not by what it does: both are refused and both touch nothing.
// What obsync says has to be git's own account, because announcing damage as a
// half-written clone points an operator at deleting a `.git` that may hold the
// only copy of their unpushed commits, which is the one repair this design
// refuses to make.
//
// So the refusal carries the failing git and git's words, which is what
// wrapping is for: git's words may *name* a failure and obsync's may never
// invent one (§1, §7).
func TestADamagedRepoIsRefusedInGitsOwnWordsAndTouchedNotAtAll(t *testing.T) {
	t.Parallel()

	env := newVaultToBootstrap(t, nil)
	env.makeVaultARepoOn("main")
	env.pushVaultTo("main")

	broken := filepath.Join(env.vault, ".git", "refs", "heads", "main")
	if err := os.WriteFile(broken, []byte("not a sha\n"), 0o644); err != nil {
		t.Fatalf("breaking the branch ref: %v", err)
	}

	env.turn()
	env.awaitIdle()

	if said := env.saidSoFar(); !strings.Contains(said, "symbolic-ref") {
		t.Errorf("obsync said %q about a repo whose branch ref it could not read, want the failing "+
			"git named so a human gets git's own account rather than obsync's guess (§1, §7)", said)
	}
	if held, err := os.ReadFile(broken); err != nil || string(held) != "not a sha\n" {
		t.Errorf("the vault's branch ref holds %q (%v), want it exactly as obsync found it: obsync "+
			"refuses a repo it cannot resolve rather than repairing it (§7)", held, err)
	}
	if !env.stillTurning() {
		t.Error("obsync's sync loop returned over a repo it could not resolve a branch in, want it " +
			"parked alive (§7)")
	}
}
