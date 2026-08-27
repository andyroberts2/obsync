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

// A damaged local repository (§7, #34).
//
// The disk rots an object, or an unclean shutdown leaves a zero-length one.
// Three rules cover the whole of what obsync does about it: **find damage by
// working, never by scanning; only persistence may call it permanent; repair
// derived state, never history.**
//
// **No `fsck`, at startup or at any cadence.** It answers the wrong question:
// damage in unreachable history harms nothing, and damage in the objects obsync
// needs announces itself the moment obsync needs them. Every sync run is
// already a proportional integrity check over exactly the right subset, where a
// full scan is O(repo bytes) on every restart — and a clean scan is not a
// promise anyway, because the disk can rot between the check and the command
// that needs the object.
//
// **The exit status can never classify this**, which is why the streak in
// internal/loop exists at all. Measured at both matrix points, 2.38.5 and
// 2.52.0, and the two agree: a corrupt loose object, a zero-length object, a
// missing object and a truncated index all exit **128** — git's everything-code,
// which also covers a bad revision, a locked index and an unreachable remote.
// Meanwhile a zero-length *blob* nothing in the run needs leaves `git status`
// exiting 0, which is the same measurement from the other side: commands fail
// only when they genuinely need the damaged object.

// Probe is the single read-only look at the vault a frozen obsync takes each
// tick, to find out whether the damage is still there (CONTEXT.md).
//
// It exists because the damage freeze is the one honest exception to how every
// other freeze clears: a gate is a cheap conclusive fact about *now* and
// self-clears by being re-checked, and a streak is a fact about *history*, so
// this one self-clears by **retrying the work**. The probe stands in for the
// gate that cannot exist (§7).
//
// It is `git status`, and that choice is the whole of what makes a probe
// consistent with what a full freeze means: it is the command a run's local
// half already fails at, it reads exactly the objects a run depends on, and it
// writes nothing — `GIT_OPTIONAL_LOCKS=0` (§1) forbids even the refreshed index
// git would otherwise write back. One command per tick, and obsync does nothing
// else at all while the freeze stands.
//
// It is asked for the same status the local half asks for, rather than a second
// spelling of one command, so that the probe can never succeed against a
// question the run does not ask. What it answers with is discarded: the fact
// wanted here is whether git could read the repository, not what changed.
func (r *Repo) Probe() error {
	_, err := r.Changed()
	return err
}

// RebuildIndex discards the vault's `.git/index` and builds it again from HEAD.
// It is the only repair obsync performs on a repository, and the invariant it
// is drawn from is the reason it is allowed at all:
//
//	obsync may discard derived state; it never discards history.
//
// The index is derived state and the only member of that class: a cache of HEAD
// plus stat data, holding no history, reconstructible from the working tree.
// Commits, blobs and a human's files are not, which is the whole difference
// between this and the re-clone obsync refuses — a re-clone discards exactly the
// unpushed commits obsync exists to have made, and obsync cannot tell whether a
// damaged object is one the remote already holds or one only this disk ever had.
//
// **The cost, stated out loud rather than hidden: this discards a human's
// staged-but-uncommitted work.** It is acceptable and near-invisible, because
// their files are untouched on disk and the next run commits them like any other
// change — but it is a thing obsync throws away, so obsync says so where it does
// it and says so again in the log (internal/loop).
//
// Two commands rather than one, and the second is what keeps the first safe.
// Deleting the file alone would leave git treating the index as **empty**, and
// an empty index reports every tracked path twice: once as a staged deletion
// and once as untracked (measured at both matrix points). The run that followed
// would re-add everything it was allowed to stage — and would carry the staged
// *deletion* of everything it was not, which is any path the settle guard is
// holding out of this commit (§6). That would have obsync publishing the
// deletion of a note somebody was mid-way through typing. `read-tree` puts the
// index back to exactly what the spec calls it, a cache of HEAD, so the only
// thing the rebuild discards is what was staged.
//
// It touches no file in the working tree: `read-tree` without `-u` writes the
// index and nothing else. The stat data is left for the next `git status` to
// fill in, which costs one whole-vault re-read once (docs/research/sizing.md).
//
// A rebuild that fails is not a fact obsync acts on either — a repository whose
// HEAD tree cannot be read is one the run after this will fail at anyway, and
// that run is what escalates. Measured at both points: with a corrupt HEAD tree
// this exits 128 and so does the `git status` that follows it.
func (r *Repo) RebuildIndex() error {
	index := filepath.Join(r.gitDir, "index")
	if err := os.Remove(index); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("discarding %s: %w", index, err)
	}
	if _, err := r.run(invocation{dir: r.vault, args: []string{"read-tree", "HEAD"}}); err != nil {
		return fmt.Errorf("rebuilding %s from HEAD: %w", index, err)
	}
	return nil
}

// RestoreIndexIfMissing puts an index back when the repository has none, and
// does nothing at all when it has one.
//
// It exists because a rebuild can leave nothing there: the discard always
// works, and the `read-tree` that follows it does not when the object HEAD
// names is itself the damage — which is the case a damaged repository reaches
// most often. Measured at both matrix points, a missing index is one git reads
// as **empty**, and `git status` against an empty index reports every tracked
// path twice: as a staged deletion, and as untracked.
//
// That is the one state obsync may not do a local half in. It would stage back
// everything it was allowed to stage and carry the staged *deletion* of
// everything it was not — which is any path the settle guard is holding out of
// this commit (§6) — so obsync would publish the deletion of a note somebody was
// in the middle of typing. Asked before every run's halves rather than at the
// one place obsync's own rebuild leaves it, because a third writer's `rm
// .git/index` reaches the same state and deserves the same answer.
//
// It is the same carve-out the rebuild is, in the direction that costs nothing:
// what is written is derived state, and what it is written from is HEAD.
func (r *Repo) RestoreIndexIfMissing() error {
	_, err := os.Stat(filepath.Join(r.gitDir, "index"))
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return r.RebuildIndex()
}

// LooksLike is git's own words *naming* a failure, and it is the one place
// obsync reads prose written for a human at all.
//
// The rule it obeys generalises the auth-string precedent, and the whole of it
// is the second half:
//
//	git's own words may name a failure; only persistence may escalate one.
//
// So nothing here reaches a caller that decides anything. What this returns
// ends up in one place — the sentence obsync writes for a human — and its only
// effect is that the message says *looks like a corrupt object* instead of
// *git failed*. On a git that rewords its errors the message gets vaguer and
// **the behaviour does not move**: the streak counts runs whatever the stated
// reason, and time is the classifier, because damage is permanent and bad luck
// is not (§7).
func LooksLike(err error) string {
	var command *CommandError
	if !errors.As(err, &command) {
		return ""
	}
	said := strings.ToLower(command.Stderr)
	for _, named := range namedByGit {
		if strings.Contains(said, named.said) {
			return named.looksLike
		}
	}
	return ""
}

// namedByGit is the prose obsync recognises, and the words it lends a human for
// it. Every phrase on the left was measured at both matrix points, 2.38.5 and
// 2.52.0, against the damage that produces it:
//
//   - a corrupt loose object — `error: inflate: data stream error (incorrect
//     header check)`, then `error: unable to unpack <oid> header` at 2.52.0 and
//     `fatal: loose object <oid> ... is corrupt` at 2.38.5. The two gits differ
//     one line down, which is exactly why more than one phrase is listed for
//     one kind of damage and why none of them decides anything;
//   - a zero-length object — `error: object file .git/objects/... is empty`,
//     then `error: bad tree object HEAD` at 2.52.0;
//   - a missing object — `fatal: bad object HEAD`;
//   - a truncated or corrupt index — `fatal: .git/index: index file smaller
//     than expected`.
//
// The last three are the network half's, and they are the precedent the rule
// above generalises rather than a second kind of thing: a wrong or expired
// credential is a labelled network-half failure on the ordinary backoff and
// never a freeze (§7, §8), because PATs expire and get rotated and a latched
// auth freeze would take away self-recovery for nothing. Measured at both
// matrix points against the failure that produces each:
//
//   - a credential the remote refused — `fatal: Authentication failed for
//     '<url>'`, over https with a PAT the remote would not take;
//   - no credential at all — `fatal: could not read Username for '<host>':
//     terminal prompts disabled`, which is what GIT_TERMINAL_PROMPT=0 turns an
//     interactive prompt into (§1);
//   - an ssh key the remote refused — `git@host: Permission denied
//     (publickey).`
//
// They share this list rather than having one of their own, because this being
// the one place obsync reads prose is worth more than the tidiness of two
// lists. Each is matched in the spelling that cannot collide with a local
// failure: `permission denied` alone would label an ordinary EACCES on the
// vault as a credential the remote refused, and `(publickey` is what makes it
// ssh's and only ssh's.
//
// The index phrase goes first because it is the one damage the rebuild repairs,
// so a human reading it is being told something they need do nothing about. The
// list is deliberately short: an unrecognised failure says nothing extra, which
// is the honest answer and the one that costs nothing.
var namedByGit = []struct{ said, looksLike string }{
	{"index file", "this looks like a damaged index"},
	{"inflate", "this looks like a corrupt object"},
	{"unable to unpack", "this looks like a corrupt object"},
	{"loose object", "this looks like a corrupt object"},
	{"object file", "this looks like an empty or corrupt object file"},
	{"bad object", "this looks like an object git cannot read"},
	{"bad tree object", "this looks like an object git cannot read"},
	{"authentication failed", "this looks like a credential the remote would not accept"},
	{"could not read username", "this looks like a missing credential"},
	{"permission denied (publickey", "this looks like an ssh key the remote would not accept"},
}

// FreeSpaceIfLow is how much room is left where the repository lives, said only
// when there is almost none, and "" otherwise.
//
// **statfs labels, never gates.** There is no free-space gate and no threshold
// to configure, because disk full is not a corruption mechanism: git writes
// objects, packs and the index through temp-plus-rename, so ENOSPC *aborts
// commands* rather than leaving damage behind. What it does do is make a local
// command fail for a reason a human can fix in five minutes and would otherwise
// spend an evening on, so obsync reads free space when a local command has
// already failed and adds a sentence to what it was going to say anyway. It
// never decides anything, and a failure to read it is not news.
//
// The threshold is the **size ceiling**, which is a number the design already
// has and which the operator has already stated about their own deployment
// (§5): below it obsync cannot be confident that committing one ordinary vault
// file would fit at all, since the ceiling is the largest single file obsync
// will ever hand git. Because this labels rather than gates, the expensive
// direction is silence — so it errs at the generous end, and a true sentence
// about free space beside an unrelated failure costs nothing.
//
// It asks about the filesystem holding the repository rather than the working
// tree, because the failing command was writing an object, a pack or the index.
// For an ordinary vault they are the same filesystem; for a vault whose `.git`
// is a file pointing elsewhere they need not be, and the one that ran out is
// the one worth naming.
func (r *Repo) FreeSpaceIfLow() string {
	var stat unix.Statfs_t
	if err := unix.Statfs(r.gitDir, &stat); err != nil {
		return ""
	}
	// Bavail rather than Bfree: obsync is not root, so the blocks reserved for
	// root are not room it can write into.
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free >= r.sizeCeiling {
		return ""
	}
	return "the filesystem holding the vault's repository has " + config.FormatSize(free) +
		" free, which is less than the largest single file obsync would commit"
}
