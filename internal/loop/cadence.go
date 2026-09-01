package loop

import (
	"math/rand/v2"
	"time"
)

// The cadence, and every value in it is a constant with a measured reason
// beside it rather than a knob (§2). None of these is a fact about a
// deployment, which is what earns a knob (§8): a tunable quiet window is an
// invitation to set it to 0 and get a commit per keystroke.
const (
	// quietWindow is how long the vault must go unmodified before a sync run
	// is allowed to commit — the difference between "the human paused" and
	// "the human is mid-sentence". 10s clears ignis's write pipeline with
	// margin: browser-side coalescing at 100ms/2000ms, then a 300ms chokidar
	// stability threshold, so bytes can still land ~2.5s after the last
	// keystroke and anything under ~3s commits into a moving tree.
	quietWindow = 10 * time.Second

	// maxWaitCap is the ceiling on how long the quiet window may defer a
	// commit, and the value doing the real work on history readability: five
	// minutes bounds an unbroken hour of writing to about twelve commits
	// rather than to one enormous one — or, with no cap at all, to none.
	//
	// It may waive the quiet window because the quiet window is taste. It may
	// not waive the settle guard (§6), which is about valid bytes: a cap that
	// can waive correctness is a bug with a timer.
	maxWaitCap = 5 * time.Minute

	// unsettledForLong is how long a path must stay continuously unsettled
	// before obsync stops treating its exclusion as transient and says so
	// (§6). Ten minutes is 2× the max-wait cap, which is what makes it safe:
	// a legitimately busy file — one a human types into without a ten-second
	// pause for a whole working stretch — is committed by the cap long before
	// this, so what is left past it is a file something is rewriting faster
	// than obsync can ever see it still.
	//
	// It is a threshold on a warning rather than on a behaviour: the path is
	// excluded from the first second and this changes nothing about that.
	unsettledForLong = 10 * time.Minute

	// tick is the periodic wake-up that starts a sync run when nothing else
	// has, and the upper bound on both "someone pushed from their laptop" and
	// "the watcher dropped an event". One timer, not two — the tick subsumes
	// reconciliation, so a fetch tick already reconciles and a reconciliation
	// tick already fetches, which is what makes that bound 60s rather than the
	// five minutes a separate timer would have given.
	tick = 60 * time.Second

	// tickJitter spreads the tick over ±10%, because many sidecars point at
	// one org and a fleet of them waking on the minute is a thundering herd
	// nobody asked for.
	tickJitter = 6 * time.Second

	// pushFloor is an optional lower bound between pushes. It is off by
	// default, and it is a comparison on the existing loop rather than a
	// timer of its own, which is the whole of what "a rate limit rather than a
	// second schedule" means: commit rate and push rate stay one cadence.
	//
	// Nothing turns it on. It is not one of §8's nine variables, and a tenth
	// is a spec change rather than an implementation detail.
	pushFloor = 0

	// The network backoff: 60s, doubling, and never longer than 15m. Reset to
	// 60s by any network success (§2). Only the network half backs off — the
	// local half is not gated by it, which is what leaves obsync a local
	// autocommitter that catches up when the remote is unreachable.
	//
	// backoffLongest is not the backoff ceiling: that is §9's 24h, a health
	// verdict rather than a wait, and obsync keeps retrying past both.
	backoffFloor   = 60 * time.Second
	backoffLongest = 15 * time.Minute
)

// hourly is the one cadence a broken obsync runs on: the remote-rejection
// retry (§7) and the hourly repeat of anything a human is needed for (§9).
//
// One constant for both, deliberately, and §2's table states them as one row —
// "the retry and the report are one tick". Two of them would be two schedules
// for one idea, and a rejection whose retry and whose repeat fell on different
// ticks would tell an operator about a state obsync had just re-tested and say
// nothing about what it found.
//
// An hour rather than the backoff's 15m ceiling, and the difference is what the
// two waits are for: fifteen minutes is for a remote that might come back, an
// hour for one that has already answered. Nothing about the wait repairs a
// rejection — a human changes something on the remote — so the retry exists to
// notice that they have, and hourly is often enough for that and rare enough
// that the pack a rejection keeps re-uploading is not sent every minute.
const hourly = time.Hour

// backoffCeiling is the point at which a remote that has merely gone quiet
// stops being healthy: 24 hours (§9).
//
// It is **not a retry limit**, and nothing in the loop reads it as one: obsync
// keeps backing off and retrying past it, and the only thing that changes is
// the health verdict. It exists because waiting is the correct behaviour and
// stays correct — nothing about the failure ever escalates it — so only elapsed
// time separates a remote that will come back from one that will not, and a day
// is the point at which an operator would rather be told than left to find out.
//
// It lives here with the other cadence constants because it is measured against
// the same clock they are, and it is carried into the status file rather than
// re-declared by the subcommand that reads it: two copies of a number is two
// answers to one question, waiting to drift.
const backoffCeiling = 24 * time.Hour

// neverWorkedWindow is how long a network half that has never once got through
// may go on failing before obsync says so: five ticks, which is five minutes
// (§9).
//
// It is the persistence threshold in time, like the staleness window beside it
// and for the same reason — five wake-ups is what "long enough to stop
// believing in bad luck" means for a loop whose wake-ups are a tick apart. One
// network half that failed is the abort tier and is not news; five minutes of a
// deployment that has never once reached its remote is.
//
// It gates a line and nothing else. The health verdict is untouched, because a
// remote that is merely down is healthy until the backoff ceiling however
// loudly obsync says it is down (§9).
const neverWorkedWindow = persistenceThreshold * tick

// stalenessWindow is how long the status file may go unwritten before that is
// evidence the sync loop has stopped turning: five ticks, which is five
// minutes (§9).
//
// It is the persistence threshold and the tick multiplied rather than a third
// number, because it *is* those two: five wake-ups is what "long enough to stop
// believing in bad luck" means for a loop whose wake-ups are a tick apart. The
// tick is jittered by a tenth, so five of them is at most 5m30s of real time
// against a 5m window — which cannot false-positive, because the file is
// rewritten at the end of *every* wake-up rather than every fifth.
const stalenessWindow = persistenceThreshold * tick

// persistenceThreshold is how many times in a row something has to happen
// before obsync stops believing in bad luck: five.
//
// It is one constant for that idea rather than one per user, deliberately, and
// §2's table states it once for both — the local failure streak (§7, #34) and
// the status file's staleness window, which is five ticks (§9, #37). A design
// that carried two of them would be answering the same question twice and
// inviting the two answers to drift.
//
// It is a count rather than a duration, and it lives here because it is the
// other half of the same table: five runs is five wake-ups, so what it actually
// measures is five ticks of elapsed time — long enough that a transient loss
// has had every chance to clear, and short enough that a human is told inside
// five minutes rather than at leisure.
const persistenceThreshold = 5

// cadence is when the sync loop wakes, and whether the run it wakes for may
// commit.
//
// It holds no timers and starts nothing: the loop asks it for the next moment
// something is due and waits for that on the injected clock. That is what makes
// every constant above assertable in a suite that runs in seconds rather than
// slept through (#21's testing decisions).
type cadence struct {
	// tickAt is when the next tick falls due, measured from the end of the
	// last run and re-jittered each time.
	tickAt time.Time

	// pendingSince is the first wake-up of the burst obsync is currently
	// waiting out and lastWake the most recent one. Zero means no burst: the
	// watcher has said nothing since the last run that captured the vault.
	//
	// A burst is tracked from wake-ups rather than from the tree, which is
	// what makes a dead watcher degrade cleanly: with nothing waking obsync,
	// there is no burst, and every tick commits what git reports.
	pendingSince time.Time
	lastWake     time.Time
}

// woke records a wake-up from the watcher. The watcher says that something
// happened and never what, so this is the whole of what a wake-up carries (§2).
func (c *cadence) woke(now time.Time) {
	if c.pendingSince.IsZero() {
		c.pendingSince = now
	}
	c.lastWake = now
}

// nextRun is the moment the loop should wake next: the earliest of the tick,
// the quiet window clearing, and the max-wait cap firing.
func (c *cadence) nextRun() time.Time {
	due := c.tickAt
	if c.pendingSince.IsZero() {
		return due
	}
	if quiet := c.lastWake.Add(quietWindow); quiet.Before(due) {
		due = quiet
	}
	if capped := c.pendingSince.Add(maxWaitCap); capped.Before(due) {
		due = capped
	}
	return due
}

// mayCommit reports whether a run starting now may commit.
//
// The quiet window gates the commit rather than the run, which is what lets one
// timer serve both jobs: a tick still fetches, reconciles and pushes while the
// human is mid-sentence, and the commit it would have made waits for the vault
// to go quiet or for the cap to fire. Without that split, an hour of unbroken
// writing would produce a commit per tick — sixty of them — and the cap would
// mean nothing.
func (c *cadence) mayCommit(now time.Time) bool {
	if c.pendingSince.IsZero() {
		return true
	}
	if !now.Before(c.lastWake.Add(quietWindow)) {
		return true
	}
	return !now.Before(c.pendingSince.Add(maxWaitCap))
}

// ran records that a sync run has just finished. committing says whether it was
// the kind of run that captures the vault: one that was, ends the burst the
// quiet window was waiting out, because what it committed is everything git
// reported. One that was not leaves the burst alone — resetting the cap's
// anchor on every tick is how a vault that is never quiet would never commit.
func (c *cadence) ran(now time.Time, committing bool) {
	c.tickAt = now.Add(tick + jitter())
	if committing {
		c.pendingSince, c.lastWake = time.Time{}, time.Time{}
	}
}

// jitter is uniform over ±tickJitter.
func jitter() time.Duration {
	return rand.N(2*tickJitter) - tickJitter
}
