package loop

import (
	"errors"

	"github.com/andyroberts2/obsync/internal/git"
)

// tier is what a runtime failure means, and §7 closes the list at three.
//
// A condition added later gets a cadence and a health input, not a fourth
// category. That is the whole value of the list being closed: an operator
// learns three behaviours once, and every failure obsync will ever produce is
// one of them.
type tier int

const (
	// abortedRun is this pass giving up; the next tick retries. No state
	// change, and nothing reported above debug — a transient loss is not news,
	// and making it news is how the signal becomes noise.
	abortedRun tier = iota

	// networkFreeze is the vault being sound while its relationship to the
	// remote is not. The local half keeps committing.
	networkFreeze

	// fullFreeze is the repo having stopped making sense. Stop everything.
	fullFreeze
)

// tiers is §7's three tiers as one closed table: every runtime failure obsync
// produces, and the tier it belongs to.
//
// It is a table rather than prose at each call site so that the membership is
// one thing a reader can check against §7, and so that a failure obsync grows
// later cannot be sorted by whichever branch happens to catch it first.
//
// **Aborted run.** `index.lock` held by someone else (`git.ErrIndexLocked`); a
// file vanished between `status` and `add` (answered before the add, in
// `toStage`, and never an error); stage-verify tripping (`errStageVerify`); an
// unsettled path on the write side (`git.ErrUnsettledOnWriteSide`); `reset
// --keep` refusing a dirty path and a dirty tree mid-run (`git.
// ErrVaultWrittenMidRun`, which is obsync's own conclusive question asked ahead
// of the apply); the remote unreachable or timed out (`git.
// ErrRemoteUnreachable`, `git.ErrNetworkDeadline`). The one member not yet
// here is a push `rejected` for losing a race, which needs the porcelain enum
// to be told from a rejection (#35).
//
// **Network freeze.** An upstream rewrite; a conflict type outside the closed
// table; a merge over the storm ceiling; a merged-tree blob over the size
// ceiling. The remaining member is a remote rejection (#35).
//
// **Full freeze.** Any of the nine gates and the vault sentinel, which arrive
// as a `*git.GateFailure` and are matched by type rather than by row — there
// are ten of them, they are one kind of fact, and a table listing them twice is
// a table that can disagree with itself. Plus HEAD moving off the tracked
// branch, a merge state appearing mid-run (gate 4), `.git` disappearing (gate
// 2), and the remote holding refs but not the tracked branch. Write-verify
// failing and the local failure streak reaching five are §7's last two members
// and are #33's and #34's.
var tiers = []struct {
	is   error
	tier tier
}{
	{git.ErrIndexLocked, abortedRun},
	{errStageVerify, abortedRun},
	{git.ErrUnsettledOnWriteSide, abortedRun},
	{git.ErrVaultWrittenMidRun, abortedRun},
	{git.ErrRemoteUnreachable, abortedRun},
	{git.ErrNetworkDeadline, abortedRun},
	{errNetworkFrozen, networkFreeze},
	{errFullFrozen, fullFreeze},
}

// tierOf sorts a failure into its tier, and answers false for one with no row.
//
// A failure with no row is not a fourth tier and is not silently treated as
// one: it is a git that failed for a reason obsync has no rule for, which is
// what the local failure streak counts, and five of them in a row is the full
// freeze §7 already names (#34, unbuilt). Until then it is reported as what it
// is — a run that failed — rather than being quietly sorted into the tier that
// says nothing.
func tierOf(err error) (tier, bool) {
	var failing *git.GateFailure
	if errors.As(err, &failing) {
		return fullFreeze, true
	}
	for _, row := range tiers {
		if errors.Is(err, row.is) {
			return row.tier, true
		}
	}
	return abortedRun, false
}

// errNetworkFrozen and errFullFrozen are a run that stopped because obsync
// entered or is holding a freeze.
//
// A freeze is entered where its fact is established, because the remedy is a
// sentence about that fact and about nothing else — so what travels back up to
// the run is the tier alone, and the reporting has already happened. They exist
// so that every way a run can end has a row in the table above: a freeze
// missing from it would be reported a second time as an ordinary failed run,
// which is the one thing §9's "state entry is said once" forbids.
var (
	errNetworkFrozen = errors.New("obsync has stopped syncing with the remote")
	errFullFrozen    = errors.New("obsync is frozen and is touching nothing")
)

// report says what a failed sync run was, at the level its tier calls for (§9).
//
// It is the only place a sync run's outcome is reported, which is what makes
// the tier table above the thing that decides rather than a description of what
// several call sites happen to do.
func (l *Loop) report(err error) {
	if errors.Is(err, git.ErrShutdownDeadline) {
		// Not a failure a human is needed for, and not a tier: obsync was told
		// to stop and the push had not finished. The next start picks the
		// commit up, because the commit is already in the vault.
		l.log.Debug("the sync run was cut short by the shutdown deadline", "problem", err)
		return
	}
	switch tier, known := tierOf(err); {
	case !known:
		l.log.Error("the sync run failed", "problem", err)
	case tier == abortedRun:
		// The abort tier reports nothing above debug (§7). §9's DEBUG row is
		// where the tier decision itself lives, so it is said here rather than
		// left to be inferred from which line was chosen.
		l.log.Debug("the sync run was abandoned and the next one will try again", "problem", err,
			"tier", "aborted run")
	default:
		// A freeze said what it was when it was entered, and state entry is
		// said exactly once (§9). What is left is the DEBUG row's tier
		// decision.
		l.log.Debug("the sync run stopped in a freeze", "problem", err, "tier", tierName(tier))
	}
}

func tierName(t tier) string {
	switch t {
	case abortedRun:
		return "aborted run"
	case networkFreeze:
		return "network freeze"
	case fullFreeze:
		return "full freeze"
	}
	return ""
}
