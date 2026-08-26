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
package loop

import (
	"context"
	"log/slog"

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
	// nothing wakes, which is what obsync is until the watcher (#39) and the
	// tick (#25) exist to fill it.
	wakes <-chan struct{}

	// repo and branch are resolved on the first run that can reach the vault
	// and are then fixed for the process lifetime, which is what §3 requires
	// of the tracked branch: the branch obsync syncs cannot become a thing a
	// human changes by accident.
	repo   *git.Repo
	branch string
}

func New(cfg config.Config, log *slog.Logger, clk clock.Clock, wakes <-chan struct{}) *Loop {
	return &Loop{config: cfg, log: log, clock: clk, wakes: wakes}
}

// Run turns the loop until ctx is done, and returns once the run in flight has
// finished.
//
// Startup runs the loop immediately and then falls into the ordinary cadence
// (§2): the reason the tree is dirty at startup is that obsync was not
// watching, so there is nothing an init phase would do differently.
//
// A context that is done means SIGTERM, and SIGTERM means refuse to start a new
// run and finish the current one (§1). It cannot interrupt a run: nothing below
// this point takes a cancellable context, so a shutdown can never kill a git
// halfway and manufacture a state no run would have produced.
func (l *Loop) Run(ctx context.Context) {
	l.syncRun()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-l.wakes:
			if !open {
				return
			}
			// A wake-up and a SIGTERM arriving together leaves both cases
			// ready, and select picks between them at random — so "refuse to
			// start a new run" (§1) has to be asked again here rather than
			// left to the select that already answered it.
			if ctx.Err() != nil {
				return
			}
		}
		l.syncRun()
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
func (l *Loop) syncRun() {
	if err := l.attach(); err != nil {
		// Reported every attempt, which is right while a wake-up is the only
		// thing that drives one. The hourly repeat that keeps a broken obsync
		// from filling a log belongs with the rest of §9's cadence (#37).
		l.log.Error("obsync cannot reach the vault it was pointed at", "problem", err,
			"vault_path", l.config.VaultPath)
		return
	}
	if err := l.perform(); err != nil {
		// One line, and no tier: the three tiers are #32's, and until they
		// exist every failure is reported the same way rather than being
		// silently sorted into a category obsync cannot yet act on.
		l.log.Error("the sync run failed", "problem", err)
	}
}

// attach resolves what a run needs and what §3 fixes for the process lifetime.
//
// It retries on every wake-up until it succeeds, which is the shape every
// freeze in this design has: the cause is repaired and obsync recovers on its
// own, with no restart (§7).
func (l *Loop) attach() error {
	if l.repo != nil {
		return nil
	}

	repo, err := git.Attach(l.config, l.log, l.clock)
	if err != nil {
		return err
	}

	// The operator's override wins, and otherwise the branch the vault is
	// already on — the thing that has an opinion about a vault that is already
	// a repo (§3). Resolving from the remote's default belongs to the
	// bootstrap that clones into an empty directory (#26).
	branch := l.config.Branch
	if branch == "" {
		if branch, err = repo.HeadBranch(); err != nil {
			_ = repo.Close()
			return err
		}
	}

	l.repo, l.branch = repo, branch
	return nil
}

// perform is the body of a sync run: what changed, one commit, one push.
func (l *Loop) perform() error {
	changed, err := l.repo.Changed()
	if err != nil {
		return err
	}

	// The committable set is what a run would actually stage. Nothing is
	// subtracted from it yet — the ignore floor and refused paths are #28, and
	// unsettled paths #29 — so in this build it is everything git reports.
	committable := changed

	if len(committable) > 0 {
		if err := l.repo.Stage(committable); err != nil {
			return err
		}
		// What the index holds is what the commit will carry, and it is not
		// always what status reported: an edit that puts a file back the way
		// HEAD has it is a change to the tree and no change to the commit.
		// Committing anyway would put an empty commit in a human's history.
		staged, err := l.repo.Staged()
		if err != nil {
			return err
		}
		if len(staged) > 0 {
			// One commit per run, covering everything git reported. Per-file
			// commits isolate the wrong unit — a rename is a delete plus an
			// add, and a note and its pasted image are one act (§2).
			if err := l.repo.Commit(commitMessage(staged)); err != nil {
				return err
			}
			l.log.Info("committed", "paths", len(staged), "subject", subject(staged))
		}
	}

	// The local half is over, and it cannot fail for network reasons. What
	// follows is the network half, which fails and backs off on its own (§2) —
	// so a dead remote leaves obsync a local autocommitter that catches up.
	unpushed, err := l.repo.HasUnpushedCommits(l.branch)
	if err != nil {
		return err
	}
	if !unpushed {
		// A run that changed nothing says nothing: docker logs --since 1h
		// being empty is a designed signal, not an accident (§9).
		return nil
	}
	if err := l.repo.Push(l.branch); err != nil {
		return err
	}
	l.log.Info("pushed", "branch", l.branch)
	return nil
}
