package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The cadence (#25), driven at seam 1 through the fake clock: when obsync
// commits, when it pushes, when it wakes with nobody typing, how it waits out a
// remote that is not there, and what a SIGTERM does to the run in flight.
//
// Nothing here sleeps. Every one of these numbers is a constant with a measured
// reason beside it in internal/loop/cadence.go, and a suite that slept through
// them would take longer to run than the vault takes to sync.

// Someone stops typing and ten seconds later their edit is in the repo (§2).
func TestAChangeCommitsOnceTheVaultHasBeenQuietForTenSeconds(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "the note I stopped typing\n")
	env.watcherWake()
	env.advance(quietWindow)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I stopped typing\n" {
		t.Errorf("the remote holds %q ten seconds after the vault went quiet, want the note (§2)", got)
	}
}

// The other half of the same rule, and the half that makes the number mean
// something: nine seconds after the last change, nothing has been committed.
//
// The quiet window is what distinguishes "the human paused" from "the human is
// mid-sentence", and it is 10s because ignis's write pipeline can still be
// landing bytes ~2.5s after the last keystroke.
func TestNothingIsCommittedBeforeTheQuietWindowClears(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "still typing\n")
	env.watcherWake()
	env.advance(quietWindow - time.Second)

	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits nine seconds after a change, want %s — the quiet "+
			"window has not cleared (§2)", got, want)
	}
}

// An unbroken hour of writing is about twelve commits: the quiet window never
// clears, so the max-wait cap is what commits, and each capped commit is an
// honest partial snapshot rather than a burst mode nobody wrote (§2).
//
// The hour is 400 wake-ups nine seconds apart — a vault that is never quiet for
// ten. What the count is really asserting is that the cap and not the tick
// decides: sixty ticks turn inside this hour, and a design where the tick
// committed would leave sixty commits behind it.
func TestAnUnbrokenHourOfWritingCommitsAboutTwelveTimes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	const (
		keystroke = 9 * time.Second
		hour      = time.Hour
	)
	writes := int(hour / keystroke)
	for i := range writes {
		env.writeNote(fmt.Sprintf("Daily/note %d.md", i), fmt.Sprintf("note %d\n", i))
		env.watcherWake()
		env.advance(keystroke)
	}

	// One commit was there before obsync started, and the rest are the cap
	// firing: 3600s over a 300s cap, plus the few seconds each capped run takes
	// to pick the burst back up.
	committed, err := strconv.Atoi(env.commitsSoFar(env.vault))
	if err != nil {
		t.Fatalf("counting the vault's commits: %v", err)
	}
	if capped := committed - 1; capped < 11 || capped > 13 {
		t.Errorf("an unbroken hour of writing produced %d commits, want about twelve — the "+
			"max-wait cap is what bounds it (§2)", capped)
	}

	// A partial snapshot, which is the honest thing a capped commit is: the
	// first note is in the remote long since, and the last one — written nine
	// seconds before the hour ended — is not, because the vault has not been
	// quiet since.
	if !env.remoteHoldsYet("Daily/note 0.md") {
		t.Error("the remote does not hold the first note of the hour, want the capped commits pushed (§2)")
	}
	last := fmt.Sprintf("Daily/note %d.md", writes-1)
	if env.remoteHoldsYet(last) {
		t.Errorf("the remote holds %q while the vault is still being written to, want it left for "+
			"the run after the vault goes quiet (§2)", last)
	}

	// And it converges: the human stops typing, the quiet window clears, and
	// the next run captures everything the capped commits left behind.
	env.advance(quietWindow)

	if got, want := env.remoteFile(last), fmt.Sprintf("note %d\n", writes-1); got != want {
		t.Errorf("the remote holds %q at the last note written, want %q — the partial snapshots "+
			"converge on the right tree (§2)", got, want)
	}
}

// Every wake-up runs the whole loop, so the tick is not blocked by the quiet
// window: while the human is still typing, a tick still pushes what an earlier
// run committed. One timer, not two, and one cadence rather than a commit
// schedule and a push schedule (§2).
func TestATickPushesWhileTheHumanIsStillTyping(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	// A commit that could not be pushed, so that there is something for a tick
	// to do while the vault is hot.
	env.remoteAway()
	env.writeNote("Daily/2026-08-24.md", "written while the remote was away\n")
	env.watcherWake()
	env.advance(quietWindow)
	env.remoteBack()

	// Then an unbroken two and a half minutes of typing, which is long enough
	// for two ticks and nowhere near the five-minute cap.
	for i := range 30 {
		env.writeNote(fmt.Sprintf("Notes/still typing %d.md", i), "mid-sentence\n")
		env.watcherWake()
		env.advance(5 * time.Second)
	}

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while the remote was away\n" {
		t.Errorf("the remote holds %q, want the commit a tick pushed while the human typed (§2)", got)
	}
	if env.remoteHoldsYet("Notes/still typing 0.md") {
		t.Error("a tick committed a note the human was still typing, want the commit deferred to " +
			"the quiet window or the cap (§2)")
	}
}

// The tick is the upper bound on both "someone pushed from their laptop" and
// "the watcher dropped an event", so it commits a change no wake-up ever
// reported. This is also tick-only mode's whole behaviour: latency degrades to
// the tick, and what obsync commits does not change.
func TestATickCommitsAChangeNoWakeUpEverReported(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "the watcher never mentioned this\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the watcher never mentioned this\n" {
		t.Errorf("the remote holds %q one tick after an unreported change, want the note (§2)", got)
	}
}

// And it does not turn early: at 53 seconds nothing has happened, which is what
// makes 60s ± 10% a number rather than an upper bound.
func TestATickDoesNotTurnBeforeItsBand(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.writeNote("Daily/2026-08-24.md", "not yet\n")
	env.advance(tick - tickJitter - time.Second)

	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits 53 seconds in, want %s — the tick is 60s jittered by "+
			"±10%% and the earliest it can fall due is 54s (§2)", got, want)
	}
}

// The tick is jittered because many sidecars point at one org, and a fleet of
// them waking on the minute is a thundering herd on someone's forge.
//
// Both halves are asserted: every tick is inside the band, and the ticks are
// not all the same number. The jitter is drawn over a continuous range, so a
// run of eight identical draws is not a flake this can suffer — it is an
// arithmetic impossibility short of the jitter being gone.
func TestTheTickIsSixtySecondsAndJittered(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	for range 8 {
		env.advance(tick + tickJitter)
	}

	// Every run takes out a network deadline of its own, because every run
	// fetches: the tick is the upper bound on someone else's push arriving, and
	// it is only that because the run it starts asks the remote (§2). Those are
	// not cadence waits — nothing is killed when a tick expires — so the ticks
	// are the waits that are not one.
	var waits []time.Duration
	for _, waited := range env.clock.waitsTaken() {
		if waited != networkDeadline {
			waits = append(waits, waited)
		}
	}
	if len(waits) < 9 {
		t.Fatalf("obsync waited %d times over nine ticks, want one wait per tick", len(waits))
	}
	distinct := map[time.Duration]bool{}
	for _, waited := range waits {
		if waited < tick-tickJitter || waited > tick+tickJitter {
			t.Errorf("obsync waited %s for a tick, want 60s jittered by at most ±6s (§2)", waited)
		}
		distinct[waited] = true
	}
	if len(distinct) == 1 {
		t.Errorf("obsync waited exactly %s for every one of %d ticks, want the tick jittered (§2)",
			waits[0], len(waits))
	}
}

// The local half cannot fail for network reasons: with the remote gone, obsync
// degrades to a local autocommitter, and catches up when it comes back (§2).
func TestTheLocalHalfKeepsCommittingWhileTheRemoteIsUnreachable(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	env.writeNote("Daily/2026-08-24.md", "first\n")
	env.watcherWake()
	env.advance(quietWindow)

	env.writeNote("Daily/2026-08-25.md", "second\n")
	env.watcherWake()
	env.advance(quietWindow)

	if got, want := env.commitsSoFar(env.vault), "3"; got != want {
		t.Errorf("the vault holds %s commits with the remote unreachable, want %s — the local half "+
			"cannot fail for network reasons (§2)", got, want)
	}

	env.remoteBack()
	env.advance(200 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "first\n" {
		t.Errorf("the remote holds %q, want the note obsync committed while it was away", got)
	}
	if got, want := env.commitsOn(env.remote), "3"; got != want {
		t.Errorf("the remote holds %s commits, want %s — obsync catches up (§2)", got, want)
	}
}

// The network backoff runs 60s → 15m, doubling. The doubling is observable
// exactly once: a retry that would have happened at the floor does not happen
// after the second failure, and the one after it does.
func TestTheNetworkBackoffDoublesWhileTheRemoteStaysUnreachable(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	// The first failure buys 60s.
	env.writeNote("Daily/2026-08-24.md", "eventually\n")
	env.watcherWake()
	env.advance(quietWindow)

	// A tick well past that retries, fails, and buys 120s.
	env.advance(200 * time.Second)
	env.remoteBack()

	// A tick 70s later is past what the first failure bought and short of what
	// the second one did, so nothing is pushed.
	env.advance(70 * time.Second)
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("obsync pushed 70s after its second failed push, want the backoff doubled to " +
			"120s (§2)")
	}

	env.advance(70 * time.Second)
	if got := env.remoteFile("Daily/2026-08-24.md"); got != "eventually\n" {
		t.Errorf("the remote holds %q once the doubled backoff expired, want the note (§2)", got)
	}
}

// Any network success resets the backoff to its floor, so the failure after a
// working spell waits 60s rather than picking up where an older run of failures
// left off.
func TestAnySuccessResetsTheNetworkBackoff(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	// Two failures, so the backoff is at 120s and a design that never reset it
	// would be at 240s after the next one.
	env.writeNote("Daily/2026-08-24.md", "first\n")
	env.watcherWake()
	env.advance(quietWindow)
	env.advance(200 * time.Second)

	// The remote comes back and a tick pushes: one success.
	env.remoteBack()
	env.advance(200 * time.Second)
	if !env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Fatal("the remote never took the push this test needs to have succeeded")
	}

	// It goes away again, one more failure, and 70s later the retry must
	// happen — which it can only do if the backoff went back to 60s.
	env.remoteAway()
	env.writeNote("Daily/2026-08-25.md", "second\n")
	env.watcherWake()
	env.advance(quietWindow)
	env.remoteBack()
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-25.md"); got != "second\n" {
		t.Errorf("the remote holds %q 70s after a failure that followed a success, want the note — "+
			"any success resets the backoff to 60s (§2)", got)
	}
}

// The doubling stops at fifteen minutes. Past that, waiting longer buys
// nothing: the remote is either coming back or it is not, and §9 answers that
// with a health verdict at 24h rather than by ever giving up on the retry.
//
// Six failures is where the difference shows: capped, the wait is 15m; doubling
// unchecked from 60s it would be 32m, and the retry this test watches for would
// not happen.
func TestTheNetworkBackoffStopsDoublingAtFifteenMinutes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	env.writeNote("Daily/2026-08-24.md", "eventually\n")
	env.watcherWake()
	env.advance(quietWindow)

	// Five more failures, each a tick far enough past the last that the retry
	// is certainly due: 60s, 120s, 240s, 480s, then the ceiling.
	for range 5 {
		env.advance(20 * time.Minute)
	}

	env.remoteBack()
	env.advance(16 * time.Minute)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "eventually\n" {
		t.Errorf("the remote holds %q sixteen minutes after the sixth failed push, want the note — "+
			"the backoff stops doubling at 15m (§2)", got)
	}
}

// A remote that is down is not a log to fill: the backoff is what stops obsync
// reporting the same unreachable remote on every wake-up.
func TestAnUnreachableRemoteIsNotReportedOnEveryWakeUp(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.remoteAway()

	for i := range 3 {
		env.writeNote(fmt.Sprintf("Daily/2026-08-2%d.md", i), "away\n")
		env.watcherWake()
		env.advance(quietWindow)
	}
	env.remoteBack()

	if got, want := strings.Count(env.said(), "level=ERROR"), 1; got != want {
		t.Errorf("obsync reported the unreachable remote %d times over three wake-ups, want %d — "+
			"only the network half backs off, and the wake-ups inside it say nothing (§2, §9)",
			got, want)
	}
}

// A watcher that goes away leaves obsync ticking, not stopped. Tearing every
// watch down is what §1 says obsync does on inotify ENOSPC, and the mode it
// then runs in is the one the shipped binary already runs in with no watcher at
// all: latency degrades to the tick and what obsync commits does not change,
// because every run asks git what changed and the watcher never says.
//
// Stopping instead would be the failure this design fears most. obsync never
// exits on a sync failure (§2), and a watcher standing down is not even that —
// the process would go quietly, the container with it, and the vault would stop
// being backed up with nothing anywhere saying so.
func TestAWatcherThatGoesAwayLeavesObsyncInTickOnlyMode(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	env.watcherGone()
	env.writeNote("Daily/2026-08-24.md", "written after the watcher stood down\n")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written after the watcher stood down\n" {
		t.Errorf("the remote holds %q one tick after the watcher went away, want the note — a "+
			"vault with no watcher is tick-only mode, not a vault obsync has stopped syncing (§1, §2)", got)
	}
}

// A SIGTERM arriving before obsync has run at all refuses the startup run like
// any other: "refuse to start a new run" is about every run, and a container
// stopped seconds after it started has committed nothing (§1).
func TestASIGTERMBeforeTheStartupRunRefusesToStartIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "never committed\n")

	env.runAlreadyStopped()

	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits after a SIGTERM that arrived first, want %s (§1)", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits after a SIGTERM that arrived first, want %s (§1)", got, want)
	}
}

// On SIGTERM obsync finishes the run in flight and exits, with a hard deadline
// of ~30s (§1). The run is not interrupted — the one thing in it that can be
// waiting on the outside world is a network git, so that is what the deadline
// cuts short, at 30s rather than at its own 120s.
//
// The vault keeps what the run had already committed, which is the whole reason
// the deadline is allowed to be hard: nothing obsync captured is at risk, only
// a push that had not landed.
func TestASIGTERMCutsAHungPushShortAtTheShutdownDeadline(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	env.installHook("pre-receive", "#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n")
	env.writeNote("Daily/2026-08-24.md", "committed but never pushed\n")

	env.turn()
	held := awaitPid(t, pidFile)
	// The hook running is the push being in flight, so every deadline this run
	// takes out — its fetch's and its push's, both 120s — has been taken by
	// now. They are dropped rather than counted, so that what is waited for
	// below is the one obsync takes out when it sees the stop.
	env.clock.drainDeadlines()

	// docker stop, with the push hung.
	env.sigterm()
	env.clock.awaitDeadline(t)
	env.clock.advanceToNextDeadline(t)
	env.stop()

	if got, want := env.clock.elapsed(), shutdownDeadline; got != want {
		t.Errorf("obsync took %s to stop with a hung push in flight, want %s (§1)", got, want)
	}
	assertProcessGone(t, held)
	if got, want := env.commitsOn(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — stopping does not undo a commit", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits, want %s — the push never landed", got, want)
	}
	if said := env.said(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about being stopped, want no ERROR: a push cut short by a "+
			"shutdown is not a failure a human is needed for (§9)", said)
	}
}

// remoteAway and remoteBack make the remote unreachable and reachable again,
// which is what a flaky home uplink looks like from inside the container. The
// bare repo is moved rather than deleted, because a test that has to put it
// back is a test that can watch obsync catch up.
func (e *vaultEnv) remoteAway() {
	e.t.Helper()

	if err := os.Rename(e.remote, e.remote+".away"); err != nil {
		e.t.Fatalf("taking the remote away: %v", err)
	}
}

func (e *vaultEnv) remoteBack() {
	e.t.Helper()

	if err := os.Rename(e.remote+".away", e.remote); err != nil {
		e.t.Fatalf("putting the remote back: %v", err)
	}
}
