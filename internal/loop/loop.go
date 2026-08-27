// Package loop is obsync's sync loop: the single serialized process that
// reconciles the vault with the remote, and the one place a sync run happens.
//
// Only one sync run is ever in flight, and that is structural rather than
// enforced — there is one goroutine, it performs a run, and it does not look at
// its next wake-up until that run is over. No mutex, no queue, and nothing to
// go wrong under load.
//
// What a run does in this build: ask git what changed, take out the paths it
// refuses to commit and the ones still being written, commit the rest as one
// commit, fetch, classify, fast-forward what is only behind, and push what is
// only ahead (#24, #27, #28, #29). Everything that will later stand between
// those steps — the gates (#32) and the out-of-tree merge a real divergence
// needs (#30) — is a rule added to a loop that already turns.
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
	// network freeze. This build has two causes for the first — a remote
	// holding refs but not the tracked branch, and HEAD moving off the tracked
	// branch (§3) — and one for the second, an upstream rewrite; the nine gates
	// that will produce the rest are #32's, and so is a tier that is a type
	// rather than two fields.
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

	if err := l.bootstrap(ctx); err != nil {
		// Reported on every run, which means once a tick for as long as the
		// vault cannot be reached or is one obsync refuses. The hourly repeat
		// that turns that into one line an hour is §9's, and #37's: unlike the
		// network half below, which the backoff already quiets, this has no
		// wait of its own to hide behind.
		l.log.Error("obsync cannot sync the vault it was pointed at", "problem", err,
			"vault_path", l.config.VaultPath)
		return
	}
	if err := l.perform(ctx, committing); err != nil {
		if errors.Is(err, git.ErrShutdownDeadline) {
			// Not a failure a human is needed for: obsync was told to stop and
			// the push had not finished. The next start picks the commit up,
			// because the commit is already in the vault.
			l.log.Debug("the sync run was cut short by the shutdown deadline", "problem", err)
			return
		}
		// One line, and no tier: the three tiers are #32's, and until they
		// exist every failure is reported the same way rather than being
		// silently sorted into a category obsync cannot yet act on.
		l.log.Error("the sync run failed", "problem", err)
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
func (l *Loop) bootstrap(ctx context.Context) error {
	if l.repo != nil {
		return nil
	}

	repo, err := git.Bootstrap(ctx, l.config, l.log, l.clock)
	if err != nil {
		return err
	}
	l.repo = repo
	return nil
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
func (l *Loop) stillRewritten(ctx context.Context, now time.Time) (bool, error) {
	if l.networkFrozen == "" {
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
// One full freeze is held at a time and the first fact wins: they stop obsync
// doing the same nothing, so a second one arriving changes no behaviour, and
// re-announcing a freeze obsync is already in would make the log say a state
// changed when it did not. The ordering that matters — full over network — is
// in the order these are asked (#32).
func (l *Loop) freeze(name, fact, remedy string) {
	if l.frozen != "" {
		return
	}
	l.frozen = name
	l.log.Error("obsync is frozen and is touching nothing until this is repaired", "freeze", name,
		"fact", fact, "remedy", remedy)
}

// networkFreeze stops the network half and leaves the local one committing: the
// vault is sound, and its relationship to the remote is not (§7).
func (l *Loop) networkFreeze(name, fact, remedy string) {
	if l.networkFrozen == name {
		return
	}
	l.networkFrozen = name
	l.log.Error("obsync has stopped syncing with the remote until this is repaired", "freeze", name,
		"fact", fact, "remedy", remedy)
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
)

// perform is the body of a sync run: what changed, one commit, one push.
//
// The two halves fail independently. The local half cannot fail for network
// reasons, so a remote that is unreachable, rejecting or backing off leaves
// obsync a local autocommitter that catches up; only the network half waits
// (§2).
func (l *Loop) perform(ctx context.Context, committing bool) error {
	// HEAD is asked before anything is committed, because a commit is what
	// this would get wrong: the tracked branch is fixed at bootstrap (§3), so
	// a run that committed here would put the vault's changes on a branch
	// nobody chose while the push sent the one obsync resolved. Checking the
	// branch back out is the human's to do — obsync never runs git checkout
	// after bootstrap, because that rewrites files they have open — and asking
	// again every run is what makes their doing it enough (§7).
	head, err := l.repo.HeadBranch()
	if err != nil {
		return err
	}
	if head != l.repo.TrackedBranch() {
		l.freeze(freezeHeadOffTrackedBranch,
			"the vault's HEAD is on "+head+" and obsync tracks "+l.repo.TrackedBranch(),
			"check "+l.repo.TrackedBranch()+" back out in the vault; obsync never checks a branch "+
				"out itself, because that would rewrite files you have open. This clears on its own "+
				"once fixed; no restart needed")
		return nil
	}
	l.thawed(freezeHeadOffTrackedBranch)

	if l.stillWithheld(ctx) {
		return nil
	}

	if committing {
		if err := l.localHalf(); err != nil {
			// An aborted run, and the abort tier reports nothing above debug:
			// a path moved while obsync was staging it, so the index holds
			// bytes obsync cannot vouch for and this pass gives up rather than
			// committing them. The whole run gives up rather than pushing on
			// with the network half — an aborted run is a pass, not a half —
			// and the next wake-up starts fresh against a vault that has since
			// stopped moving (§6, §7).
			if errors.Is(err, errStageVerify) {
				l.log.Debug("the sync run was abandoned rather than committing a path that moved "+
					"while obsync was staging it", "problem", err)
				return nil
			}
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
// Ten minutes is 2× the max-wait cap, so a legitimately busy file never trips
// it. The tracking is in-memory and process-lifetime only, deliberately: a
// restart restarts the clock, which is acceptable for a warning that is not a
// gate. A path that settles is forgotten, so the same file going hot again is
// news again — the same shape reportRefusals has, and the transition out is
// silent for the same reason.
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
		if !record.said && now.Sub(record.since) >= unsettledForLong {
			record.said = true
			l.log.Warn("this path has moved on disk every time obsync has looked at it for the last "+
				"ten minutes, so it is not reaching the remote; the rest of your vault is syncing "+
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
	if err := l.repo.Stage(toStage(changed, committable.Paths)); err != nil {
		return err
	}
	// Stage-verify: nothing may have moved on disk while obsync was staging it
	// (§6). The paths were verified stable across the settle interval a moment
	// ago, which is what makes aborting safe here — the third writer is the one
	// whose writes no sampling window can anticipate, and the index now holds
	// bytes obsync cannot vouch for.
	if moved := committable.StageVerify(); moved != "" {
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

// toStage is the committable set narrowed to the paths the working tree holds
// something to stage for, which is what `git add` is given (git.ChangedPath).
func toStage(changed []git.ChangedPath, committable []string) []string {
	keep := make(map[string]bool, len(committable))
	for _, path := range committable {
		keep[path] = true
	}
	paths := make([]string, 0, len(committable))
	for _, change := range changed {
		if change.InWorkingTree && keep[change.Path] {
			paths = append(paths, change.Path)
		}
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
	if err != nil || frozen {
		return err
	}

	state, err := l.repo.Reconcile(ctx)
	switch {
	case errors.Is(err, git.ErrUpstreamRewrite):
		l.networkFreeze(freezeUpstreamRewrite,
			"the remote no longer holds the commit obsync last saw at the tip of "+
				l.repo.TrackedBranch(),
			"decide which history you want: take the remote's with one `git reset --hard origin/"+
				l.repo.TrackedBranch()+"` in the vault, or put the history you meant back on the "+
				"remote. obsync will do neither itself — merging would resurrect what the rewrite "+
				"removed and pushing would restore it. This clears on its own once fixed; no "+
				"restart needed")
		return nil
	case errors.Is(err, git.ErrUnsettledOnWriteSide):
		// The other aborted run the incoming change can produce, and the same
		// tier: a path it overwrites is still being written, so nothing is
		// applied at all. All-or-nothing, because a partial apply is not a
		// valid state the way a partial commit is (§6).
		l.log.Debug("the sync run was abandoned rather than applying over a file still being written",
			"problem", err)
		return nil
	case errors.Is(err, git.ErrVaultWrittenMidRun):
		// An aborted run, and the abort tier reports nothing above debug: the
		// vault is being written where the incoming change lands, which is
		// news about the next few seconds rather than about obsync (§7).
		l.log.Debug("the sync run was abandoned rather than applying over a file being written",
			"problem", err)
		return nil
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
			l.freeze(freezeNoUpstreamCounterpart,
				"the remote holds refs but not "+l.repo.TrackedBranch(),
				"create it on the remote yourself with one `git push -u origin "+l.repo.TrackedBranch()+
					"`, or point obsync at the branch you meant; this clears on its own once fixed, "+
					"no restart needed")
			return nil
		}
		l.networkSucceeded()
		return l.push(ctx, now)
	case err != nil:
		l.backOff(now)
		return err
	}
	l.networkSucceeded()

	switch state {
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
		// anomaly — and the out-of-tree merge that keeps both is #30's. Until
		// it lands obsync holds its commits locally rather than pushing them:
		// the push could only be refused, since every write to the remote is a
		// fast-forward or it does not happen (§3).
		//
		// Not in step, deliberately: the fetch was fine, and obsync still
		// cannot publish, which is exactly the state the churn one-shot waits
		// out rather than making a structural commit into.
		l.log.Debug("the vault and the remote have both changed", "branch", l.repo.TrackedBranch())
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
