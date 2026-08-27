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
