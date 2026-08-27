// Package loop is obsync's sync loop: the single serialized process that
// reconciles the vault with the remote, and the one place a sync run happens.
//
// Only one sync run is ever in flight, and that is structural rather than
// enforced — there is one goroutine, it performs a run, and it does not look at
// its next wake-up until that run is over. No mutex, no queue, and nothing to
// go wrong under load.
//
// What a run does in this build: check the interlocks, ask git what changed,
// take out the paths it refuses to commit and the ones still being written,
// commit the rest as one commit, fetch, classify, fast-forward what is only
// behind, merge what has genuinely diverged out of tree so that both sides
// survive unless a ceiling says a human should look first, check that the vault
// holds the tree it just applied, and push (#24, #27, #28, #29, #30, #31, #32,
// #33).
//
// The interlocks come first and everything after them is a thing obsync does to
// a vault they said it may (§7). What a failure then *means* is tier.go: three
// tiers, a closed list, and the one place a run's outcome is reported.
//
// When it turns is cadence.go: the quiet window, the max-wait cap, the jittered
// tick and the network backoff, none of which is a knob (#25).
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/git"
	"github.com/andyroberts2/obsync/internal/vault"
)

// Loop is the sync loop. New builds one; Run turns it until its context is
// done.
type Loop struct {
	config config.Config
	log    *slog.Logger
	clock  clock.Clock

	// wakes is the watcher's channel, and the watcher's whole contribution: it
	// says that something happened, never what (§2). A nil channel is a loop
	// no watcher wakes, which is what obsync is until #39 — and, since the tick
	// exists, that is tick-only mode rather than a loop nothing wakes at all.
	// It is also what this field becomes when a watcher that did exist goes
	// away, so the two arrivals at tick-only mode are the same state.
	wakes <-chan struct{}

	// cadence is when this loop wakes and whether the run it wakes for may
	// commit. It is state rather than timers: the loop asks it what is due and
	// waits for that on the injected clock.
	cadence cadence

	// The network half's own state, and none of it gates the local half.
	// backoff is the current wait, retryNetworkAt the moment it expires, and
	// lastPush what the push floor is measured from.
	backoff        time.Duration
	retryNetworkAt time.Time
	lastPush       time.Time

	// frozen is the full freeze obsync is in, or empty, and networkFrozen the
	// network freeze. The first has twelve causes — the nine gates and the
	// vault sentinel (§7), a remote holding refs but not the tracked branch,
	// and HEAD moving off the tracked branch (§3) — and the second four: an
	// upstream rewrite, and each of §4's three ways a merge stops rather than
	// being improvised into a commit. The thirteenth cause of a full freeze is
	// write-verify failing (#33), which is gate 9's own freeze arriving from
	// the run that established it rather than from the ref. The one §7 still
	// names and this build does not have is the local failure streak reaching
	// five (#34).
	//
	// They are two fields rather than one tier because they are two live
	// states rather than one classification: a vault can be in both at once,
	// and §7's answer to that is that full wins rather than that the network
	// one stops being true. What the *tier* decides is what a failure means
	// and what is said about it, which is tier.go.
	//
	// A full freeze stops obsync touching the repo at all, so it gates the
	// local half as well as the network one; a network freeze leaves the local
	// half committing, because the vault is sound and only its relationship to
	// the remote is not (§7). Both are re-evaluated at the top of every run, so
	// that repairing the cause releases obsync with no restart.
	frozen        string
	networkFrozen string

	// repo is the vault's repository once obsync has bootstrapped into it, and
	// it carries the tracked branch, resolved on the first run that reached the
	// vault and fixed for the process lifetime (§3).
	repo *git.Repo

	// refused is every path obsync is currently declining to commit, and why.
	// It exists so the WARN fires once per path on transition rather than once
	// per tick for as long as a 200MB video sits in the vault (§5, §9) — the
	// standing signal is the attention note (#38), which is derived from live
	// state and needs no memory at all.
	//
	// It is in-memory and process-lifetime only. A restart re-announces what
	// is still refused, which is the right way round for a warning that is not
	// a gate: the operator who restarted is the one looking.
	refused map[string]string

	// remoteInStep is whether the vault and the remote have ever held the same
	// tip since obsync started, and churnUntracked whether the one-shot that
	// untracks the churn subset has run (§5).
	//
	// The order is the decision. Untracking is a structural commit against a
	// tip obsync has just confirmed, rather than against whatever the vault
	// happened to hold at bootstrap: doing it against a stale tip risks doing
	// it twice, and while the remote is unreachable obsync should not make a
	// commit it cannot push.
	remoteInStep   bool
	churnUntracked bool

	// unsettled is every path obsync is currently leaving out of the commit
	// because it is still being written, when each first looked that way, and
	// which of them obsync has said so about (§6).
	//
	// In-memory and process-lifetime only, deliberately: a restart restarts the
	// clock, which is acceptable for a warning that is not a gate.
	unsettled map[string]unsettledPath
}

// unsettledPath is one path the settle guard is excluding: when obsync first
// saw it move, and whether it has stopped looking transient loudly enough to
// have been said.
type unsettledPath struct {
	since time.Time
	said  bool
}

func New(cfg config.Config, log *slog.Logger, clk clock.Clock, wakes <-chan struct{}) *Loop {
	return &Loop{config: cfg, log: log, clock: clk, wakes: wakes}
}

// Run turns the loop until ctx is done, and returns once the run in flight has
// finished.
//
// Startup runs the loop immediately and then falls into the ordinary cadence
// (§2): the reason the tree is dirty at startup is that obsync was not
// watching, so there is nothing an init phase would do differently and there
// are no init-phase knobs.
//
// A context that is done means SIGTERM, and SIGTERM means refuse to start a new
// run and finish the current one (§1) — including the startup run, which is a
// new run like any other, so a container stopped seconds after it started
// commits nothing. A run already under way is not interrupted: nothing below
// this point can be cancelled except the one thing that can be waiting on the
// outside world, which is a network git, and that is cut short at the shutdown
// deadline rather than left to its own 120s.
func (l *Loop) Run(ctx context.Context) {
	for ctx.Err() == nil {
		l.syncRun(ctx)

		if !l.waitForNextRun(ctx) {
			return
		}
	}
}

// waitForNextRun blocks until the next sync run is due, and reports false when
// obsync is stopping instead.
//
// One timer, not two: the tick, the quiet window and the max-wait cap are three
// deadlines and one wait, because every wake-up runs the whole loop (§2). A
// wake-up from the watcher does not start a run — it moves the moment one is
// due, which is what the quiet window is.
//
// A wake-up and a SIGTERM arriving together leave two cases of the select ready
// and select picks between them at random, which is why the refusal is asked at
// the top of Run rather than here: whichever case wins, the next run does not
// start.
//
// Only a SIGTERM ends this wait. A burst of wake-ups cannot starve the run they
// are deferring, because what is due is recomputed at the top of every turn: as
// soon as the max-wait cap is in the past this returns, whichever case the
// select would have picked.
func (l *Loop) waitForNextRun(ctx context.Context) bool {
	for {
		now := l.clock.Now()
		due := l.cadence.nextRun()
		if !now.Before(due) {
			return true
		}

		// One deadline per turn of this wait, taken out fresh, because a
		// wake-up moves what is due: the first wake-up of a burst brings the
		// next run forward to the quiet window, and every wake-up after it
		// pushes that window later. The deadline obsync is waiting on is
		// therefore always the one it just computed, with nothing to keep in
		// step with anything else.
		expiry := l.clock.After(due.Sub(now))

		select {
		case <-ctx.Done():
			return false
		case _, open := <-l.wakes:
			if !open {
				// The watcher has gone: it tore its watches down on ENOSPC
				// (§1), or it stopped for a reason of its own. That is
				// tick-only mode, not a reason to stop syncing — the tick is
				// what obsync already runs on when no watcher exists at all,
				// and every run asks git what changed, so what obsync commits
				// is the same and only the latency degrades.
				//
				// A nil channel is how that mode is expressed, here as in
				// main: a receive on one blocks forever, so this case is gone
				// rather than permanently ready. Returning instead would exit
				// the process on a signal that is not a sync failure, which is
				// silent non-backup announced by nothing (§2, §7). The WARN
				// naming the sysctl is the watcher's to log (#39).
				l.wakes = nil
				l.log.Debug("the watcher has gone; obsync is in tick-only mode")
				continue
			}
			l.cadence.woke(l.clock.Now())
		case <-expiry:
		}
	}
}

// Close releases what the loop holds. It is safe on a loop that never reached a
// vault.
func (l *Loop) Close() error {
	if l.repo == nil {
		return nil
	}
	return l.repo.Close()
}

// syncRun is one sync run, and the only thing that reports its failure.
//
// obsync never exits on a sync failure: exiting means restarting, which does
// not fix a bad token, discards backoff state, and turns a diagnosable stuck
// state into a crash loop (§2). So a failed run is logged and the loop keeps
// turning, and the next wake-up starts fresh.
func (l *Loop) syncRun(ctx context.Context) {
	// The quiet window decides whether this run may commit, not whether it
	// happens. Asked once, before the run, so that a run which takes a while
	// is judged on the vault it started against.
	committing := l.cadence.mayCommit(l.clock.Now())
	defer func() { l.cadence.ran(l.clock.Now(), committing) }()

	bootstrapped, err := l.bootstrap(ctx)
	if err != nil {
		// Gates 1, 2, 6 and 8 are what bootstrap decides, and each is a full
		// freeze named after the fact behind it: said once on entry, said once
		// when it clears, and re-established from scratch by every run for as
		// long as obsync has no repository (§7, §9).
		var failing *git.InterlockFailure
		if errors.As(err, &failing) {
			l.report(l.freeze(failing.Interlock, failing.Fact, failing.Remedy))
			return
		}
		// Everything else bootstrap can fail at is a clone that did not
		// happen, and it is reported on every run — once a tick for as long as
		// the remote cannot be reached. The hourly repeat that turns that into
		// one line an hour is §9's, and #37's: unlike the network half below,
		// which the backoff already quiets, this has no wait of its own to
		// hide behind.
		l.log.Error("obsync cannot sync the vault it was pointed at", "problem", err,
			"vault_path", l.config.VaultPath)
		return
	}
	// A bootstrap that *ran* and got through is gates 1, 2, 6 and 8 all
	// holding, so the freeze one of them was holding clears here rather than a
	// run later — and only those four, because nothing has yet looked at the
	// interlocks the run below re-checks.
	//
	// Only a bootstrap that ran, because two of those four names are also
	// per-run interlocks: gate 2 is `.git` still being there and gate 6 is the
	// tracked branch still naming a commit, and Refusing below enters a freeze
	// under each of those same names. A bootstrap that returned early because
	// obsync already has a repository has established nothing since the last
	// run, so clearing on it would announce a freeze cleared and re-enter it
	// one line later, once a tick, for as long as the cause stands — which is
	// both the noise §9's "state exit is said once" forbids and a log telling
	// an operator obsync recovered when it did not. Those two clear where they
	// are re-checked, in InterlockFreezes below.
	if bootstrapped {
		l.interlocksHold(git.BootstrapFreezes)
	}
	if err := l.perform(ctx, committing); err != nil {
		l.report(err)
	}
}

// bootstrap is the one decision obsync makes about the vault before it syncs
// it: clone into an empty directory, attach to a repo, refuse anything else
// (§3, gate 2). It also resolves the tracked branch, which is then fixed for
// the process lifetime.
//
// It is retried on every wake-up until it succeeds, which is the shape every
// refusal in this design has: the cause is repaired — the remote gains a vault
// to clone, the stray folder is emptied, the human checks their branch out —
// and obsync recovers on its own, with no restart (§7).
//
// It reports whether this run is the one that did it, because that is the run
// on which the interlocks bootstrap answers are established. Every run after it
// establishes nothing, and a fact nothing looked at is not a fact that cleared.
func (l *Loop) bootstrap(ctx context.Context) (bool, error) {
	if l.repo != nil {
		return false, nil
	}

	repo, err := git.Bootstrap(ctx, l.config, l.log, l.clock)
	if err != nil {
		return false, err
	}
	l.repo = repo
	return true, nil
}

// stillWithheld reports whether the remote still holds refs but not the tracked
// branch, which is the fact behind the one full freeze this build enters from
// the network half.
//
// Re-checking it is one read-only look at the remote per run — the same shape
// as the probe a damage freeze self-clears by, and consistent with what a full
// freeze means, because it touches nothing (§7). A remote obsync cannot reach
// answers nothing, and obsync stays frozen: the freeze clears on a fact, never
// on a failure to establish one.
func (l *Loop) stillWithheld(ctx context.Context) bool {
	if l.frozen != freezeNoUpstreamCounterpart {
		return false
	}

	withheld, err := l.repo.RemoteHoldsRefsButNotTrackedBranch(ctx)
	if err != nil || withheld {
		return true
	}
	l.thawed(freezeNoUpstreamCounterpart)
	return false
}

// stillRewritten reports whether the remote's history is still the rewritten
// one obsync stopped syncing with, and clears the network freeze when it is
// not.
//
// The re-check asks the remote, because one of the freeze's two repairs is a
// fact only the remote holds — the human putting the history they meant back on
// it, which is the second thing obsync's own remedy tells them to do. It asks
// in the one way that cannot overwrite obsync's record of what it last saw the
// remote hold; git.UpstreamRewritten is where that is argued.
//
// A remote obsync cannot reach answers nothing, and obsync stays frozen: the
// freeze clears on a fact, never on a failure to establish one. The probe is a
// network command like any other, so a failure backs the network half off and
// the ordinary tick retries it.
//
// It is asked of this one freeze by name rather than of any network freeze,
// and that is the whole of the difference between the two shapes a network
// freeze has. This one has to be re-checked *before* a reconcile, because an
// ordinary fetch would overwrite the record the question is asked against;
// §4's three merge freezes are re-checked *by* a reconcile, since their cause
// is not a fact obsync can look up but what the next merge comes out as
// (networkThawed).
func (l *Loop) stillRewritten(ctx context.Context, now time.Time) (bool, error) {
	if l.networkFrozen != freezeUpstreamRewrite {
		return false, nil
	}

	rewritten, err := l.repo.UpstreamRewritten(ctx)
	if err != nil {
		l.backOff(now)
		return true, err
	}
	l.networkSucceeded()
	if rewritten {
		return true, nil
	}
	cleared := l.networkFrozen
	l.networkFrozen = ""
	l.log.Info("the freeze cleared and obsync is syncing with the remote again", "freeze", cleared,
		"branch", l.repo.TrackedBranch())
	return false, nil
}

// freeze enters a full freeze, and says so once. State entry and state exit
// each log exactly one line (§9); the hourly repeat that keeps a broken obsync
// from going quiet in between is #37's.
//
// One full freeze is held at a time, and which one is the *current* fact rather
// than the first one ever seen. They stop obsync doing the same nothing, so a
// second freeze arriving changes no behaviour — but it changes what an operator
// is looking at, and that is the whole of what a freeze is for. Holding the
// first name would leave obsync naming a fact the human has already repaired,
// silently, for as long as a second one stood: they would do exactly what the
// log asked and be told nothing, which is the failure §7's self-clearing rule
// exists to make impossible.
//
// So the guard is the name, which is the same guard networkFreeze has always
// had: a freeze obsync is already in is not re-announced, and a different fact
// is. The ordering that matters — full over network, and the first *failing*
// interlock within a run — is in the order these are asked (#32).
func (l *Loop) freeze(name, fact, remedy string) error {
	if l.frozen == name {
		return errFullFrozen
	}
	l.frozen = name
	l.log.Error("obsync is frozen and is touching nothing until this is repaired", "freeze", name,
		"fact", fact, "remedy", remedy)
	return errFullFrozen
}

// networkFreeze stops the network half and leaves the local one committing: the
// vault is sound, and its relationship to the remote is not (§7).
func (l *Loop) networkFreeze(name, fact, remedy string) error {
	if l.networkFrozen == name {
		return errNetworkFrozen
	}
	l.networkFrozen = name
	l.log.Error("obsync has stopped syncing with the remote until this is repaired", "freeze", name,
		"fact", fact, "remedy", remedy)
	return errNetworkFrozen
}

// interlocksHold clears the freeze an interlock was holding, on a run that
// found every one of them holding.
//
// It is asked of the whole set rather than of the one that fired, because the
// set is re-checked as a set: Refusing answers with nothing only when every
// interlock holds, so which one put obsync here is not a question that has to
// survive to the moment it is released. thawed is a no-op on a freeze obsync is
// not in, so the freezes that are not interlocks — HEAD moving off the tracked
// branch, and the remote withholding the tracked branch — are left where their
// own checks clear them.
func (l *Loop) interlocksHold(names []string) {
	for _, name := range names {
		l.thawed(name)
	}
}

// thawed clears the named full freeze if it is the one obsync is in, and says
// so once.
func (l *Loop) thawed(name string) {
	if l.frozen != name {
		return
	}
	l.frozen = ""
	l.log.Info("the freeze cleared and obsync is syncing again", "freeze", name,
		"branch", l.repo.TrackedBranch())
}

// networkThawed clears a network freeze a run has just disproved by doing the
// thing the freeze stopped, and says so once (§9).
//
// It is the shape the merge freezes need and the shape thawed cannot give
// them: their cause is not a fact obsync can re-check on its own, it is what
// the next merge of the two histories comes out as. So the freeze is entered by
// computing a merge and left by computing one that is fine — a human who
// settles the conflict, shrinks the file, or resolves the storm on either side
// gets obsync back on the next tick, with no restart (§7).
//
// The upstream-rewrite freeze never reaches here: it is re-checked before a
// reconcile happens at all, by a probe that cannot overwrite obsync's record of
// what the remote last held, and it stops the run when it holds.
func (l *Loop) networkThawed() {
	if l.networkFrozen == "" {
		return
	}
	cleared := l.networkFrozen
	l.networkFrozen = ""
	l.log.Info("the freeze cleared and obsync is syncing with the remote again", "freeze", cleared,
		"branch", l.repo.TrackedBranch())
}

const (
	// freezeNoUpstreamCounterpart is §3's classification row for a tracked
	// branch the remote does not hold: obsync creates it only on a remote with
	// no refs at all, and freezes on any other.
	freezeNoUpstreamCounterpart = "no upstream counterpart"

	// freezeHeadOffTrackedBranch is HEAD having moved off the branch bootstrap
	// resolved. Committing would put the vault's changes on a branch nobody
	// chose, and obsync never checks a branch out to put that right (§3).
	freezeHeadOffTrackedBranch = "head off the tracked branch"

	// freezeUpstreamRewrite is the remote's history having been rewritten under
	// the tip obsync last saw. Merging would resurrect what the rewrite
	// removed and pushing would restore it, so obsync does neither (§3).
	freezeUpstreamRewrite = "upstream rewrite"

	// The merge outcomes obsync stops the network half for rather than
	// improvising into a commit (§4, #31). Each is a fact about *this* merge
	// rather than about the vault, so each is re-established from scratch by
	// every run that reaches a divergence: the freeze is entered by computing
	// the merge and cleared by computing it again and finding it fine.
	//
	// freezeConflictOutsideTheTable is a conflict kind §4's closed table has no
	// row for. Every row of that table is a rule about which side's bytes
	// survive, and a kind with no row is one where obsync does not know the
	// answer — including where git has an answer of its own, which is the case
	// this reaches most often.
	freezeConflictOutsideTheTable = "conflict outside the table"

	// freezeConflictStorm is more conflicted paths in one merge than a human
	// can be asked to read. Past that count keeping both sides stops being a
	// kindness: the cause is nearly always structural, and it deserves human
	// eyes before it is baked into a commit.
	freezeConflictStorm = "conflict storm"

	// freezeMergedTreeOverTheCeiling is a clean auto-merge blob over the size
	// ceiling — the one blob a merge can invent, and so the only route through
	// the merge path to bytes the remote has never accepted.
	freezeMergedTreeOverTheCeiling = "merged tree over the size ceiling"
)

// perform is the body of a sync run: what changed, one commit, one push.
//
// The two halves fail independently. The local half cannot fail for network
// reasons, so a remote that is unreachable, rejecting or backing off leaves
// obsync a local autocommitter that catches up; only the network half waits
// (§2).
func (l *Loop) perform(ctx context.Context, committing bool) error {
	// The interlocks go first, and everything below them is a thing obsync
	// does to a vault they said it may (§7). Gates 2-7 and 9 and the vault
	// sentinel are re-checked here on every run, which is what makes a frozen
	// obsync keep ticking and doing nothing else — and what makes repairing
	// the cause release it within a tick, with no restart.
	failing, err := l.repo.Refusing()
	if err != nil {
		return err
	}
	if failing != nil {
		return l.freeze(failing.Interlock, failing.Fact, failing.Remedy)
	}

	// The index lock is asked next: it is not a gate — nothing is wrong with
	// the vault — but a run that cannot stage is one that gives up before it
	// changes anything, and asking here is what keeps that an aborted run
	// rather than a `git add` failing with git's everything-code (§7).
	if err := l.repo.IndexLocked(); err != nil {
		return err
	}

	// HEAD is asked before anything is committed, because a commit is what
	// this would get wrong: the tracked branch is fixed at bootstrap (§3), so
	// a run that committed here would put the vault's changes on a branch
	// nobody chose while the push sent the one obsync resolved. Checking the
	// branch back out is the human's to do — obsync never runs git checkout
	// after bootstrap, because that rewrites files they have open — and asking
	// again every run is what makes their doing it enough (§7).
	head, err := l.repo.HeadBranch()
	if err != nil {
		// Gate 3, and the last of the interlocks re-checked at the top of a
		// run: HEAD is not on a branch at all. It is asked here rather than in
		// Refusing because the branch it answers with is the thing the next
		// question is about, and one symbolic-ref answers both.
		var detached *git.InterlockFailure
		if errors.As(err, &detached) {
			return l.freeze(detached.Interlock, detached.Fact, detached.Remedy)
		}
		return err
	}
	if head != l.repo.TrackedBranch() {
		return l.freeze(freezeHeadOffTrackedBranch,
			"the vault's HEAD is on "+head+" and obsync tracks "+l.repo.TrackedBranch(),
			"check "+l.repo.TrackedBranch()+" back out in the vault; obsync never checks a branch "+
				"out itself, because that would rewrite files you have open"+git.SelfClearing)
	}
	l.thawed(freezeHeadOffTrackedBranch)
	l.interlocksHold(git.InterlockFreezes)

	if l.stillWithheld(ctx) {
		return errFullFrozen
	}

	if committing {
		if err := l.localHalf(); err != nil {
			// A local half that failed stops the whole run rather than pushing
			// on with the network one: an aborted run is a pass, not a half
			// (§7). Which tier it was is report's, from the closed table.
			return err
		}
	}
	return l.networkHalf(ctx)
}

// untrackChurnSubset is the one-shot that takes the workspace churn and OS
// cruft out of a vault whose history already carries them (§5), and reports
// whether it made this run's commit.
//
// Ignore rules only ever affect untracked paths, so a vault that has committed
// its workspace file once churns forever no matter what the floor says. The
// remedy is `git rm --cached`, once, files left on disk — and its cost is
// stated rather than hidden: the commit deletes those paths from every other
// clone on its next pull. For a workspace file that is the point, and it is why
// the floor stays narrow enough that this is a fair trade.
//
// It runs at the top of the local half rather than at bootstrap, and only after
// a run that left the vault and the remote holding the same tip. Untracking
// against a stale tip risks doing it twice, and while the remote is unreachable
// obsync should not be making structural commits it cannot push.
//
// The untracking is its own commit and the run stops there, which keeps §2's
// one-commit-per-run true and keeps this commit legible: a human reading git
// log finds one loudly-messaged commit that did exactly one thing, rather than
// a day's notes with an untracking folded into them. The next run commits the
// notes, one tick later, once ever.
//
// A failure leaves the one-shot undone, so the next run tries again. The one
// way it can fail that is not a broken repo is a human's own staged work in the
// index, which git refuses to discard — and it clears the moment obsync commits
// that work like any other change.
func (l *Loop) untrackChurnSubset() (bool, error) {
	if l.churnUntracked || !l.remoteInStep {
		return false, nil
	}

	churn, err := l.repo.TrackedChurnSubset()
	if err != nil {
		return false, err
	}
	if len(churn) == 0 {
		l.churnUntracked = true
		return false, nil
	}

	if err := l.repo.Untrack(churn); err != nil {
		return false, err
	}
	// This commit records the index like any other, so it passes the refusal
	// layer like any other: a human who staged a credential before the one-shot
	// fired would otherwise have it carried out by the one commit in obsync
	// that is not built from the committable set. One diff-index, once ever.
	if _, err := l.stagedWithoutRefused(); err != nil {
		return false, err
	}
	if err := l.repo.Commit(untrackMessage(churn)); err != nil {
		return false, err
	}
	l.churnUntracked = true

	// WARN rather than the INFO a run that committed gets, because the news is
	// not that obsync committed: it is that this commit reaches every other
	// clone of the repo and takes those paths out of it too (§9's advisory
	// row). It is true, it needs no action, and an operator should not have to
	// find out from a diff.
	l.log.Warn("obsync stopped tracking the workspace and cruft files its ignore floor covers, in "+
		"one commit; every byte is still on disk, and pulling this commit removes those paths from "+
		"your other clones too",
		"paths", len(churn), "subject", untrackSubject(churn))
	return true, nil
}

// reportRefusals says what obsync is refusing to commit, once per path on
// transition (§5, §9).
//
// Once, because a refused path stays refused: a 200MB attachment is reported by
// git as changed on every run for as long as it sits in the vault, and a WARN a
// tick is how a signal becomes noise. Forgetting a path that stopped being
// refused is deliberate rather than an omission — the same file arriving again
// is news again — and the transition out is silent, because a file that went
// back to syncing is a run that changed something and says so where every other
// run does.
func (l *Loop) reportRefusals(refused []vault.Refusal) {
	if l.refused == nil {
		l.refused = map[string]string{}
	}
	still := make(map[string]bool, len(refused))
	for _, refusal := range refused {
		still[refusal.Path] = true
		if _, known := l.refused[refusal.Path]; known {
			continue
		}
		l.refused[refusal.Path] = refusal.Reason
		l.log.Warn("obsync is not committing this path, and the rest of the vault keeps syncing; "+
			"the remote holds the last version that passed and your vault holds a newer one",
			"path", refusal.Path, "reason", refusal.Reason)
	}
	for path := range l.refused {
		if !still[path] {
			delete(l.refused, path)
		}
	}
}

// reportConflictCopies says what the keep-both rule kept, once per copy as it
// is written (§9's WARN row).
//
// WARN rather than INFO because it is true, self-healing and advisory: nothing
// is broken, nothing is lost, and there is one thing for the human to do at
// their own pace. It is said once because a copy is written once — the standing
// signal is the attention note's second section (#38), which is derived from
// the vault by the filename pattern rather than from anything obsync remembers.
//
// A conflict is never an unhealthy check either: under the keep-both rule a
// conflict is normal operation, and reserving unhealthy for freezes keeps that
// signal meaning "a human must act" (§9).
func (l *Loop) reportConflictCopies(copies []git.ConflictCopy) {
	for _, written := range copies {
		l.log.Warn("both sides changed this note, so obsync kept both: your version is untouched at "+
			"its own path and the remote's is beside it, byte for byte. Edit the two together and "+
			"delete the copy — that is the whole of it, and the ordinary loop commits it",
			"path", written.Of, "conflict_copy", written.Path)
	}
}

// errStageVerify is a path that moved on disk between the settle guard's second
// sample and obsync's `git add` — the third writer, whose writes no sampling
// window can anticipate (§6).
//
// It is an aborted run (§7): this pass gives up, nothing is reported above
// debug, and the next tick retries against a vault that has stopped moving. The
// index is left holding what the add captured, which the next run re-stages from
// disk the moment the working tree and the index differ.
var errStageVerify = errors.New("a path moved on disk while obsync was staging it")

// reportUnsettled says which paths have stayed unsettled long enough to stop
// looking transient, once each (§6, §9's WARN row).
//
// Transient exclusion is silent, because it is latency rather than news: a note
// somebody is typing into is excluded from run after run and arrives the moment
// they pause, and a WARN a tick for that is how a signal becomes noise.
// Persistent exclusion is news — a note a plugin rewrites every 500ms never
// reaches the remote at all, and a silently-skipped file that stays silent for
// ever is the failure this reports.
//
// How long is unsettledForLong, and the reason for the number lives there
// rather than being restated here. The tracking is in-memory and
// process-lifetime only, deliberately: a restart restarts the clock, which is
// acceptable for a warning that is not a gate. A path that settles is
// forgotten, so the same file going hot again is news again — the same shape
// reportRefusals has, and the transition out is silent for the same reason.
//
// Every exclusion is said at debug as it happens, which is §9's DEBUG row —
// per-path settle-guard outcomes — and is the only place "why is this note not
// in the commit" has an answer before the ten minutes are up.
//
// The standing signal is the attention note's fourth section (#38), which is
// derived from this state rather than accumulated.
func (l *Loop) reportUnsettled(unsettled []string, now time.Time) {
	if l.unsettled == nil {
		l.unsettled = map[string]unsettledPath{}
	}
	still := make(map[string]bool, len(unsettled))
	for _, path := range unsettled {
		still[path] = true
		record, known := l.unsettled[path]
		if !known {
			record = unsettledPath{since: now}
		}
		l.log.Debug("the settle guard left a path out of this commit because it moved on disk while "+
			"obsync was looking at it", "path", path, "unsettled_for", now.Sub(record.since))
		if !record.said && now.Sub(record.since) >= unsettledForLong {
			record.said = true
			l.log.Warn("this path has moved on disk every time obsync has looked at it for a long "+
				"time now, so it is not reaching the remote; the rest of your vault is syncing "+
				"normally, and it will commit on its own as soon as whatever is writing it stops",
				"path", path, "unsettled_for", now.Sub(record.since))
		}
		l.unsettled[path] = record
	}
	for path := range l.unsettled {
		if !still[path] {
			delete(l.unsettled, path)
		}
	}
}

// localHalf is status and commit: the part of a run that touches only the vault
// and its .git.
func (l *Loop) localHalf() error {
	if untracked, err := l.untrackChurnSubset(); err != nil || untracked {
		return err
	}

	changed, err := l.repo.Changed()
	if err != nil {
		return err
	}

	// The committable set is what a run would actually commit. The ignore floor
	// has already come out of it, by git rather than here — status does not
	// report an untracked path the exclude file covers — which is what leaves
	// the vault's own .gitignore able to overrule the floor (§5). What obsync
	// subtracts itself is the refusal layer and the settle guard (§6).
	committable := vault.CommittableSet(l.clock, l.config.VaultPath, changedPaths(changed), l.config.SizeCeiling)
	l.reportRefusals(committable.Refused)
	l.reportUnsettled(committable.Unsettled, l.clock.Now())

	// A tree holding nothing but refused and unsettled paths is quiet: no
	// commit, no push, and no repeated warning (§5).
	if len(committable.Paths) == 0 {
		return nil
	}

	// What the add is given is narrower than what the commit will carry: a
	// path whose change the index already holds in full has nothing in the
	// working tree to stage, and naming one that git also ignores is fatal to
	// the whole add rather than to that pathspec (git.ChangedPath). So the
	// committable set decides whether to commit and the working tree decides
	// what to add, and a run with nothing to add still commits what a human
	// staged.
	adding := toStage(changed, committable)
	if err := l.repo.Stage(adding); err != nil {
		return err
	}
	// Stage-verify: nothing may have moved on disk while obsync was staging it
	// (§6). The paths were verified stable across the settle interval a moment
	// ago, which is what makes aborting safe here — the third writer is the one
	// whose writes no sampling window can anticipate, and the index now holds
	// bytes obsync cannot vouch for.
	if moved := committable.StageVerify(adding); moved != "" {
		return fmt.Errorf("%w: %q", errStageVerify, moved)
	}
	// What the index holds is what the commit will carry, and it is not always
	// what status reported: an edit that puts a file back the way HEAD has it
	// is a change to the tree and no change to the commit. Committing anyway
	// would put an empty commit in a human's history.
	staged, err := l.stagedWithoutRefused()
	if err != nil {
		return err
	}
	if len(staged) == 0 {
		return nil
	}
	// One commit per run, covering everything git reported. Per-file commits
	// isolate the wrong unit — a rename is a delete plus an add, and a note and
	// its pasted image are one act (§2). On a bulk import that means each
	// capped commit is an honest partial snapshot converging on the right tree,
	// which is why there is no burst mode to write.
	if err := l.repo.Commit(commitMessage(staged)); err != nil {
		return err
	}
	l.log.Info("committed", "paths", len(staged), "subject", subject(staged))
	return nil
}

// stagedWithoutRefused is what the index would commit, with any refused path
// somebody else staged taken back out of it first (§5). Both of obsync's
// commits go through it, and that is the point of it being one function.
//
// The subtraction the committable set makes decides what obsync *stages*, and
// that is not the whole of what a commit records: `git add` is not the only way
// a path reaches the index, and `git commit` records the index. A human's own
// `git add -A` in their vault — muscle memory, and what every plugin that
// drives git for them does — would otherwise put a refused path in obsync's
// commit and push it, in the same run whose WARN said obsync was not committing
// it. That is the one unrecoverable mistake in this design (§5), so it is
// checked against the list obsync will actually commit rather than against what
// status happened to report.
//
// It costs no extra git in the ordinary case: this is the same diff-index the
// commit message is written from. The unstaging happens only when something
// else staged a refused path, and is index-only.
func (l *Loop) stagedWithoutRefused() ([]git.Change, error) {
	staged, err := l.repo.Staged()
	if err != nil {
		return nil, err
	}
	_, refused := l.refusedAmong(staged)
	if len(refused) == 0 {
		return staged, nil
	}
	if err := l.repo.Unstage(refused); err != nil {
		return nil, err
	}

	staged, err = l.repo.Staged()
	if err != nil {
		return nil, err
	}
	// Fail closed rather than commit one anyway. A refusal never freezes the
	// loop, but a run that cannot keep this particular promise is a run that
	// does not commit: the vault is intact either way, and the mistake this
	// prevents is the one no later commit can undo.
	if reason, still := l.refusedAmong(staged); len(still) > 0 {
		return nil, fmt.Errorf("obsync could not take %q back out of the index and will not commit "+
			"a refused path: %s", still[0], reason)
	}
	return staged, nil
}

// refusedAmong is the paths of these that obsync refuses to commit, and the
// reason the first of them is refused.
func (l *Loop) refusedAmong(staged []git.Change) (string, []string) {
	paths := make([]string, len(staged))
	for i, change := range staged {
		paths[i] = change.Path
	}
	refusals := vault.Refusals(l.config.VaultPath, paths, l.config.SizeCeiling)
	if len(refusals) == 0 {
		return "", nil
	}
	refused := make([]string, len(refusals))
	for i, refusal := range refusals {
		refused[i] = refusal.Path
	}
	return refusals[0].Reason, refused
}

// changedPaths is what git reported as changed, by name: the dirty set the
// committable set is computed from.
func changedPaths(changed []git.ChangedPath) []string {
	paths := make([]string, len(changed))
	for i, change := range changed {
		paths[i] = change.Path
	}
	return paths
}

// toStage is the committable set narrowed to what `git add` may actually be
// given. Two separate facts narrow it, and getting either wrong is fatal to the
// whole commit rather than to the one path (git.ChangedPath).
//
// The committable set still decides whether to commit; this decides only what
// is named on the add, so a run with nothing to add still commits what a human
// staged.
func toStage(changed []git.ChangedPath, committable vault.Committable) []string {
	keep := make(map[string]bool, len(committable.Paths))
	for _, path := range committable.Paths {
		keep[path] = true
	}
	paths := make([]string, 0, len(committable.Paths))
	for _, change := range changed {
		// Nothing in the working tree to stage: the index already holds this
		// change in full, and if git also ignores the path, naming it is fatal.
		if !change.InWorkingTree || !keep[change.Path] {
			continue
		}
		// And nothing on disk to match: a path git has no index entry for is
		// matched by the file alone, so once it is gone the pathspec matches
		// nothing and git refuses the whole add with it. That is the other end
		// of the window the settle guard narrows (§6) — the guard's second
		// sample already looked, so this is its answer rather than a stat.
		// Naming it instead would spend a failed run and an ERROR on a note
		// that is simply not there any more, which §7 tiers as an aborted run.
		// A *tracked* path that was deleted is not skipped:
		// the deletion is the change, and the index entry is what the pathspec
		// matches.
		if change.Untracked && !committable.OnDisk(change.Path) {
			continue
		}
		paths = append(paths, change.Path)
	}
	return paths
}

// networkHalf is the part of a run that talks to the remote: fetch, classify,
// reconcile, push (§2).
//
// It is the only half that waits: a failure here backs off from 60s to 15m and
// is retried by the ordinary tick, so the backoff is a rate limit on the
// existing loop rather than a schedule of its own. Any success resets it.
func (l *Loop) networkHalf(ctx context.Context) error {
	now := l.clock.Now()
	if now.Before(l.retryNetworkAt) {
		return nil
	}
	frozen, err := l.stillRewritten(ctx, now)
	if err != nil {
		return err
	}
	if frozen {
		return errNetworkFrozen
	}

	reconciled, err := l.repo.Reconcile(ctx)
	var failing *git.InterlockFailure
	switch {
	case errors.As(err, &failing):
		// Write-verify: obsync applied a tree it had computed and the vault
		// does not hold it, so it can no longer trust its own view of the vault
		// (§7, #33). It is a full freeze rather than a network one — obsync
		// stops committing too, because the thing it would be committing is a
		// tree it cannot account for — and it is the one freeze a restart
		// cannot clear, because the fact it is keyed on is the ref write-verify
		// has already written.
		//
		// The backoff is deliberately left where it is: nothing here is a fact
		// about the remote, and the remote answered every question this run
		// asked it.
		//
		// It is matched by type rather than by name, which is the same way the
		// tier table sorts an interlock: this is where the network half meets
		// the full-freeze tier at all, and §7 puts more than write-verify in it
		// — a merge state appearing mid-run and `.git` disappearing are both
		// facts a run can establish after the interlocks were asked.
		return l.freeze(failing.Interlock, failing.Fact, failing.Remedy)
	case errors.Is(err, git.ErrUpstreamRewrite):
		return l.networkFreeze(freezeUpstreamRewrite,
			"the remote no longer holds the commit obsync last saw at the tip of "+
				l.repo.TrackedBranch(),
			"decide which history you want: take the remote's with one `git reset --hard origin/"+
				l.repo.TrackedBranch()+"` in the vault, or put the history you meant back on the "+
				"remote. obsync will do neither itself — merging would resurrect what the rewrite "+
				"removed and pushing would restore it"+git.SelfClearing)
	case errors.Is(err, git.ErrConflictOutsideTheTable):
		// §4's fallback, and the reason the table is worth closing: obsync
		// stops rather than guessing at bytes, and stops the smallest part of
		// itself that could publish the guess. The fact is git's own — the
		// conflict kind is a machine field and the paths are git's spelling —
		// so nothing here reads a word written for a human.
		return l.networkFreeze(freezeConflictOutsideTheTable, err.Error(),
			"look at what the two sides did to those paths and settle it yourself, in the vault or "+
				"on the remote. obsync keeps both sides of every conflict it has a rule for and "+
				"improvises none of the rest, so this one is yours. Your vault is still being "+
				"committed locally meanwhile"+git.SelfClearing)
	case errors.Is(err, git.ErrConflictStorm):
		return l.networkFreeze(freezeConflictStorm, err.Error(),
			"look at what changed on both sides before it is baked into a commit — a conflict this "+
				"large is nearly always one act rather than fifty, a folder moved or a bulk edit "+
				"or a vault restored over itself. Settle it in the vault or on the remote; your "+
				"vault is still being committed locally meanwhile"+git.SelfClearing)
	case errors.Is(err, git.ErrMergedTreeOverTheCeiling):
		return l.networkFreeze(freezeMergedTreeOverTheCeiling, err.Error(),
			"raise OBSYNC_SIZE_CEILING to what your remote accepts, or make that file smaller in "+
				"the vault. It is what the two versions of it merge to, so it is bytes neither "+
				"side holds yet and nothing of yours is waiting on it. Your vault is still being "+
				"committed locally meanwhile"+git.SelfClearing)
	case errors.Is(err, git.ErrNoUpstreamCounterpart):
		// §3's sharpest rule, and the one place obsync may create a ref on the
		// remote. The remote does not hold the tracked branch, so it is asked
		// what it does hold before any bytes go: a remote with no refs at all
		// is a brand-new one and the push creates the tracked branch, and a
		// remote holding anything else does not get a branch nobody agreed on
		// — the name came from local HEAD, and the push would succeed. The
		// cost to an operator who genuinely wants a dedicated branch is one
		// deliberate manual `git push -u`.
		withheld, err := l.repo.RemoteHoldsRefsButNotTrackedBranch(ctx)
		if err != nil {
			l.backOff(now)
			return err
		}
		if withheld {
			return l.freeze(freezeNoUpstreamCounterpart,
				"the remote holds refs but not "+l.repo.TrackedBranch(),
				"create it on the remote yourself with one `git push -u origin "+l.repo.TrackedBranch()+
					"`, or point obsync at the branch you meant"+git.SelfClearing)
		}
		l.networkSucceeded()
		return l.push(ctx, now)
	case errors.Is(err, git.ErrUnsettledOnWriteSide), errors.Is(err, git.ErrVaultWrittenMidRun):
		// The two aborted runs the incoming change can produce: a path it
		// overwrites is still being written, or the vault is being written
		// where it lands. Nothing is applied at all — all-or-nothing, because
		// a partial apply is not a valid state the way a partial commit is
		// (§6) — and neither is a fact about the remote, so the backoff is
		// left where it is and the next tick tries again.
		return err
	case err != nil:
		l.backOff(now)
		return err
	}
	l.networkSucceeded()
	l.networkThawed()

	switch reconciled.State {
	case git.Ahead:
		return l.push(ctx, now)
	case git.Equal:
		l.remoteInStep = true
	case git.Behind:
		// The fast-forward already happened, so the vault now holds the
		// remote's tip.
		l.remoteInStep = true
		// A run that changed something says so, and this changed the vault
		// (§9): someone else's edit is now in front of the human.
		l.log.Info("the vault caught up with the remote", "branch", l.repo.TrackedBranch())
	case git.Diverged:
		// Both sides moved, which is the designed-for case rather than an
		// anomaly. The merge already happened, out of tree and in one commit
		// carrying whatever conflict copies the keep-both rule needed (§4), so
		// what is left is to publish it.
		l.reportConflictCopies(reconciled.ConflictCopies)
		l.log.Info("the vault and the remote had both changed and were merged, keeping both sides",
			"branch", l.repo.TrackedBranch(), "conflict_copies", len(reconciled.ConflictCopies))
		return l.push(ctx, now)
	}
	// Equal, or reconciled: a run that changed nothing says nothing, because
	// docker logs --since 1h being empty is a designed signal (§9).
	return nil
}

// push sends the tracked branch, and is the only thing in a sync run that
// writes to the remote.
func (l *Loop) push(ctx context.Context, now time.Time) error {
	// The push floor, off by default: a lower bound between pushes, checked
	// here on the loop obsync already turns rather than kept by a second timer.
	if !l.lastPush.IsZero() && now.Sub(l.lastPush) < pushFloor {
		return nil
	}

	if err := l.repo.Push(ctx); err != nil {
		l.backOff(now)
		return err
	}
	l.lastPush = now
	l.remoteInStep = true
	l.networkSucceeded()
	l.log.Info("pushed", "branch", l.repo.TrackedBranch())
	return nil
}

// backOff doubles the network half's wait, from 60s and never past 15m.
func (l *Loop) backOff(now time.Time) {
	if l.backoff == 0 {
		l.backoff = backoffFloor
	} else {
		l.backoff = min(2*l.backoff, backoffLongest)
	}
	l.retryNetworkAt = now.Add(l.backoff)
}

// networkSucceeded resets the backoff to its floor. Any success does it: the
// remote came back, and the next failure is a fresh one rather than the
// continuation of an old one.
func (l *Loop) networkSucceeded() {
	l.backoff, l.retryNetworkAt = 0, time.Time{}
}
