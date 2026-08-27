package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/vault"
)

// SyncState is how the tracked branch stands against its upstream counterpart
// at classify time (§3). The four of them are a closed set, and the one thing
// that is not among them is the counterpart's *absence*, which is not a sync
// state at all: without it there is nothing to be equal to or ahead of, and
// what obsync does about that is §3's first push rather than a reconciliation.
type SyncState string

const (
	Equal    SyncState = "equal"
	Ahead    SyncState = "ahead"
	Behind   SyncState = "behind"
	Diverged SyncState = "diverged"
)

// Reconciliation is what one pass of Reconcile found and did.
type Reconciliation struct {
	// State is how the tracked branch stood against its upstream counterpart.
	State SyncState

	// ConflictCopies is what the keep-both rule wrote into this run's merge
	// commit, in git's own order (§4). It is empty on every state but a
	// divergence, and empty on most of those too: a clean line-level merge
	// keeps both sides without a copy, which is the common case rather than
	// the exception.
	ConflictCopies []ConflictCopy
}

// Reconcile brings the vault as far into step with the remote as it safely
// can, and answers with how the two stood when it looked.
//
// It fetches, checks that the remote's history is still the history obsync last
// saw, and classifies. A branch that is only behind is fast-forwarded here and
// one that has genuinely diverged is merged here, because both are the whole of
// what those answers need doing to them; what is left for the caller is the
// push (§3).
//
// The merge is a merge and never a rebase, and the working tree is why: a
// rebase walks the vault through one checkout per replayed commit, with a human
// watching their notes revert to older versions and ignis writing into the
// intermediate tree. A fast-forward and an out-of-tree merge are each one
// working-tree transition, and HEAD never leaves the branch.
func (r *Repo) Reconcile(ctx context.Context) (Reconciliation, error) {
	before, err := r.remoteTip()
	if err != nil {
		return Reconciliation{}, err
	}
	if before == "" {
		// obsync has no ref for this branch, so it has never seen the remote
		// hold it, and asking for it by name would fail rather than answer:
		// measured on both matrix points, a fetch of a ref the remote does not
		// have exits 128 — git's everything-code, and the same status an
		// unreachable remote gives. So the remote is asked by status first,
		// and a remote that does hold the branch is fetched from normally.
		holds, err := r.remoteMatches(ctx, "--heads", "refs/heads/"+r.branch)
		if err != nil {
			return Reconciliation{}, err
		}
		if !holds {
			return Reconciliation{}, ErrNoUpstreamCounterpart
		}
	}

	if err := r.fetch(ctx); err != nil {
		return Reconciliation{}, err
	}

	tip, err := r.remoteTip()
	if err != nil {
		return Reconciliation{}, err
	}
	if tip == "" {
		return Reconciliation{}, ErrNoUpstreamCounterpart
	}

	// What obsync last saw the remote hold. Ordinarily it is what was read a
	// moment ago, before this run's fetch; when that fetch moved nothing it is
	// one update further back, which is the value git keeps in the ref's own
	// reflog. That second reading is what makes a rewrite obsync detected in an
	// earlier process still detectable in this one, where the ref itself holds
	// the rewritten tip and no longer remembers what it replaced.
	lastSeen := before
	if lastSeen == tip {
		if lastSeen, err = r.previousRemoteTip(); err != nil {
			return Reconciliation{}, err
		}
	}
	rewritten, err := r.upstreamRewritten(lastSeen, tip)
	if err != nil {
		return Reconciliation{}, err
	}
	if rewritten {
		return Reconciliation{}, ErrUpstreamRewrite
	}

	state, err := r.classify()
	if err != nil {
		return Reconciliation{}, err
	}
	switch state {
	case Behind:
		return Reconciliation{State: Behind}, r.fastForward(tip)
	case Diverged:
		// Both sides moved, which is the designed-for case rather than an
		// anomaly, and §4's out-of-tree merge is the whole of the answer to it.
		// Fast-forward-only-and-freeze was rejected on frequency alone.
		copies, err := r.merge(tip)
		return Reconciliation{State: Diverged, ConflictCopies: copies}, err
	default:
		return Reconciliation{State: state}, nil
	}
}

// UpstreamRewritten re-asks the question a network freeze on an upstream
// rewrite was entered on, so that repairing it releases obsync with no restart
// (§7, without exception).
//
// It asks the remote, and it has to: the freeze has two repairs and only one of
// them is visible in the vault. A human who takes the remote's history moves
// obsync's branch, which the ancestry below sees locally. A human who does the
// other thing obsync's own remedy names — puts the history they meant back on
// the remote — changes nothing obsync's refs can see, and a freeze that reads
// only local refs would hold for ever against the operator doing exactly what
// it asked.
//
// It asks with --refmap=, and that is the whole of why asking is safe. What
// obsync last saw the remote hold is the remote-tracking ref's own reflog, so
// an ordinary fetch would overwrite the record and the freeze would clear the
// moment the rewritten remote gained one more commit — which is precisely when
// a merge would resurrect everything the rewrite removed. --refmap= discards
// the vault's configured refspecs and uses only the one named here, which has
// no destination, so the answer arrives in FETCH_HEAD and the ref and its
// reflog are left exactly where they were.
func (r *Repo) UpstreamRewritten(ctx context.Context) (bool, error) {
	lastSeen, err := r.previousRemoteTip()
	if err != nil {
		return false, err
	}
	if lastSeen == "" {
		return false, nil
	}
	tip, err := r.remoteTipUnrecorded(ctx)
	if err != nil || tip == "" {
		return false, err
	}
	return r.upstreamRewritten(lastSeen, tip)
}

// upstreamRewritten reports whether the remote's history has been rewritten
// underneath obsync: the tip obsync last saw the remote hold is gone from it,
// and obsync's own branch still carries what went with it.
//
// The first half is §3's detection. The second half is what the freeze is
// *for*, and it is what lets the freeze clear: the commits a rewrite removed
// are the ones reachable from the tip obsync last saw and not from the tip the
// remote holds now, and they are dangerous only while obsync's branch still
// holds them — a merge would resurrect them and a push would restore them. So
// the ancestry is asked of where obsync's branch and that tip come together,
// which is the tip itself in the ordinary case and moves once a human has
// decided which history wins.
func (r *Repo) upstreamRewritten(lastSeen, tip string) (bool, error) {
	if lastSeen == "" {
		return false, nil
	}
	shared, err := r.mergeBase(lastSeen, "HEAD")
	if err != nil || shared == "" {
		// No common history at all: obsync's branch holds nothing the rewrite
		// could have taken away, so there is nothing to resurrect.
		return false, err
	}
	inRemoteHistory, err := r.isAncestor(shared, tip)
	return !inRemoteHistory, err
}

// classify is §3's classification, and it is one command: the counts on each
// side of the symmetric difference between the upstream counterpart and HEAD.
//
// HEAD rather than the branch by name, because HEAD is what a fast-forward
// moves and what a push sends. The two are the same thing here — the loop
// refuses to act at all when HEAD is not on the tracked branch — and naming the
// one the rest of the run acts on is what keeps them from drifting apart.
func (r *Repo) classify() (SyncState, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-list", "--count", "--left-right", r.upstreamRef() + "...HEAD"},
	})
	if err != nil {
		return "", err
	}
	// Two counts and a tab: no path is in this output, which is the whole
	// reason it can be read as fields at all.
	counts := strings.Fields(string(out))
	if len(counts) != 2 {
		return "", fmt.Errorf("git rev-list --count --left-right answered %q, which is not two counts", out)
	}
	behind, err := strconv.Atoi(counts[0])
	if err != nil {
		return "", fmt.Errorf("git rev-list --count --left-right answered %q, which is not two counts: %w", out, err)
	}
	ahead, err := strconv.Atoi(counts[1])
	if err != nil {
		return "", fmt.Errorf("git rev-list --count --left-right answered %q, which is not two counts: %w", out, err)
	}

	switch {
	case behind == 0 && ahead == 0:
		return Equal, nil
	case behind == 0:
		return Ahead, nil
	case ahead == 0:
		return Behind, nil
	default:
		return Diverged, nil
	}
}

// fetch brings the remote's tracked branch down, and nothing else.
//
// The refspec is written out in full rather than left to the vault's own
// config: obsync's refspec is one branch in each direction (§3), and the config
// is the human's file — a vault an operator made with `git remote add` carries
// git's own +refs/heads/*, which would bring down every branch they have. Tags
// are refused for the same reason, and submodules are not followed at all: a
// submodule's own remote is one obsync was never pointed at.
//
// The + is the ordinary force git applies to a remote-tracking ref, and it is
// not a force-push: nothing about it writes to the remote. It is what lets
// obsync *see* a rewritten remote rather than merely failing to fetch it —
// measured on both matrix points, an unforced refspec leaves the ref where it
// was and exits 1, which is a status obsync could not tell from anything else.
func (r *Repo) fetch(ctx context.Context) error {
	_, err := r.run(invocation{
		dir: r.vault,
		args: []string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules",
			config.RemoteName, "+refs/heads/" + r.branch + ":" + r.upstreamRef()},
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	})
	return unanswered(err)
}

// ErrRemoteUnreachable is the remote not answering a question obsync asked it.
//
// It is an aborted run (§7): this pass gives up, nothing is reported above
// debug, and the ordinary backoff retries it. That tier is not an inference
// from what git said — git exits 128 for a host that is down, a repository that
// is not there and a credential that was refused alike, and the exit status can
// never classify any of them. It is a fact about *which command* failed: a
// fetch returns no verdict about anything, so a fetch that failed is obsync
// having been told nothing, which is precisely the state waiting repairs.
//
// The same is true of every other read-only ask obsync makes of the remote, so
// they all carry this and `unanswered` is the one place that is decided. The
// ones that are not the run's own fetch matter more rather than less: they are
// the probes a *frozen* obsync re-asks on every tick, so a remote that is
// merely down would otherwise put an ERROR a tick under a freeze that is
// already correctly announced — which is exactly the noise the abort tier
// exists to prevent.
//
// A *push* that fails is deliberately not this. A push carries a verdict from
// the party whose opinion is the whole question, and telling a lost race from a
// rejection is what §7's push disposition table is for (#35, unbuilt) — so
// until that lands a failed push stays a reported failure rather than being
// quietly sorted into the tier that says nothing.
var ErrRemoteUnreachable = errors.New("the remote did not answer")

// unanswered labels a failed read-only network git as the remote not having
// answered, and it is the only place that judgement is made.
//
// The class is stated by what the command is rather than by what it said: a
// fetch and an `ls-remote` each ask the remote a question and carry back no
// verdict about anything, so one that failed leaves obsync having been told
// nothing. A conclusive answer the command *does* carry — `ls-remote
// --exit-code`'s "no matching refs" — is read before this is reached, and never
// reaches it.
func unanswered(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRemoteUnreachable, err)
}

// fastForward moves the vault onto the remote's tip, and is the one thing in a
// sync run that writes files a human owns.
//
// It refuses to do that over a note being written. The paths a fast-forward
// touches are the ones it would overwrite, and any of them that the vault has
// changed since HEAD is a run obsync abandons rather than a merge it forces:
// there is no second commit inside a run and no stashing (§3). The scope is
// deliberate — checking the whole tree instead would let one continuously
// edited note block every incoming change indefinitely, and git applies exactly
// this scope itself when it refuses.
func (r *Repo) fastForward(tip string) error {
	touched, err := r.pathsBetween("HEAD", tip)
	if err != nil {
		return err
	}
	if err := r.refuseWhileTheVaultIsWritten(touched); err != nil {
		return err
	}

	// The tip by object name rather than by ref, so that what is applied is
	// exactly what was classified and checked for a rewrite a moment ago.
	//
	// --ff-only, so this can only ever be the transition git can make without
	// writing a commit: obsync's merge of two histories that really did diverge
	// is computed out of tree (§4, #30) and never by this command.
	_, err = r.run(invocation{
		dir:  r.vault,
		args: []string{"merge", "--ff-only", "--quiet", tip},
	})
	return err
}

// refuseWhileTheVaultIsWritten abandons the run rather than letting an incoming
// change overwrite a file something is writing, and is the guard both of
// obsync's applies take — the fast-forward, and §4's out-of-tree merge.
//
// Two facts, and the second is the one no sampling window can anticipate. A
// path the incoming change touches that the vault has changed since HEAD is a
// run obsync abandons rather than a write it forces: there is no second commit
// inside a run and no stashing (§3), and git applies exactly this scope itself
// when it refuses. Then the settle guard, over the same scope and immediately
// before the apply, catches a write that started after that status.
//
// The scope is deliberate and load-bearing. Checking the whole tree instead
// would let one continuously edited note block every incoming change
// indefinitely, on a vault that is never quiet.
//
// The apply is all-or-nothing — there is no skipping a path here, because a
// partial apply leaves the vault holding a tree obsync never computed — so one
// unsettled path abandons the run, and recomputing costs nothing (§6).
func (r *Repo) refuseWhileTheVaultIsWritten(touched []string) error {
	overwrites := make(map[string]bool, len(touched))
	for _, path := range touched {
		overwrites[path] = true
	}
	changed, err := r.Changed()
	if err != nil {
		return err
	}
	for _, change := range changed {
		if overwrites[change.Path] {
			return fmt.Errorf("%w: the vault holds a change to %q, which the incoming change "+
				"overwrites", ErrVaultWrittenMidRun, change.Path)
		}
	}

	if moving := vault.Unsettled(r.clock, r.vault, touched); moving != "" {
		return fmt.Errorf("%w: %q is still being written and the incoming change overwrites it",
			ErrUnsettledOnWriteSide, moving)
	}
	return nil
}

// pathsBetween is every path whose content differs between two commits, in
// git's own order — which is what makes the path an abandoned run names the
// same one on every run rather than whichever a map handed back first.
func (r *Repo) pathsBetween(from, to string) ([]string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"diff-tree", "-r", "-z", "--name-only", from, to},
	})
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// remoteTip is the commit the upstream counterpart names, or "" when obsync has
// no ref for it — a branch it has never pushed and never seen the remote hold.
//
// for-each-ref rather than rev-parse: an absent ref is not a failure here, and
// for-each-ref says so by printing nothing and exiting 0.
func (r *Repo) remoteTip() (string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"for-each-ref", "--format=%(objectname)", r.upstreamRef()},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// previousRemoteTip is the commit the upstream counterpart named before
// whatever last moved it, which git keeps in that ref's reflog in a non-bare
// repo. It is what obsync last saw the remote hold, across a restart.
//
// An answer that is not there is not a failure: a ref that has never moved has
// no earlier value, and neither has one whose reflog begins with its creation.
// Measured on both matrix points, those exit 1 and 128 respectively with
// nothing on stdout and, under --quiet, nothing on stderr — so what is read
// here is the value, and its absence means obsync has no earlier tip recorded.
// The in-run reading above does not depend on this, so a repo whose reflogs are
// off still has its rewrites detected in the run they happen in.
func (r *Repo) previousRemoteTip() (string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", r.upstreamRef() + "@{1}"},
	})
	var command *CommandError
	if errors.As(err, &command) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// remoteTipUnrecorded is what the remote holds at the tracked branch now, asked
// without disturbing obsync's record of what it last held.
//
// The refspec names no destination and --refmap= discards the vault's
// configured ones, so nothing writes a remote-tracking ref and the answer is
// FETCH_HEAD. Without --refmap= git updates that ref opportunistically whenever
// the command-line refspec matches a configured one, which is exactly the
// overwrite this must not do — measured on both matrix points, along with the
// objects arriving with it, which is what makes the ancestry answerable
// locally afterwards.
//
// --no-tags and --no-recurse-submodules for the same reasons fetch passes them:
// one branch in each direction, and a submodule's remote is one obsync was
// never pointed at.
func (r *Repo) remoteTipUnrecorded(ctx context.Context) (string, error) {
	if _, err := r.run(invocation{
		dir: r.vault,
		args: []string{"fetch", "--refmap=", "--quiet", "--no-tags", "--no-recurse-submodules",
			config.RemoteName, "refs/heads/" + r.branch},
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	}); err != nil {
		return "", unanswered(err)
	}
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", "FETCH_HEAD"},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mergeBase is the commit where two histories come together, or "" when they
// share none. git-merge-base(1) documents the exit status, which is what makes
// "they share nothing" a fact obsync may act on rather than prose.
func (r *Repo) mergeBase(one, other string) (string, error) {
	out, err := r.run(invocation{dir: r.vault, args: []string{"merge-base", one, other}})
	var command *CommandError
	if errors.As(err, &command) && command.ExitCode == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isAncestor reports whether one commit is reachable from another.
//
// git-merge-base(1) defines the status: 0 for yes, 1 for no, and anything else
// is an error. A closed enum in an exit status, which is why this reads it and
// never git's words.
func (r *Repo) isAncestor(one, other string) (bool, error) {
	_, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"merge-base", "--is-ancestor", one, other},
	})
	var command *CommandError
	if errors.As(err, &command) && command.ExitCode == 1 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// upstreamRef is the tracked branch's own ref on the remote, as the vault knows
// it (§3).
func (r *Repo) upstreamRef() string {
	return "refs/remotes/" + config.RemoteName + "/" + r.branch
}

// ErrUpstreamRewrite is the remote's tip having ceased to be a descendant of
// the tip obsync last saw, with obsync's branch still holding what the rewrite
// removed. It is a network freeze (§7): the vault is sound and its relationship
// to the remote is not.
//
// Left alone it would classify as ordinary divergence, and the merge would
// resurrect every commit the rewrite removed — so obsync would quietly undo a
// deliberate secret purge and push the secret back. Following the remote
// instead, by hard-resetting to the new tip, is the mirror image of
// force-pushing and is refused for the same reason.
var ErrUpstreamRewrite = errors.New("the remote's history has been rewritten under the tip obsync last saw")

// ErrNoUpstreamCounterpart is the tracked branch having no ref on the remote
// and none in the vault: obsync has never pushed it and has never seen the
// remote hold it. It is not a sync state, and what it leads to is §3's first
// push — which may create the branch, or may be the one thing that freezes.
var ErrNoUpstreamCounterpart = errors.New("the remote does not hold the tracked branch")

// ErrVaultWrittenMidRun is the vault having been written at a path an incoming
// change would overwrite, found before obsync applied anything.
//
// It is an aborted run (§7): nothing was decided, nothing is reported above
// debug, and the next wake-up starts fresh — the local half commits the new
// edit and the same incoming commits are reconciled against the new HEAD.
// Committing a second time inside this run is ruled out, and stashing is worse:
// it would revert the working tree to HEAD, so the human's most recent edits
// would vanish from their open vault for the duration.
var ErrVaultWrittenMidRun = errors.New("the vault was written where the incoming change lands")

// ErrUnsettledOnWriteSide is a path the incoming change overwrites that was
// still being written when obsync looked, across the settle interval (§6).
//
// It is an aborted run (§7), and the write side is all-or-nothing: skipping the
// path would leave the vault holding a tree obsync never computed, which
// write-verify then turns into a full freeze, and applying anyway would eat the
// user's keystrokes silently — write-verify would not catch that either,
// because obsync wrote exactly what it intended.
var ErrUnsettledOnWriteSide = errors.New("a path the incoming change overwrites is still being written")
