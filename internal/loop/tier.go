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
// ErrRemoteUnreachable`, `git.ErrNetworkDeadline`, which a push with no verdict
// at all also carries); and a push `rejected` for losing a race
// (`git.ErrLostTheRace`), told from a rejection by the documented porcelain
// enum rather than by an exit status that cannot separate them. `git.
// ErrRemoteFailure` is here for the same reason the unreachable remote is: the
// remote never reported what it did, so nothing was decided.
//
// **Network freeze.** An upstream rewrite; a conflict type outside the closed
// table; a merge over the storm ceiling; a merged-tree blob over the size
// ceiling; and a **remote rejection**, which is the one member that is a
// verdict rather than an inconclusive check — it arrives as `errNetworkFrozen`
// like the others, entered where the verdict was read.
//
// **Full freeze.** Any of the nine gates and the vault sentinel, which arrive
// as a `*git.InterlockFailure` and are matched by type rather than by row —
// there are ten of them, they are one kind of fact, and a table listing them
// twice is a table that can disagree with itself. Write-verify failing arrives
// the same way and is the reason the type match is worth more than a row: it is
// gate 9's own freeze, established by the run that writes the ref and re-read
// by every run afterwards, so the two are one state rather than two rows that
// could disagree. Plus HEAD moving off the tracked branch, a merge state
// appearing mid-run (gate 4), `.git` disappearing (gate 2), the remote holding
// refs but not the tracked branch, and the local failure streak reaching five
// (#34) — which arrives here as `errFullFrozen` like every other freeze,
// because it is entered where its evidence is counted.
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
	{git.ErrLostTheRace, abortedRun},
	{git.ErrRemoteFailure, abortedRun},
	{errNetworkFrozen, networkFreeze},
	{errFullFrozen, fullFreeze},
}

// tierOf sorts a failure into its tier, and answers false for one with no row.
//
// A failure with no row is not a fourth tier and is not silently treated as
// one: it is a git that failed for a reason obsync has no rule for, and it is
// reported as exactly that — a run that failed — rather than being quietly
// sorted into the tier that says nothing.
//
// It is not what decides the local failure streak, and the two are worth
// keeping apart. The streak counts a run whose *local half* failed whatever the
// stated reason (§7), which is a fact about where the failure happened rather
// than about whether this table has a row for it: an `index.lock` somebody else
// holds for five runs running has a row and is still five runs of obsync not
// being able to work in the vault, and a push the remote rejected has a row of
// its own and is not the local half at all. So the streak is counted at the
// sites in perform that are the local half, and this table goes on saying only
// what a failure means.
func tierOf(err error) (tier, bool) {
	var failing *git.InterlockFailure
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
