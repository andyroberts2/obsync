package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §9 is the half of this design that a human ever sees: `docker ps` shows
// whether obsync needs one, `docker exec obsync status` answers a suspicious
// operator directly, and `docker logs --since 1h` is empty exactly when nothing
// is wrong. A parked obsync means PID liveness carries no information at all,
// so all of that is manufactured to replace the signal a normal daemon gets for
// free by dying.
//
// Every assertion below reads it the way an operator or Docker does: the exit
// status of a subcommand run in a process of its own, the report it prints, and
// what obsync's log holds. The status file itself is private — it is read
// through the subcommands here for the same reason it is documented that way.

// The two windows §9 states, written out at the assertions that use them
// because the numbers are the promise: five ticks of staleness, and a day
// before a remote that has merely gone quiet stops being healthy.
const (
	stalenessWindow = 5 * tick
	backoffCeiling  = 24 * time.Hour
)

// A healthy obsync answers Docker with a 0 and keeps its own record where it
// can never be synced anywhere: inside `.git`, which is outside every commit by
// construction (§9, §10).
func TestAHealthyObsyncPassesTheHealthcheckAndKeepsItsRecordInsideTheRepository(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "an ordinary run\n")

	env.wake()

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d after a run that committed and pushed, want 0 — "+
			"health answers whether this needs a human, not whether everything is working (§9); "+
			"obsync said:\n%s", got, env.saidSoFar())
	}

	// The owned path §10 declares, and the staging directory every obsync write
	// is renamed out of, left with nothing in it.
	if _, err := os.Stat(filepath.Join(env.vault, ".git", "obsync", "status.json")); err != nil {
		t.Errorf("obsync wrote no status file under .git/obsync/: %v — siting it there is what "+
			"makes a dropped mount, a directory that is not a repository and a container that has "+
			"not run yet all read as unhealthy with no special case (§9)", err)
	}
	debris, err := os.ReadDir(filepath.Join(env.vault, ".git", "obsync", "tmp"))
	if err != nil {
		t.Fatalf("reading obsync's staging directory: %v", err)
	}
	if len(debris) != 0 {
		t.Errorf("obsync's staging directory holds %d entries after a run, want none — the status "+
			"file goes write-then-rename through it, so a reader sees the previous bytes or the "+
			"new ones and never a half-written file (§6)", len(debris))
	}
	// Nothing obsync wrote about itself is in the working tree, which is the
	// whole of what "private" buys: it cannot be committed, cannot conflict,
	// and cannot reach anybody's clone.
	if dirty := strings.TrimSpace(env.mustGit(env.vault, "status", "--porcelain")); dirty != "" {
		t.Errorf("the vault is dirty after a run: %q — obsync's own record of itself lives inside "+
			"`.git`, where `git status` can never see it (§9)", dirty)
	}
	if said := env.said(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q on a run that worked, want nothing a human is needed for (§9)", said)
	}
}

// The three ways there is nothing to read, and all three are unhealthy without
// a line of code apiece: a directory that is not a repository has no `.git` to
// write into, and a container that has not finished a run has written nothing
// yet. The mount dropping is the third and takes the whole vault with it.
func TestAContainerThatHasNotFinishedASyncRunIsUnhealthy(t *testing.T) {
	t.Parallel()

	for _, unhealthy := range []struct {
		name  string
		build func(*testing.T) *vaultEnv
	}{
		{
			name:  "a vault obsync has not run against yet",
			build: func(t *testing.T) *vaultEnv { return newVault(t) },
		},
		{
			name:  "a directory that is not a repository",
			build: func(t *testing.T) *vaultEnv { return newVaultToBootstrap(t, nil) },
		},
	} {
		t.Run(unhealthy.name, func(t *testing.T) {
			t.Parallel()

			env := unhealthy.build(t)
			env.clockAnchoredToNow()

			if got := env.healthcheck(); got != 1 {
				t.Errorf("obsync healthcheck exited %d over %s, want 1 — nobody has seen this "+
					"deployment work yet, and the absent file is the signal (§9)", got, unhealthy.name)
			}
			if report := env.statusReport(); !strings.Contains(report, "needs a human: yes") {
				t.Errorf("obsync status printed %q over %s, want it to say a human is needed and "+
					"exit 0 anyway (§10)", report, unhealthy.name)
			}
		})
	}
}

// A full freeze is unhealthy at once, the report carries the fact and the
// remedy the log line carried, and repairing the cause clears it within a tick
// with no restart — which is the highest-value thing obsync can say anywhere,
// because self-clearing design is worth nothing if the operator's reflex is to
// restart the container and destroy the diagnosis (§7, §9).
func TestAFullFreezeIsUnhealthyAtOnceAndHealthyAgainWhenItsCauseIsRepaired(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.turn()
	env.awaitIdle()

	if got := env.healthcheck(); got != 0 {
		t.Fatalf("obsync healthcheck exited %d before anything was wrong, want 0; it said:\n%s",
			got, env.saidSoFar())
	}

	// The vault sentinel: every note and `.obsidian/` gone while `.git` is
	// still there, which is what a dropped mount looks like from inside git.
	env.theVaultGoesEmpty()
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d while fully frozen, want 1 — any full freeze needs "+
			"a human (§9); it said:\n%s", got, env.saidSoFar())
	}
	report := env.statusReport()
	if !strings.Contains(report, ".obsidian") {
		t.Errorf("obsync status printed %q, want the conclusive fact behind the freeze — the "+
			"report says what the log line said rather than a second wording of it (§9)", report)
	}
	if !strings.Contains(report, "This clears on its own once fixed; no restart needed") {
		t.Errorf("obsync status printed %q, want the remedy to close with the sentence that keeps "+
			"an operator from restarting the container and destroying the diagnosis (§9, §11)", report)
	}

	env.theVaultComesBack()
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d a tick after the mount came back, want 0 — every "+
			"freeze clears when its cause is repaired, with no restart (§7); it said:\n%s",
			got, env.saidSoFar())
	}
	if report := env.statusReport(); !strings.Contains(report, "needs a human: no") {
		t.Errorf("obsync status printed %q once the freeze cleared, want it to say so", report)
	}
}

// The two remote rules are opposites and both are surprising: a remote that has
// *rejected* a push is unhealthy at once, because no amount of waiting repairs
// a verdict (§7, §9).
//
// The report carries the remote's own words, which exist nowhere else — obsync
// will not make that push again for an hour, and the log line scrolls away.
func TestARemoteRejectionIsUnhealthyImmediatelyAndTheReportCarriesTheRemotesWords(t *testing.T) {
	t.Parallel()

	// A deployment that has been working, so that the rejection is the only
	// thing wrong with it: a vault whose very first push were refused would be
	// unhealthy for a second reason — nobody having ever seen it work — and
	// this is the row about the verdict.
	env := newVault(t)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "written before the remote's rule changed\n")
	env.turn()
	env.awaitIdle()

	env.installHook("pre-receive", refusesObsyncsCommits)
	env.writeNote("Daily/2026-08-25.md", "written after the remote's rule changed\n")
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d on the first rejected push, want 1 — a rejection is "+
			"a verdict from the party whose opinion is the whole question, so it is unhealthy at "+
			"once rather than after a streak (§7, §9); it said:\n%s", got, env.saidSoFar())
	}
	report := env.statusReport()
	if !strings.Contains(report, "policy: obsync may not push here") {
		t.Errorf("obsync status printed %q, want the remote's own words relayed — they exist "+
			"nowhere else, because obsync will not make that push again for an hour (§7, §9)", report)
	}
	if !strings.Contains(report, "remote rejection") {
		t.Errorf("obsync status printed %q, want it to name the state a human has to deal with", report)
	}
}

// The opposite rule, and the more surprising of the two: a remote that is
// merely unreachable is healthy for a day. Ten minutes of downtime does not
// page anybody, and the local half keeps capturing the vault throughout.
//
// The ceiling is **not a retry limit**. obsync keeps backing off and retrying
// past it, and only the health verdict changes — which is what the last third
// of this test is about.
func TestAnUnreachableRemoteIsHealthyForADayAndUnhealthyPastTheCeiling(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "written while the remote was there\n")
	env.turn()
	env.awaitIdle()

	env.remoteAway()
	env.writeNote("Daily/2026-08-25.md", "written while the remote was away\n")

	// The clock the ceiling is measured against starts at the run that first
	// could not reach the remote, rather than at the moment the remote went:
	// obsync measures what it has observed, and it observes on a tick.
	env.advance(70 * time.Second)
	env.advance(backoffCeiling - time.Hour)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d with the remote away for 23 hours, want 0 — an "+
			"unreachable remote is healthy inside the 24h ceiling, and any amount of backoff is "+
			"healthy (§9); it said:\n%s", got, env.saidSoFar())
	}
	if got := env.vaultFileYet("Daily/2026-08-25.md"); got != "written while the remote was away\n" {
		t.Errorf("the vault holds %q, want the note written while the remote was away — the local "+
			"half is not gated by the network one (§2)", got)
	}

	env.advance(2 * time.Hour)

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d with the remote away for 25 hours, want 1 — past "+
			"the ceiling, only elapsed time separates a remote that will come back from one that "+
			"will not (§9); it said:\n%s", got, env.saidSoFar())
	}
	if report := env.statusReport(); !strings.Contains(report, "24h0m0s") {
		t.Errorf("obsync status printed %q, want it to name the ceiling it is past", report)
	}

	// And the ceiling was never a retry limit: obsync has gone on backing off
	// and retrying throughout, so the remote coming back is enough.
	env.remoteBack()
	env.advance(20 * time.Minute)

	if got, _ := env.remoteContentYet("Daily/2026-08-25.md"); got != "written while the remote was away\n" {
		t.Errorf("the remote holds %q once it came back after a day away, want the note obsync "+
			"had been holding — the backoff ceiling is a health verdict rather than a retry "+
			"limit (§9); obsync said:\n%s", got, env.saidSoFar())
	}
	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d once the remote came back, want 0; it said:\n%s",
			got, env.saidSoFar())
	}
	if said := env.said(); !strings.Contains(said, `msg="the remote answered again"`) {
		t.Errorf("obsync said %q, want the recovery said once — every recovery is INFO (§9)", said)
	}
}

// §9's middle state, and the reason obsync runs no startup push-capability
// probe: a token scoped to read and not write clones and fetches perfectly and
// fails only at the first real push. Nobody has ever seen this deployment work,
// so it is an ERROR immediately and unhealthy at once rather than riding the
// backoff quietly for a day like an established deployment would.
//
// The run itself is still an aborted run and says nothing above debug — the two
// are not in tension: the tier decides what a *failure* means, and this is a
// standing state about the deployment.
func TestATokenThatReadsAndCannotWriteIsAnErrorAtTheFirstPushAndUnhealthyAtOnce(t *testing.T) {
	t.Parallel()

	const credential = "ghp_the_read_only_token"
	env, remote := newAuthenticatedVault(t, writeCredential(t, credential+"\n"), credential)
	env.clockAnchoredToNow()
	remote.takesNoWrites()
	env.writeNote("Daily/2026-08-24.md", "the first thing this deployment ever wrote\n")

	env.turn()
	env.awaitIdle()

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d after a push that has never once succeeded, want "+
			"1 — this is the deployment nobody has ever seen work, and finding it is what the "+
			"startup probe would have been for (§8, §9); it said:\n%s", got, env.saidSoFar())
	}
	said := env.saidSoFar()
	if !strings.Contains(said, "level=ERROR") || !strings.Contains(said, "never once succeeded") {
		t.Errorf("obsync said %q, want an ERROR immediately rather than a quiet backoff (§9)", said)
	}
	if report := env.statusReport(); !strings.Contains(report, "never — obsync has tried") {
		t.Errorf("obsync status printed %q, want *never* said in the way that cannot be mistaken "+
			"for *obsync has had nothing to push* — never pushed is three states, not two (§9)", report)
	}

	// The operator widens the scope, which is the only repair there is, and
	// obsync recovers with no restart.
	remote.takesWritesAgain()
	env.advance(2 * time.Minute)

	if got, _ := env.remoteContentYet("Daily/2026-08-24.md"); got != "the first thing this deployment ever wrote\n" {
		t.Errorf("the remote holds %q once the token could write, want the note obsync had been "+
			"holding; obsync said:\n%s", got, env.saidSoFar())
	}
	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d once a push had landed, want 0; it said:\n%s",
			got, env.saidSoFar())
	}
}

// The first of §9's three states, and the one that must never be reported as a
// failure: obsync has had nothing to push. It is told from *attempted, never
// succeeded* by whether obsync has tried, which it knows — and the difference
// matters most on exactly the vault where the two look alike from outside, one
// whose remote is not answering.
func TestARemoteObsyncHasHadNothingToPushToIsNeverAFailure(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.remoteAway()

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d with nothing to push and a remote that is not "+
			"there, want 0 — nothing has changed yet, which is not a failure and is never "+
			"reported as one (§9); it said:\n%s", got, env.saidSoFar())
	}
	if report := env.statusReport(); !strings.Contains(report, "never — obsync has had nothing to push") {
		t.Errorf("obsync status printed %q, want *never attempted* said as itself rather than as "+
			"the state that needs a human (§9)", report)
	}
	if said := env.said(); strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about a remote it had nothing to send, want nothing a human is "+
			"needed for (§7, §9)", said)
	}
}

// The one place §9's two remote rows could collide, and the boundary is the
// word *attempted*: a deployment that has never pushed, has something to push,
// and whose remote is merely down. "A push attempted but never once succeeded"
// is unhealthy at once and "an unreachable remote inside 24h" is healthy, and
// which applies is decided by the fetch — a remote that is not there fails it,
// so the push is never reached and nothing was ever attempted.
//
// Getting this wrong is expensive in the direction §7 names: an unhealthy
// container invites a restart, and a restart during a remote outage is a loop
// of them.
func TestAVaultWaitingOnADownRemoteHasNotAttemptedAPush(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.remoteAway()
	env.writeNote("Daily/2026-08-24.md", "written for a remote that was not there\n")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(10 * time.Minute)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d over a vault holding a commit for a remote that is "+
			"merely down, want 0 — an unreachable remote is healthy inside the ceiling, and a "+
			"push the fetch never got as far as is not one that was attempted (§9); it said:\n%s",
			got, env.saidSoFar())
	}
	if report := env.statusReport(); !strings.Contains(report, "never — obsync has had nothing to push") {
		t.Errorf("obsync status printed %q, want *never attempted* rather than the state that "+
			"needs a human — the two are told apart by whether obsync ever got to try (§9)", report)
	}

	// And it publishes the moment the remote is back, which is what says the
	// quiet was obsync waiting rather than obsync having given up.
	env.remoteBack()
	env.advance(20 * time.Minute)

	if got, _ := env.remoteContentYet("Daily/2026-08-24.md"); got != "written for a remote that was not there\n" {
		t.Errorf("the remote holds %q once it came back, want the note obsync had been holding; "+
			"obsync said:\n%s", got, env.saidSoFar())
	}
}

// neverGotThrough is the WARN a network half that has never once worked says,
// and the substring a test may hold on to: the rest of the line is a duration
// and a remedy.
const neverGotThrough = "obsync has not once reached the remote since it started"

// The hole §9's quiet left, and the one an operator falls into on their very
// first deployment: a network half that fails *before* the push rides the
// abort tier, which reports nothing above debug, and stays healthy until the
// 24h ceiling. A URL git cannot use, key material that was never mounted, a
// token the remote will not take, no egress at all — every one of them is an
// empty log and a healthy container for a day.
//
// So a network half that has never once got through says so, once, after five
// ticks of it. Five because that is the persistence threshold: one failure is
// bad luck, and five is long enough to stop believing in it (§2).
//
// It is a WARN and the health verdict is deliberately untouched. A remote that
// is merely down is healthy until the ceiling however loudly obsync says it is
// down, because an unhealthy container invites a restart and a restart during
// an outage is a loop of them (§7, §9).
func TestANetworkHalfThatHasNeverOnceGotThroughIsSaidRatherThanLeftToTheCeiling(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.remoteAway()
	env.writeNote("Daily/2026-08-24.md", "written before obsync ever reached the remote\n")
	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)

	if said := env.saidSoFar(); strings.Contains(said, neverGotThrough) {
		t.Errorf("obsync said %q one tick in, want nothing yet — one network half that failed is "+
			"the abort tier, and making a transient loss news is how the signal becomes noise "+
			"(§7)", said)
	}

	env.advance(5 * tick)

	if said := env.saidSoFar(); !strings.Contains(said, neverGotThrough) {
		t.Errorf("obsync said %q with a remote it has never once reached for five ticks, want the "+
			"one WARN that says so — the log is the only channel this state has (§9)", said)
	}
	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d, want 0 — saying so is not a health verdict, and a "+
			"remote that is merely down stays healthy until the ceiling (§9); it said:\n%s",
			got, env.saidSoFar())
	}

	env.advance(5 * tick)

	if got := strings.Count(env.saidSoFar(), neverGotThrough); got != 1 {
		t.Errorf("obsync said it %d times, want exactly once — state entry is said once, and a "+
			"line repeated once a tick is a log an operator stops reading (§9)", got)
	}

	// And the state exits the way every other one does: said once, when the
	// fact that put obsync in it stops being true.
	env.remoteBack()
	env.advance(20 * time.Minute)

	if got, _ := env.remoteContentYet("Daily/2026-08-24.md"); got != "written before obsync ever reached the remote\n" {
		t.Fatalf("the remote holds %q once it was reachable, want the note obsync had been "+
			"holding; it said:\n%s", got, env.saidSoFar())
	}
	if said := env.saidSoFar(); !strings.Contains(said, "the remote answered for the first time") {
		t.Errorf("obsync said %q once the remote finally answered, want the state exit said once "+
			"(§9)", said)
	}
}

// Liveness is a fact about the loop rather than about the outcome, so every
// wake-up refreshes it whatever the run turned out to be. A run that keeps
// losing to a third writer's `index.lock` is aborting, which is not news — but
// it is unambiguously alive, and separating the two is what makes staleness
// measure precisely one thing: has the loop stopped turning (§9).
func TestEveryWakeUpRefreshesTheLivenessTimestampWhateverTheRunWas(t *testing.T) {
	t.Parallel()

	// obsync starts ten minutes ago, which is past the staleness window: only
	// a run that refreshed the timestamp since can leave this healthy.
	env := newVault(t)
	env.clockAnchoredTo(time.Now().Add(-10 * time.Minute))
	env.turn()
	env.awaitIdle()

	env.someoneElseHoldsTheIndexLock()
	env.writeNote("Daily/2026-08-24.md", "written while somebody else held the index lock\n")
	env.advance(stalenessWindow + 4*time.Minute + 30*time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d after a run that gave up, want 0 — an aborted run "+
			"is healthy, and the wake-up it happened on still refreshed the liveness timestamp "+
			"(§9); it said:\n%s", got, env.saidSoFar())
	}
	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q about an index lock somebody else holds, want nothing above "+
			"debug — the abort tier reports nothing (§7)", said)
	}

	env.theIndexLockIsReleased()
	env.advance(70 * time.Second)

	if got, _ := env.remoteContentYet("Daily/2026-08-24.md"); got != "written while somebody else held the index lock\n" {
		t.Errorf("the remote holds %q once the lock was released, want the note the aborted runs "+
			"had been waiting to commit", got)
	}
}

// The one row a reader establishes for itself: obsync writes this file at the
// end of every wake-up, so a file that has stopped being refreshed is a loop
// that has stopped turning — which is the only failure mode nothing else in the
// design can detect, since a parked obsync looks exactly like a working one
// from outside (§9).
func TestAStatusFileStalerThanFiveTicksIsTheLoopHavingStoppedTurning(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name          string
		lastTurned    time.Duration
		wantExitCode  int
		wantInReport  string
		wantOtherwise string
	}{
		{
			name:         "four minutes ago",
			lastTurned:   4 * time.Minute,
			wantExitCode: 0,
			wantInReport: "needs a human: no",
		},
		{
			name:         "six minutes ago",
			lastTurned:   6 * time.Minute,
			wantExitCode: 1,
			wantInReport: "the sync loop has stopped turning",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			// The loop turns once and stops, at an instant this far in the
			// past: a stopped loop is exactly what this measures, and driving
			// the clock is how a suite reaches it without sleeping through
			// five ticks.
			env := newVault(t)
			env.clockAnchoredTo(time.Now().Add(-row.lastTurned))
			env.wake()

			if got := env.healthcheck(); got != row.wantExitCode {
				t.Errorf("obsync healthcheck exited %d over a loop that last turned %s, want %d — "+
					"the window is five ticks (§9)", got, row.name, row.wantExitCode)
			}
			if report := env.statusReport(); !strings.Contains(report, row.wantInReport) {
				t.Errorf("obsync status printed %q over a loop that last turned %s, want %q",
					report, row.name, row.wantInReport)
			}
		})
	}
}

// `docker logs --since 1h` is empty exactly when nothing is wrong, and never
// empty when something is. Both halves are one rule: obsync says nothing on a
// run that changed nothing, and repeats whatever a human is needed for once an
// hour — on a counter over the existing tick rather than on a cadence of its
// own (§9).
func TestAHealthyObsyncIsQuietForHoursAndABrokenOneRepeatsItselfHourly(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.turn()
	env.awaitIdle()

	quietFrom := len(env.saidSoFar())
	for range 12 {
		env.advance(70 * time.Second)
	}
	env.advance(2 * time.Hour)

	if said := env.saidSoFar()[quietFrom:]; said != "" {
		t.Errorf("obsync said %q over hours of a vault nobody was typing into, want silence — a "+
			"log line on a successful no-op run is a defect, because an empty log is a designed "+
			"signal (§9)", said)
	}

	// And now something a human is needed for, which must never go quiet.
	env.theVaultGoesEmpty()
	env.advance(70 * time.Second)

	entered := strings.Count(env.saidSoFar(), "level=ERROR")
	if entered != 1 {
		t.Fatalf("obsync said %d ERRORs entering a full freeze, want exactly one — state entry is "+
			"said once (§9); it said:\n%s", entered, env.saidSoFar())
	}

	// Two more hours, each of which must carry a line and must not carry a
	// line a tick.
	for hour := range 2 {
		before := strings.Count(env.saidSoFar(), "level=ERROR")
		env.advance(61 * time.Minute)
		after := strings.Count(env.saidSoFar(), "level=ERROR")
		if after != before+1 {
			t.Errorf("obsync said %d ERRORs in hour %d of a freeze, want exactly one — a broken "+
				"obsync repeats itself hourly, so `docker logs --since 1h` is never empty while "+
				"something is wrong (§9); it said:\n%s", after-before, hour+1, env.saidSoFar())
		}
	}
	if said := env.said(); !strings.Contains(said, `msg="obsync needs a human"`) {
		t.Errorf("obsync said %q, want the repeat to carry what the entry line carried", said)
	}
}

// The freezes with nowhere to write a status file are the ones that most need
// the log to go on saying so: gates 1, 2, 6 and 8 stand between obsync and a
// repository, so there is no `.git` to write a file into and — for the two §9
// makes log-only carve-outs — no vault obsync may write an attention note in
// either. A channel that says a thing once is one an operator reads an hour
// later and finds empty.
func TestAVaultObsyncCannotBootstrapIsSaidAgainEveryHourAndIsUnhealthyThroughout(t *testing.T) {
	t.Parallel()

	// Gate 2: a non-empty directory that is not a repository, which obsync
	// refuses rather than adopts.
	env := newVaultToBootstrap(t, nil)
	env.clockAnchoredToNow()
	env.seedRemote("main")
	env.writeNote("someone else's notes.md", "a folder that belongs to something else\n")

	env.turn()
	env.awaitIdle()

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d over a directory it refused, want 1 — there is no "+
			"repository to write a status file into, and the absent file is the signal (§9)", got)
	}
	entered := strings.Count(env.saidSoFar(), "level=ERROR")
	if entered != 1 {
		t.Fatalf("obsync said %d ERRORs refusing the directory, want exactly one — state entry is "+
			"said once (§9); it said:\n%s", entered, env.saidSoFar())
	}

	for hour := range 2 {
		before := strings.Count(env.saidSoFar(), "level=ERROR")
		env.advance(61 * time.Minute)
		if after := strings.Count(env.saidSoFar(), "level=ERROR"); after != before+1 {
			t.Errorf("obsync said %d ERRORs in hour %d of refusing the directory, want exactly "+
				"one — a broken obsync repeats itself hourly, and this is the freeze with no "+
				"status file and no attention note to stand in for the log (§9); it said:\n%s",
				after-before, hour+1, env.saidSoFar())
		}
	}

	// And it clears with no restart, which is the other half of what the
	// repeated line promises.
	env.deleteNote("someone else's notes.md")
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d once the directory was emptied, want 0; it said:\n%s",
			got, env.saidSoFar())
	}
}

// Under the keep-both rule a conflict is normal operation, and reserving
// unhealthy for freezes is what keeps that signal meaning "a human must act"
// (§9). There is one thing for them to do, at their own pace, and the attention
// note is where it is said.
func TestAConflictIsNeverAnUnhealthyCheck(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.remoteCommit("Daily/2026-08-24.md", "written on the laptop\n")

	env.wake()

	if copies := env.conflictCopies(); len(copies) != 1 {
		t.Fatalf("obsync wrote %d conflict copies (%v), want the one this test is built on",
			len(copies), copies)
	}
	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d over a conflict obsync kept both sides of, want 0 — "+
			"a conflict is normal operation and never an unhealthy check (§9); it said:\n%s",
			got, env.saidSoFar())
	}
}

// §9's DEBUG row, and it is deliberate rather than generous: beliefs about git
// plumbing are this design's whole risk surface, and a bug report carrying the
// argv is one a maintainer replays by hand. It is safe by construction, because
// the credential is never in a URL or an argv.
//
// The tier decision is on the same row, and it is the only place an aborted run
// says anything at all.
func TestDebugCarriesEveryGitInvocationAndTheTierOfEveryFailure(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_LOG_LEVEL=debug")
	env.someoneElseHoldsTheIndexLock()
	env.writeNote("Daily/2026-08-24.md", "written while somebody else held the index lock\n")

	env.wake()

	said := env.said()
	for _, wanted := range []string{"level=DEBUG msg=git", "argv=", "exit=", "duration=",
		`tier="aborted run"`} {
		if !strings.Contains(said, wanted) {
			t.Errorf("obsync's DEBUG log does not carry %q, want every git invocation with its "+
				"full argv, exit status and duration, and the tier of every failure (§9); it "+
				"said:\n%s", wanted, said)
		}
	}
}

// The status file is written at the end of every wake-up, from inside a freeze
// included — and the one place that has to stop is a vault that is no longer a
// repository. The write creates the directories it needs, so a `.git` that has
// gone would otherwise be put *back*: an empty `.git/obsync/` in a directory
// obsync has just frozen over, which is a trace left somewhere obsync promised
// to stop and a directory gate 2 would then have to reason about.
func TestAVaultThatStoppedBeingARepositoryGetsNoGitDirectoryBack(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.turn()
	env.awaitIdle()

	env.theRepositoryGoes()
	env.advance(70 * time.Second)

	if _, err := os.Stat(filepath.Join(env.vault, ".git")); !os.IsNotExist(err) {
		left, _ := os.ReadDir(filepath.Join(env.vault, ".git"))
		names := make([]string, len(left))
		for i, entry := range left {
			names[i] = entry.Name()
		}
		t.Errorf("obsync put a `.git` back into a vault that had stopped being a repository, "+
			"holding %v (stat: %v) — a freeze stops obsync touching the repository, and writing "+
			"its own record may not be the one thing that recreates one (§7, §9)", names, err)
	}
	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d over a vault that stopped being a repository, "+
			"want 1 — there is nothing to read, and the absent file is the signal (§9); it "+
			"said:\n%s", got, env.saidSoFar())
	}

	// And the freeze self-clears, which is what says the refusal to write was a
	// refusal rather than a failure obsync never recovered from.
	env.theRepositoryComesBack()
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d a tick after the repository came back, want 0 — "+
			"every freeze clears when its cause is repaired, with no restart (§7); it said:\n%s",
			got, env.saidSoFar())
	}
}

// The other half of the middle state, and the one that is easy to miss because
// the deployment *did* work once: an operator narrows the token's scope, or the
// remote's owner does, and from then on every fetch succeeds and every push is
// refused. The vault has permanently stopped being backed up.
//
// The ceiling is what catches it, so the clock it is measured against may not be
// cleared by the half of the network that still works — a run whose fetch got
// through and whose push did not is a failing network half, and treating the
// fetch as evidence would leave this reading healthy for ever and saying
// nothing above debug (§9). Nor may the remote be said to have answered again
// while it is still refusing.
func TestADeploymentThatCanNoLongerPublishStopsBeingHealthyAtTheCeiling(t *testing.T) {
	t.Parallel()

	const credential = "ghp_the_token_that_was_narrowed"
	env, remote := newAuthenticatedVault(t, writeCredential(t, credential+"\n"), credential)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "written while the token could still write\n")
	env.turn()
	env.awaitIdle()

	if got, _ := env.remoteContentYet("Daily/2026-08-24.md"); got == "" {
		t.Fatalf("the deployment never worked, so this is the other row; obsync said:\n%s",
			env.saidSoFar())
	}

	// The scope is narrowed under a running obsync. Fetching goes on working,
	// which is exactly what makes this quiet.
	remote.takesNoWrites()
	env.writeNote("Daily/2026-08-25.md", "written after the token was narrowed\n")
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d one tick after the first refused push, want 0 — "+
			"an established deployment gets the day's grace, and one failed push is not a "+
			"verdict (§9); it said:\n%s", got, env.saidSoFar())
	}

	quietFrom := len(env.saidSoFar())
	env.advance(backoffCeiling + 2*time.Hour)

	if got := env.healthcheck(); got != 1 {
		t.Errorf("obsync healthcheck exited %d after a day of a vault it cannot publish, want 1 — "+
			"the fetch still working is not obsync working, and a vault that has stopped being "+
			"backed up may not read as healthy for ever (§9); it said:\n%s", got, env.saidSoFar())
	}
	if said := env.saidSoFar()[quietFrom:]; !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q over a day of pushes the remote refused, want a human told — "+
			"`docker logs --since 1h` is never empty while something is wrong (§9)", said)
	}
	if said := env.saidSoFar(); strings.Contains(said, `msg="the remote answered again"`) {
		t.Errorf("obsync said the remote answered again while every push was still being "+
			"refused; it said:\n%s", said)
	}

	// And it is a health verdict rather than a retry limit here too: obsync
	// has gone on pushing throughout, so widening the scope is enough.
	remote.takesWritesAgain()
	env.advance(20 * time.Minute)

	if got, _ := env.remoteContentYet("Daily/2026-08-25.md"); got != "written after the token was narrowed\n" {
		t.Errorf("the remote holds %q once the token could write again, want the note obsync had "+
			"been holding; obsync said:\n%s", got, env.saidSoFar())
	}
	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d once a push landed again, want 0; it said:\n%s",
			got, env.saidSoFar())
	}
	if said := env.said(); !strings.Contains(said, `msg="the remote answered again"`) {
		t.Errorf("obsync said %q, want the recovery said once, and only now that it is true (§9)", said)
	}
}

// The two signal subcommands answer *does this need a human?* out of obsync's
// own record of itself, and they may not answer it out of anything else. A
// credential file that turns unreadable is §8's self-healing tier rather than a
// config error — PATs get rotated, and a rotation that unlinks before it writes
// leaves a window — so a healthcheck that stats it would call a running,
// healthy obsync unhealthy and invite the restart that turns the rotation into
// a crash loop, since a *restarted* obsync exits 1 on the same file.
func TestARotatingCredentialFileIsNeverAnUnhealthyCheck(t *testing.T) {
	t.Parallel()

	const credential = "ghp_the_token_being_rotated"
	tokenFile := writeCredential(t, credential+"\n")
	env, _ := newAuthenticatedVault(t, tokenFile, credential)
	env.clockAnchoredToNow()
	env.writeNote("Daily/2026-08-24.md", "a deployment that is working\n")
	env.turn()
	env.awaitIdle()

	if got := env.healthcheck(); got != 0 {
		t.Fatalf("obsync healthcheck exited %d on a working deployment, want 0; it said:\n%s",
			got, env.saidSoFar())
	}

	if err := os.Remove(tokenFile); err != nil {
		t.Fatalf("taking the credential file away mid-rotation: %v", err)
	}
	env.advance(70 * time.Second)

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d while the credential file was mid-rotation, want "+
			"0 — the loop is turning and its record of itself is fresh, and a credential turning "+
			"unreadable later is the self-healing tier rather than a config error (§7, §8)", got)
	}
	if report := env.statusReport(); !strings.Contains(report, "needs a human: no") {
		t.Errorf("obsync status printed %q mid-rotation, want the verdict derived from obsync's "+
			"own record rather than from a surface this subcommand is not asking about (§9)", report)
	}
}

// The subcommands run in a process of their own, holding none of the isolation
// a Repo carries — so the one git they run has to be pinned as deaf to an
// ambient configuration as every git the loop runs already is. HOME is not
// hypothetical: it is exactly where §8 says an ssh key arrives, so a
// `~/.gitconfig` beside one is an ordinary mount rather than an exotic one.
func TestAnAmbientGitconfigCannotChangeWhatASubcommandsGitMeans(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.clockAnchoredToNow()
	env.wake()

	if got := env.healthcheck(); got != 0 {
		t.Fatalf("obsync healthcheck exited %d after a run that worked, want 0; it said:\n%s",
			got, env.saidSoFar())
	}

	// A `.gitconfig` git cannot parse, in the HOME the subcommand inherits.
	// The loop is unaffected by construction — a Repo pins GIT_CONFIG_GLOBAL
	// at its own private file — so this is only ever a question about the
	// subcommand's git.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core\nbroken =\n"), 0o600); err != nil {
		t.Fatalf("writing the ambient gitconfig: %v", err)
	}
	env.home = home

	if got := env.healthcheck(); got != 0 {
		t.Errorf("obsync healthcheck exited %d with a `~/.gitconfig` git cannot parse, want 0 — "+
			"the vault is healthy and obsync's record of it is fresh; measured at both matrix "+
			"points, an unparseable global config fails `rev-parse --git-path` with exit 128", got)
	}
	if report := env.statusReport(); !strings.Contains(report, "needs a human: no") {
		t.Errorf("obsync status printed %q with an ambient `~/.gitconfig`, want the verdict it "+
			"gives without one", report)
	}
}

// clockAnchoredToNow puts obsync's injected clock at the instant this test
// began, which is what makes §9's two processes comparable.
//
// The suite's clock is otherwise at a fixed instant so that nothing depends on
// the day a test runs — and that is exactly wrong here, because the subcommands
// are a *second process* reading obsync's own record of itself against the wall
// clock, the way Docker's HEALTHCHECK and a human's `docker exec` do. In a
// container the two clocks are one; here they are one only when a test says so.
//
// Driving the clock forward afterwards is still how time passes: it moves the
// loop's own view into the future, which leaves the file looking newer than
// now rather than older, so nothing but this anchoring can make a file stale.
func (e *vaultEnv) clockAnchoredToNow() {
	e.t.Helper()
	e.clockAnchoredTo(time.Now())
}

// clockAnchoredTo is clockAnchoredToNow at a chosen instant, which is how a
// test reaches a loop that stopped turning some minutes ago without sleeping
// through them.
func (e *vaultEnv) clockAnchoredTo(instant time.Time) {
	e.t.Helper()

	if e.turning {
		e.t.Fatal("this loop is already turning; its clock is anchored before it starts, never " +
			"under a run in flight")
	}
	e.clock.mu.Lock()
	defer e.clock.mu.Unlock()
	e.clock.start, e.clock.now = instant, instant
}

// healthcheck is `obsync healthcheck` over this vault, run the way the image's
// HEALTHCHECK runs it: a process of its own, inheriting the container's
// environment, answering with an exit status and nothing else.
func (e *vaultEnv) healthcheck() int {
	e.t.Helper()

	stdout, stderr, exitCode := runObsync(e.t, e.containerEnviron(), "healthcheck")
	if stdout != "" || stderr != "" {
		e.t.Errorf("obsync healthcheck wrote %q to stdout and %q to stderr, want silence — Docker "+
			"reads the exit status and nothing reads the output (§10)", stdout, stderr)
	}
	if exitCode != 0 && exitCode != 1 {
		e.t.Fatalf("obsync healthcheck exited %d, want 0 or 1 (§10); it wrote %q", exitCode, stderr)
	}
	return exitCode
}

// statusReport is `obsync status` over this vault, run the way a suspicious
// operator runs it through `docker exec`. It exits 0 whatever it finds, which
// this asserts on every call rather than in one test: a subcommand whose job is
// to answer a question has answered it (§10).
func (e *vaultEnv) statusReport() string {
	e.t.Helper()

	stdout, stderr, exitCode := runObsync(e.t, e.containerEnviron(), "status")
	if exitCode != 0 {
		e.t.Errorf("obsync status exited %d, want 0 always (§10); it wrote %q", exitCode, stderr)
	}
	if stderr != "" {
		e.t.Errorf("obsync status wrote %q to stderr, want stdout left to the subcommands that "+
			"print there (§9)", stderr)
	}
	if !strings.Contains(stdout, stampedVersion) {
		e.t.Errorf("obsync status printed %q, want the build version — the version identifies the "+
			"bytes of the image an operator pinned (§10, §12)", stdout)
	}
	return stdout
}

// containerEnviron is the block a subcommand of this obsync is started with:
// the one the loop was configured from, plus what any process in a container
// has. PATH is not decoration — it is how the subcommand finds the same git the
// loop is driving, which at the matrix's floor point is not the same git as the
// one first on a developer's path.
func (e *vaultEnv) containerEnviron() []string {
	home := e.home
	if home == "" {
		home = os.Getenv("HOME")
	}
	return append(append([]string{}, e.environ...),
		"PATH="+os.Getenv("PATH"),
		"HOME="+home,
	)
}
