package loop

import (
	"time"

	"github.com/andyroberts2/obsync/internal/status"
)

// signal is §9, and it is the whole of what a sync run tells the outside world
// about itself: the status file rewritten, the attention note reconciled
// against what obsync can see, and one line to a human if one is needed and
// none has been said for an hour.
//
// It runs at the end of every wake-up whatever the run turned out to be. A
// parked obsync means PID liveness carries no information at all, so the signal
// has to be manufactured to replace the one a normal daemon gets for free by
// dying — and the manufactured one is this file's timestamp, which is why it is
// refreshed regardless of tier. A run that kept losing to a third writer's
// index.lock is aborting, which is not news, but it is unambiguously alive.
//
// A run with no repository writes nothing, which is not a case to handle: the
// file lives under `.git/obsync/`, so a dropped mount, a directory that is not
// a repository and a container that has not run yet all leave no file to read,
// and all three read as unhealthy with no special case (§9).
func (l *Loop) signal(now time.Time) {
	if l.repo == nil {
		return
	}

	file := l.statusFile(now)
	l.write(file)
	// The third channel, and the only one that goes where this user is already
	// looking (§9, #38). It is written from the same live state the file is,
	// on the same wake-up, so the note in the vault, the verdict `docker ps`
	// acts on and the line in the log cannot describe three different obsyncs.
	l.reconcileAttentionNote(now)

	// The verdict is derived from the same file a subcommand reads, by the same
	// function, so that what obsync repeats to a human and what `obsync
	// healthcheck` answers Docker cannot disagree.
	health := file.Health(now)
	if !health.NeedsHuman {
		// Cleared rather than left, so the next state a human is needed for is
		// announced when it happens rather than an hour after the last one.
		l.saidNeedsHumanAt = time.Time{}
		return
	}
	l.sayNeedsHuman(now, health)
}

// sayNeedsHuman is the one line §9's hourly repeat says, and one spelling of it
// for the two places that reach it: a state derived from the status file, and a
// bootstrap interlock that has no status file to be derived from.
func (l *Loop) sayNeedsHuman(now time.Time, health status.Health) {
	l.needsHuman(now, "obsync needs a human", "state", health.State, "fact", health.Fact,
		"remedy", health.Remedy)
}

// statusFile is what obsync knew about itself at the end of this wake-up.
//
// Facts rather than a conclusion: the verdict is derived from them, here and in
// the subcommands alike, and the two windows it needs travel with them so that
// a reader in another process asks the same question with the same numbers
// rather than holding a second copy of them.
func (l *Loop) statusFile(now time.Time) status.File {
	return status.File{
		WrittenAt:         now,
		Branch:            l.repo.TrackedBranch(),
		FullFreeze:        l.frozen.record(),
		NetworkFreeze:     l.networkFrozen.record(),
		PushAttempted:     l.pushAttempted,
		LastPush:          l.lastPush,
		LastCommit:        l.lastCommit,
		NetworkFailingFor: l.networkFailingFor(now),
		StaleAfter:        stalenessWindow,
		BackoffCeiling:    backoffCeiling,
	}
}

// networkFailingFor is how long the network half has been failing, or zero
// while it is working. Measured here, against obsync's own clock, so that the
// verdict a reader in another process draws from it is a comparison against a
// threshold rather than a subtraction across two clocks (status.File).
func (l *Loop) networkFailingFor(now time.Time) time.Duration {
	if l.networkFailingSince.IsZero() {
		return 0
	}
	return now.Sub(l.networkFailingSince)
}

// write puts the status file where the subcommands read it.
//
// A write that fails is a debug line and nothing more, and that is the design
// working rather than an omission: the consequence is that the file stops being
// refreshed, which is precisely the unhealthy verdict a vault obsync cannot
// write to deserves. Reporting it at ERROR as well would be obsync announcing
// the loss of the channel it announces things through, once a tick.
func (l *Loop) write(file status.File) {
	content, err := file.Encode()
	if err != nil {
		l.log.Debug("obsync could not render its own status file", "problem", err)
		return
	}
	if err := l.repo.WriteStatus(content); err != nil {
		l.log.Debug("obsync could not write its status file, so it will read as stale",
			"problem", err)
	}
}

// needsHuman says one ERROR line, and says nothing at all if obsync has told a
// human something within the hour.
//
// This is §9's "healthy is quiet; broken repeats hourly", and the two halves are
// one mechanism rather than two: obsync writes nothing at all on a run that
// changed nothing, so `docker logs --since 1h` is empty exactly when nothing is
// wrong — and never empty when something is, because whatever a human is needed
// for is repeated inside every hour it stands.
//
// It is a counter over the existing tick rather than a schedule of its own:
// nothing here starts a timer, and the question is asked at the end of a
// wake-up obsync was going to have anyway. The comparison is against the clock
// rather than a count of runs because the tick is jittered — sixty ticks is
// fifty-four minutes as easily as sixty-six.
//
// The freezes stamp this themselves when they announce their own entry, which
// is what keeps state entry said exactly once: the line that says what a freeze
// is arrives immediately, and the repeat that follows is an hour later rather
// than in the same breath. Everything else a human is needed for — a push that
// has never once succeeded, a remote gone past the backoff ceiling, a vault
// obsync cannot bootstrap — has no entry line of its own, so this is it, and
// the first occurrence is immediate because nothing has been said.
func (l *Loop) needsHuman(now time.Time, message string, attrs ...any) {
	if !l.saidNeedsHumanAt.IsZero() && now.Sub(l.saidNeedsHumanAt) < hourly {
		return
	}
	l.saidNeedsHumanAt = now
	l.log.Error(message, attrs...)
}
