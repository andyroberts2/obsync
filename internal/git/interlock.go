package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/andyroberts2/obsync/internal/config"
)

// InterlockFailure is an interlock that is not holding: which one, the
// conclusive fact behind it, and what a human does about it (§7).
//
// It is named for the wider word rather than for the gates, because the vault
// sentinel travels in it too and the sentinel is not a gate — it is a fact
// about the vault rather than about the repository (CONTEXT.md).
//
// Every one of them is a full freeze. They are values rather than sentinel
// errors because the loop does not branch on which: it says the fact and the
// remedy, touches nothing, and asks again next run. What varies between them
// is the words, and the words are here beside the check that establishes them
// rather than in a table one file away from the fact it describes.
//
// It is an error as well as a value so that bootstrap — which answers the four
// interlocks obsync cannot re-check its way out of — can return one through the
// ordinary error path.
type InterlockFailure struct {
	// Interlock is the freeze's name, and the one an operator sees in the log
	// and in the attention note (#38).
	Interlock string
	// Fact is the conclusive thing obsync observed. Every interlock is a fact
	// rather than a judgement, which is what makes it safe to act on.
	Fact string
	// Remedy is what a human does, closing with the sentence that is the whole
	// point of a design whose freezes self-clear.
	Remedy string
}

func (g *InterlockFailure) Error() string { return g.Interlock + ": " + g.Fact }

// SelfClearing is the last sentence of every remedy obsync writes, and it is
// **load-bearing documentation** in the strictest sense (§11, #16): every
// freeze in this design clears when its cause is repaired, and that is worth
// nothing if the operator's reflex is to restart the container and destroy the
// diagnosis. Never cut it for brevity.
//
// It is one exported constant rather than a sentence each remedy spells for
// itself, and that is the point: a line the design leans on this hard may not
// be a thing twelve call sites remember to say, in whichever words each of them
// picked.
const SelfClearing = ". This clears on its own once fixed; no restart needed"

// The interlocks' freeze names — the nine gates of §7's table plus the vault
// sentinel, which is a full freeze of its own rather than a tenth gate.
//
// They are the names the log and the attention note carry, so they read as the
// fact an operator is being told about rather than as a number in a table only
// the spec has.
const (
	// gate 1, process lifetime: the vault path exists, is a directory, and is
	// writable by obsync's UID. The one refusal where obsync cannot write an
	// attention note either, so the log is the only channel (§7).
	freezeVaultUnusable = "the vault is not usable"

	// gate 2: the directory obsync was pointed at is a repository. At
	// bootstrap that is "clone if empty, attach if a repo, refuse if non-empty
	// non-repo"; every run afterwards it is `.git` still being there, which
	// §7's tier table names in its own right. obsync never re-clones, so a
	// repository that has gone is a freeze rather than a bootstrap.
	freezeNoRepository = "the vault holds no repository"

	// gate 3: HEAD is on a branch. A detached HEAD has no branch to commit to
	// and obsync never checks one out.
	freezeDetachedHead = "the vault's HEAD is detached"

	// gate 4: no interrupted rebase, merge, cherry-pick or bisect. A human's
	// half-finished operation is theirs to finish, and committing into one
	// would finish it for them with obsync's own idea of the answer.
	freezeInterruptedOperation = "an interrupted git operation is in progress"

	// gate 5: the repo's origin is the remote obsync was configured with,
	// compared as the normalised (host, path) pair of §8. obsync never runs
	// `git remote set-url`: adopting a new remote silently is the failure
	// where the push succeeds and an entire vault lands somewhere nobody
	// chose.
	freezeRemoteMismatch = "the vault's origin is not the remote obsync was given"

	// gate 6: the tracked branch names a commit. An unborn branch is what a
	// clone killed halfway leaves behind, and obsync cannot tell that from a
	// human having broken HEAD in a repo that holds history.
	freezeBranchUnresolved = "the tracked branch names no commit"

	// gate 7: git is at or above the git floor. Below it obsync's plumbing is
	// not there to be driven — §4's whole out-of-tree merge stands on `merge-
	// tree --write-tree`, which landed at the floor.
	freezeGitBelowTheFloor = "git is older than obsync's floor"

	// gate 8, process lifetime: the advisory flock on `.git/obsync.lock`. It
	// guards against a second obsync; it cannot guard against a human running
	// git in the vault, which gate 4 catches on the next run.
	freezeSecondObsync = "another obsync holds this vault"

	// gate 9: `refs/obsync/failed-apply` does not exist. Keyed on a ref rather
	// than on process lifetime so that a restart cannot clear it, which is
	// what lets §7 say every freeze self-clears with no footnote (§9).
	freezeFailedApplyAnchor = "obsync could not verify a tree it applied"

	// The vault sentinel is not a gate — it is a fact about the vault rather
	// than about the repository — and it is the full freeze that matters most:
	// the mount drops, `git status` reports every tracked file deleted, and a
	// fail-open local half would faithfully commit the deletion of the entire
	// vault and push it to every other clone (§7).
	freezeVaultSentinel = "the vault sentinel is missing"
)

// InterlockFreezes is every full-freeze name the interlocks produce, so that a
// run finding all of them holding can clear the one it was in without asking
// which one put it there.
//
// They clear together because they are re-checked together: gates 2–7 and 9
// and the vault sentinel are asked at the top of every run, and Refusing
// answers with nothing only when every one of them holds. The three process-
// lifetime ones are in the list because bootstrap is retried on every run for
// as long as it has not succeeded, which is the same shape one run further out.
var InterlockFreezes = []string{
	freezeVaultUnusable,
	freezeNoRepository,
	freezeDetachedHead,
	freezeInterruptedOperation,
	freezeRemoteMismatch,
	freezeBranchUnresolved,
	freezeGitBelowTheFloor,
	freezeSecondObsync,
	freezeFailedApplyAnchor,
	freezeVaultSentinel,
}

// BootstrapFreezes is the subset bootstrap answers — gates 1, 2, 6 and 8 —
// which is exactly the interlocks a run that has no repository yet can have
// been stopped by.
//
// It is a list of its own because a bootstrap that got through establishes
// these four and none of the others, so clearing the whole set there would
// announce a freeze cleared and then re-enter it in the same run for a gate
// nothing had looked at yet.
var BootstrapFreezes = []string{
	freezeVaultUnusable,
	freezeNoRepository,
	freezeBranchUnresolved,
	freezeSecondObsync,
}

// vaultSentinel is the folder whose presence is obsync's proof that the vault
// is really there. An Obsidian vault always has it (§7).
const vaultSentinel = ".obsidian"

// sentinelHolds is the vault sentinel asked as a gate is asked: one stat, one
// conclusive fact.
//
// It is deliberately not a count, a ratio or a threshold. A deletion-ratio
// heuristic was rejected for having real false positives and an indefensible
// number; this draws the line exactly where it belongs, because any amount of
// note deletion with `.obsidian/` intact is a human editing their vault, which
// obsync syncs without comment.
//
// A path that is there but is not a directory is the sentinel missing: what
// obsync is looking for is the folder Obsidian keeps its configuration in, and
// a file of that name is not it.
func (r *Repo) sentinelHolds() (*InterlockFailure, error) {
	path := filepath.Join(r.vault, vaultSentinel)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case info.IsDir():
		return nil, nil
	}
	return &InterlockFailure{
		Interlock: freezeVaultSentinel,
		Fact: "the vault at " + r.vault + " holds no " + vaultSentinel + "/ folder, so obsync is " +
			"not looking at the vault",
		Remedy: "check the mount: an Obsidian vault always has a " + vaultSentinel + "/ folder, so " +
			"obsync reads its absence as the vault not being there rather than as your having " +
			"deleted your notes. Nothing has been committed and nothing has been pushed" +
			SelfClearing,
	}, nil
}

// repositoryHolds is gate 2 on every run after the first: the repository obsync
// bootstrapped into is still there.
//
// At bootstrap gate 2 is the three-way decision about a directory — clone if
// empty, attach if a repo, refuse if non-empty non-repo. Afterwards there is
// nothing to decide: obsync never re-clones and never repairs a repo by
// replacing it, so a `.git` that has gone is a freeze, which is what §7's tier
// table names it in its own right.
//
// A `.git` that is a file rather than a directory is a worktree or a submodule
// and is the case bootstrap attached to, so the question is asked with Lstat
// and answered by existence.
func (r *Repo) repositoryHolds() (*InterlockFailure, error) {
	_, err := os.Lstat(filepath.Join(r.vault, ".git"))
	switch {
	case err == nil:
		return nil, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	return &InterlockFailure{
		Interlock: freezeNoRepository,
		Fact:      "the vault at " + r.vault + " no longer holds a .git",
		Remedy: "obsync never re-clones and never repairs a repository by replacing it, because a " +
			"re-clone discards exactly the commits obsync exists to have made. Check the mount " +
			"first; if the repository is genuinely gone, docs/operations.md has the recovery " +
			"recipe" + SelfClearing,
	}, nil
}

// Refusing is every interlock §7 re-checks at the top of a sync run, asked as
// one question: the first one that is not holding, or nil when they all are.
//
// One question rather than seven, because the loop does the same thing with
// every answer — say the fact and the remedy, touch nothing, ask again next
// run — and because the order they are asked in is a property of the set
// rather than of any one of them. The cheap conclusive facts go first: a stat
// that answers "the vault is not there" makes every git below it a command
// obsync would have run against a repository it cannot see.
//
// Gate 7 goes ahead of the gates that read git's own answers, because below the
// floor obsync's plumbing is not there to be driven and an answer from a git
// obsync does not support is not a fact it may act on.
//
// Three of the ten interlocks are not here. Gates 1 and 8 are process-lifetime
// and are bootstrap's, as is the half of gate 2 that decides what a directory
// is — what survives a bootstrap is only that the repository is still there.
// Gate 3 is HeadBranch's, because the branch it answers with is what the run's
// next question is about and one symbolic-ref answers both.
func (r *Repo) Refusing() (*InterlockFailure, error) {
	for _, holds := range []func() (*InterlockFailure, error){
		r.repositoryHolds,
		r.sentinelHolds,
		r.noInterruptedOperation,
		r.gitAtOrAboveTheFloor,
		r.trackedBranchResolves,
		r.originMatches,
		r.noFailedApplyAnchor,
	} {
		failing, err := holds()
		if err != nil || failing != nil {
			return failing, err
		}
	}
	return nil, nil
}

// interruptedOperations is what git leaves in the repository while one of §7's
// four operations is half-finished, and it is the whole of gate 4.
//
// Measured at both matrix points, 2.38.5 and 2.52.0, and the two agree: a
// stopped rebase leaves a `rebase-merge/` directory (or `rebase-apply/` for the
// apply backend), a conflicted merge leaves `MERGE_HEAD`, a stopped cherry-pick
// leaves `CHERRY_PICK_HEAD`, and `git bisect start` leaves `BISECT_LOG`. Each
// is the file git itself looks for, which is why this is a fact rather than an
// inference — `git merge --abort` is exactly "there is no MERGE_HEAD" when
// there is none.
//
// A stopped `git revert` leaves `REVERT_HEAD` and the same unmerged index, and
// it is deliberately not here: §7's list is closed at four, and widening a gate
// is a spec change rather than an implementation detail. Flagged where the code
// is rather than left to be rediscovered.
var interruptedOperations = []string{
	"rebase-merge",
	"rebase-apply",
	"MERGE_HEAD",
	"CHERRY_PICK_HEAD",
	"BISECT_LOG",
}

// noInterruptedOperation is gate 4: no interrupted rebase, merge, cherry-pick
// or bisect.
//
// A human's half-finished operation is theirs to finish. obsync committing into
// one would record its own idea of the answer as if it were theirs, and the
// index it would commit is an unmerged one — which is the state §4's whole
// out-of-tree merge exists so that obsync never creates.
//
// The lock guards against a second obsync; it cannot guard against a human
// running git in their own vault, and this is the gate that catches that on the
// next run (§7).
func (r *Repo) noInterruptedOperation() (*InterlockFailure, error) {
	for _, name := range interruptedOperations {
		_, err := os.Lstat(filepath.Join(r.gitDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &InterlockFailure{
			Interlock: freezeInterruptedOperation,
			Fact: "the repository holds " + name + ", so a rebase, merge, cherry-pick or bisect " +
				"was started in the vault and has not finished",
			Remedy: "finish it or abort it in the vault yourself — obsync will not commit into a " +
				"half-finished operation, because the commit would be its own idea of the answer " +
				"recorded as if it were yours" + SelfClearing,
		}, nil
	}
	return nil, nil
}

// trackedBranchResolves is gate 6: the branch obsync syncs names a commit.
//
// An unborn tracked branch is what a clone killed halfway leaves behind — a
// `.git` with a config and a HEAD and no commit — and obsync cannot tell that
// from a human having broken HEAD in a repo that holds history, so the safe
// reading of the pair is the one that touches nothing (§7).
func (r *Repo) trackedBranchResolves() (*InterlockFailure, error) {
	if _, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", "refs/heads/" + r.branch},
	}); err != nil {
		var command *CommandError
		if !errors.As(err, &command) || command.ExitCode != refDoesNotResolve {
			return nil, err
		}
		return &InterlockFailure{
			Interlock: freezeBranchUnresolved,
			Fact:      "the vault's " + r.branch + " names no commit",
			Remedy: "obsync refuses such a repository rather than repairing it: that is what a " +
				"clone killed halfway leaves behind, and obsync cannot tell it apart from a " +
				"broken HEAD in a repository that holds history. docs/operations.md has the " +
				"recovery recipe" + SelfClearing,
		}, nil
	}
	return nil, nil
}

// noFailedApplyAnchor is gate 9: `refs/obsync/failed-apply` does not exist.
//
// It is the one gate keyed on something a restart cannot clear, and that is the
// whole reason it is a ref: write-verify failing means obsync can no longer
// trust its own view of the vault, and a latch on process lifetime would let an
// operator's reflex — restart the container — clear the one refusal that must
// not be cleared by anything except a human who has looked (§7, §9).
//
// obsync attempts no corrective action: a tool that has just proved it cannot
// apply a tree correctly is the last thing that should try again unsupervised.
// Nothing here writes the ref; that is write-verify's, and it is #33's.
func (r *Repo) noFailedApplyAnchor() (*InterlockFailure, error) {
	if _, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", FailedApplyAnchor},
	}); err != nil {
		var command *CommandError
		if errors.As(err, &command) && command.ExitCode == refDoesNotResolve {
			return nil, nil
		}
		return nil, err
	}
	return &InterlockFailure{
		Interlock: freezeFailedApplyAnchor,
		Fact: "the repository holds " + FailedApplyAnchor + ", so a tree obsync applied to the " +
			"vault was not the tree it had computed",
		Remedy: "this is the one freeze a restart cannot clear, and the one you clear yourself. " +
			FailedApplyAnchor + " holds the tree obsync meant to apply, kept so that a later gc " +
			"cannot prune the one artifact that explains it; compare it against your vault, " +
			"recover what you need, and delete the ref with `git update-ref -d " +
			FailedApplyAnchor + "`. obsync attempts no repair of its own" + SelfClearing,
	}, nil
}

// FailedApplyAnchor is the ref holding the tree obsync computed but could not
// verify. It sits outside refs/heads/ and is never pushed, and it is an owned
// path obsync declared (§10, docs/interface.md).
const FailedApplyAnchor = "refs/obsync/failed-apply"

// refDoesNotResolve is the status a ref that is simply not there gives, as
// against 128, which is git's everything-code and here means the repository
// could not be read at all. It is a documented status rather than prose, which
// is what makes it a fact obsync may branch on.
//
// Two commands answer with it and both are asked for the same kind of fact:
// `rev-parse --verify --quiet` for a ref that does not resolve, and
// `symbolic-ref --quiet` for a HEAD that is not a symbolic ref at all. Measured
// at both matrix points, 2.38.5 and 2.52.0, and the two agree on 1 for each —
// git-symbolic-ref(1) promises only "non-zero", so it is measured rather than
// read off the page, and a different non-zero would travel on as an ordinary
// error rather than be mistaken for gate 3.
const refDoesNotResolve = 1

// originMatches is gate 5: the repository's own `origin` is the remote obsync
// was configured with, compared as the normalised (host, path) pair of §8.
//
// The comparison discards scheme, embedded credentials, a default port, host
// case, a `.git` suffix and a trailing slash — everything that varies without
// changing where bytes go — so an operator swapping a PAT for a deploy key is
// not a freeze, and a different host or a different path is.
//
// obsync never runs `git remote set-url`, and this is the gate that is the
// reason: obsync silently adopting a new remote is the "confidently writes to
// the wrong place" failure, except worse, because the push *succeeds* and an
// entire vault lands somewhere nobody chose.
//
// The URL is read the way git itself resolves it, rather than from the
// repository's own file alone, because where bytes go is the question and git's
// resolution is the answer to it. Reading it is the only thing obsync ever does
// with the vault's `.git/config`: obsync never writes that file.
//
// `--get-all -z`, and the *first* record, because a remote may legally carry
// more than one url and git fetches from the first. Measured at both matrix
// points: `--get-all -z` lists them NUL-separated in the order the config sets
// them, and plain `--get` answers with the *last* — which would have obsync
// comparing against a URL git never uses.
//
// Nothing here echoes the URL. An operator may put a token in one even though
// obsync never does, and a fact written into a log line and an attention note
// is the last place a secret should be reconstructable from — so what is said
// is the normalised pair, which has no credentials in it by construction.
func (r *Repo) originMatches() (*InterlockFailure, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"config", "-z", "--get-all", "remote." + config.RemoteName + ".url"},
	})
	if err != nil {
		var command *CommandError
		if !errors.As(err, &command) || command.ExitCode != configKeyUnset {
			return nil, err
		}
		return &InterlockFailure{
			Interlock: freezeRemoteMismatch,
			Fact: "the vault's repository has no " + config.RemoteName + " remote, and obsync was " +
				"given " + r.configuredRemote.String(),
			Remedy: "set it yourself with `git remote add " + config.RemoteName + " <url>` in the " +
				"vault. obsync never writes your `.git/config` and never runs `git remote " +
				"set-url`, because a remote obsync re-points itself is an entire vault landing " +
				"somewhere nobody chose — and the push would succeed" + SelfClearing,
		}, nil
	}

	first, _, _ := strings.Cut(string(out), "\x00")
	origin, err := config.ParseRemote(first)
	if err != nil {
		return &InterlockFailure{
			Interlock: freezeRemoteMismatch,
			Fact: "the vault's " + config.RemoteName + " is a URL obsync cannot read, and obsync " +
				"was given " + r.configuredRemote.String(),
			Remedy: "check `git remote -v` in the vault against OBSYNC_REPO. obsync does not " +
				"repeat the URL it could not read, because an operator may have put a token in " +
				"one" + SelfClearing,
		}, nil
	}
	if origin == r.configuredRemote {
		return nil, nil
	}
	return &InterlockFailure{
		Interlock: freezeRemoteMismatch,
		Fact: "the vault's " + config.RemoteName + " is " + origin.String() + " and obsync was " +
			"given " + r.configuredRemote.String(),
		Remedy: "point one of them at the other yourself — OBSYNC_REPO, or `git remote set-url` in " +
			"the vault. obsync never re-points a remote itself, because adopting a new one " +
			"silently is an entire vault landing somewhere nobody chose. Only the host and the " +
			"path are compared, so swapping https for ssh against the same repository is not " +
			"this" + SelfClearing,
	}, nil
}

// configKeyUnset is the status `git config --get` gives for a key that is not
// set, as against 128 for a repository it could not read. Documented in
// git-config(1), which is what makes it a status obsync may branch on.
const configKeyUnset = 1

// vaultUsable is gate 1: the vault path exists, is a directory, and is writable
// by obsync's UID. It is process lifetime, asked once by bootstrap, and it is
// the one refusal where obsync cannot write an attention note either — so the
// log is the only channel there is (§7).
//
// Writability is asked of the kernel rather than read off a mode bit, because a
// mode bit is not the answer: the vault may be on a read-only mount, under an
// ACL, or owned by the UID ignis chowned it to rather than the one the compose
// file gave obsync. This is the check that makes §8's decision to leave UID/GID
// to documentation safe — a wrong UID becomes a full freeze with a named cause
// rather than silent corruption.
//
// It writes nothing to find out. Creating a probe file to test a directory
// would be obsync writing outside its owned paths, in the one place it is least
// entitled to: a directory it has just been told it may not be in.
func vaultUsable(path string) *InterlockFailure {
	refuse := func(fact string) *InterlockFailure {
		return &InterlockFailure{
			Interlock: freezeVaultUnusable,
			Fact:      fact,
			Remedy: "check the mount and the `user:` the container runs as — obsync runs as the UID " +
				"it was given and never changes it, so it has to be the UID that owns the vault. " +
				"This is the one refusal obsync cannot write an attention note about, because it " +
				"cannot write in the vault at all" + SelfClearing,
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return refuse("the vault at " + path + " cannot be read: " + err.Error())
	}
	if !info.IsDir() {
		return refuse("the vault at " + path + " is not a directory")
	}
	// W_OK to create a file in it, X_OK to reach what is already there: a
	// directory obsync may write in and may not enter is not a vault it can
	// sync either.
	if err := unix.Access(path, unix.W_OK|unix.X_OK); err != nil {
		return refuse("obsync's own UID cannot write in the vault at " + path + ": " + err.Error())
	}
	return nil
}

// ErrIndexLocked is somebody else's git holding `.git/index.lock` — a plugin, a
// human at a terminal, or a backup tool driving git in the vault. It is an
// aborted run (§7): this pass gives up, nothing is reported above debug, and
// the next tick retries.
//
// obsync racing its own previous run cannot produce this: one serialized loop
// with one run in flight means the only lock obsync ever meets is somebody
// else's. So the fact is conclusive from the file alone, and nothing here reads
// a word of what git would have said about it — which matters, because the
// failure it prevents is `git add` exiting with git's everything-code, the one
// status that can never classify anything.
var ErrIndexLocked = errors.New("another git holds the vault's index lock")

// IndexLocked reports the lost race before obsync starts one it cannot finish.
//
// It is asked rather than inferred, and asked first: the alternative is
// discovering it at the `git add`, which stages nothing and fails the whole
// commit with an exit status obsync may not branch on. `GIT_OPTIONAL_LOCKS=0`
// keeps `git status` out of the fight (§1), so this is the only thing between a
// third writer's git and a run reported as a failure it is not.
func (r *Repo) IndexLocked() error {
	_, err := os.Lstat(filepath.Join(r.gitDir, "index.lock"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrIndexLocked, filepath.Join(r.gitDir, "index.lock"))
}
