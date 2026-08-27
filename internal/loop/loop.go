// Package loop is obsync's sync loop: the single serialized process that
// reconciles the vault with the remote, and the one place a sync run happens.
//
// Only one sync run is ever in flight, and that is structural rather than
// enforced — there is one goroutine, it performs a run, and it does not look at
// its next wake-up until that run is over. No mutex, no queue, and nothing to
// go wrong under load.
//
// What a run does in this build is the tracer bullet of #24: ask git what
// changed, commit it as one commit, and push. Everything that will later stand
// between those steps — the gates (#32), the ignore floor and refused paths
// (#28), the settle guard (#29), classification and the merge (#27, #30) — is a
// rule added to a loop that already turns.
//
// When it turns is cadence.go: the quiet window, the max-wait cap, the jittered
// tick and the network backoff, none of which is a knob (#25).
package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/git"
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

	// frozen is the full freeze obsync is in, or empty. This build has one
	// cause for it — a remote holding refs but not the tracked branch (§3) —
	// and the nine gates that will produce the rest are #32's. A full freeze
	// stops obsync touching the repo at all, so it gates the local half as well
	// as the network one, and it is re-evaluated at the top of every run so
	// that repairing the cause releases obsync with no restart (§7).
	frozen string

	// repo is the vault's repository once obsync has bootstrapped into it, and
	// it carries the tracked branch, resolved on the first run that reached the
	// vault and fixed for the process lifetime (§3).
	repo *git.Repo
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
	if l.stillFrozen(ctx) {
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

// stillFrozen reports whether obsync is in a full freeze, having first
// re-evaluated the fact that put it there.
//
// The fact this build freezes on is a fact about the remote, so re-checking it
// is one read-only look at the remote per run — the same shape as the probe a
// damage freeze self-clears by, and consistent with what a full freeze means,
// because it touches nothing (§7). A remote obsync cannot reach answers
// nothing, and obsync stays frozen: the freeze clears on a fact, never on a
// failure to establish one.
func (l *Loop) stillFrozen(ctx context.Context) bool {
	if l.frozen == "" {
		return false
	}

	withheld, err := l.repo.RemoteHoldsRefsButNotTrackedBranch(ctx)
	if err != nil || withheld {
		return true
	}

	cleared := l.frozen
	l.frozen = ""
	l.log.Info("the freeze cleared and obsync is syncing again", "freeze", cleared,
		"branch", l.repo.TrackedBranch())
	return false
}

// freeze enters a full freeze, and says so once. State entry and state exit
// each log exactly one line (§9); the hourly repeat that keeps a broken obsync
// from going quiet in between is #37's.
//
// One freeze is held at a time, which is all this build can produce. Where two
// are live at once the full freeze wins over the network one, and that ordering
// arrives with the tiers (#32).
func (l *Loop) freeze(name, fact, remedy string) {
	if l.frozen == name {
		return
	}
	l.frozen = name
	l.log.Error("obsync is frozen and is touching nothing until this is repaired", "freeze", name,
		"fact", fact, "remedy", remedy)
}

// freezeNoUpstreamCounterpart is §3's classification row for a tracked branch
// the remote does not hold: obsync creates it only on a remote with no refs at
// all, and freezes on any other.
const freezeNoUpstreamCounterpart = "no upstream counterpart"

// perform is the body of a sync run: what changed, one commit, one push.
//
// The two halves fail independently. The local half cannot fail for network
// reasons, so a remote that is unreachable, rejecting or backing off leaves
// obsync a local autocommitter that catches up; only the network half waits
// (§2).
func (l *Loop) perform(ctx context.Context, committing bool) error {
	if committing {
		if err := l.localHalf(); err != nil {
			return err
		}
	}
	return l.networkHalf(ctx)
}

// localHalf is status and commit: the part of a run that touches only the vault
// and its .git.
func (l *Loop) localHalf() error {
	changed, err := l.repo.Changed()
	if err != nil {
		return err
	}

	// The committable set is what a run would actually stage. Nothing is
	// subtracted from it yet — the ignore floor and refused paths are #28, and
	// unsettled paths #29 — so in this build it is everything git reports.
	committable := changed
	if len(committable) == 0 {
		return nil
	}

	if err := l.repo.Stage(committable); err != nil {
		return err
	}
	// What the index holds is what the commit will carry, and it is not always
	// what status reported: an edit that puts a file back the way HEAD has it
	// is a change to the tree and no change to the commit. Committing anyway
	// would put an empty commit in a human's history.
	staged, err := l.repo.Staged()
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

// networkHalf is the part of a run that talks to the remote.
//
// It is the only half that waits: a failure here backs off from 60s to 15m and
// is retried by the ordinary tick, so the backoff is a rate limit on the
// existing loop rather than a schedule of its own. Any success resets it.
func (l *Loop) networkHalf(ctx context.Context) error {
	now := l.clock.Now()
	if now.Before(l.retryNetworkAt) {
		return nil
	}

	unpushed, knowsCounterpart, err := l.repo.UnpushedCommits()
	if err != nil {
		return err
	}
	if !unpushed {
		// A run that changed nothing says nothing: docker logs --since 1h
		// being empty is a designed signal, not an accident (§9).
		return nil
	}
	// §3's sharpest rule, and the one place obsync may create a ref on the
	// remote. A branch obsync has no remote-tracking ref for is a branch it has
	// never pushed, so the remote is asked what it holds before any bytes go:
	// a remote with no refs at all is a brand-new one and the push creates the
	// tracked branch, and a remote holding anything else does not get a branch
	// nobody agreed on — the name came from local HEAD, and the push would
	// succeed. The cost to an operator who genuinely wants a dedicated branch
	// is one deliberate manual `git push -u`.
	if !knowsCounterpart {
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
	}

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
