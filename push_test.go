package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// §7's push disposition table, driven at seam 1 against a real bare remote: a
// real `pre-receive` hook and a real `receive.maxInputSize` produce every
// verdict the table keys on, offline and with no flake.
//
// The distinction the whole table rests on is git's own: `git-push(1)` defines
// the `--porcelain` `<summary>` as a closed enum and the trailing `(<reason>)`
// as "a human-readable explanation". obsync branches on the enum and relays
// the parenthetical.

// A remote rejection is a verdict rather than a failure, so it escalates on the
// first occurrence: waiting cannot repair it, because the party whose opinion is
// the whole question has already answered.
func TestARemoteThatRejectsThePushStopsObsyncOnTheFirstOccurrence(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive", "#!/bin/sh\necho 'policy: Attachments/ is not allowed here' >&2\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "captured anyway\n")

	env.wake()

	said := env.said()
	if !strings.Contains(said, `freeze="remote rejection"`) {
		t.Errorf("obsync said %q about a push the remote rejected, want a remote-rejection freeze "+
			"on the first occurrence (§7)", said)
	}
	// The remote's own words, relayed verbatim: the parenthetical git carries
	// as a machine-adjacent reason, and what the remote said for itself.
	if !strings.Contains(said, "pre-receive hook declined") {
		t.Errorf("obsync said %q, want git's own (<reason>) relayed verbatim (§7)", said)
	}
	if !strings.Contains(said, "policy: Attachments/ is not allowed here") {
		t.Errorf("obsync said %q, want the remote's own words relayed to the human who has to act "+
			"on them (§7)", said)
	}
	if got, want := env.commitsOn(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a network freeze leaves the local half "+
			"committing (§7)", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits, want %s — the push was rejected", got, want)
	}
}

// A push that lost a race is not a verdict about anything: somebody else landed
// a commit on the remote between this run's fetch and this run's push. Nobody is
// told, and the next run fetches, merges and publishes both sides — which is
// §3's designed-for case rather than an anomaly.
func TestAPushThatLostARaceIsRetriedNextRunAndNobodyIsTold(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
	env.theOtherDevicePushesInsideTheRun()

	env.turn()
	env.awaitIdle()

	if said := env.saidSoFar(); strings.Contains(said, "level=ERROR") || strings.Contains(said, "level=WARN") {
		t.Errorf("obsync said %q about a push that lost a race, want nothing above debug — an "+
			"aborted run is not news (§7)", said)
	}
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote holds the vault's note although the push lost the race, want a push " +
			"that did not land")
	}

	// The other device stops pushing, and the next tick is an ordinary run.
	env.duringSettle(nil)
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written in the vault\n" {
		t.Errorf("the remote holds %q at the vault's note, want the next run to have fetched, "+
			"merged and pushed what the lost race left behind", got)
	}
	if got := env.vaultFile("Laptop/note 2.md"); got != "written on the laptop\n" {
		t.Errorf("the vault holds %q at the laptop's second note, want the commit that won the "+
			"race merged in rather than lost", got)
	}
}

// A rejection is retried once an hour rather than on the 15-minute backoff
// ceiling, and the retry is a whole network half — so upstream changes still
// arrive while the freeze stands, just hourly. Fifteen minutes is for a remote
// that might come back; an hour is for one that has already answered.
func TestARejectionIsRetriedHourlyAndTheRetryIsAWholeNetworkHalf(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive", refusesObsyncsCommits)
	env.writeNote("Daily/2026-08-24.md", "written in the vault\n")

	env.turn()
	env.awaitIdle()
	env.remoteCommit("Laptop/plan.md", "written on the laptop\n")

	// Past the backoff's own ceiling, which is what obsync would be waiting if
	// a rejection were an ordinary network failure.
	env.advance(16 * time.Minute)

	if env.vaultHoldsYet("Laptop/plan.md") {
		t.Error("the vault holds the laptop's note within the 15-minute backoff ceiling, want a " +
			"rejection retried hourly rather than on the backoff (§7)")
	}

	// Past the hour.
	env.advance(45 * time.Minute)

	if got := env.vaultFileYet("Laptop/plan.md"); got != "written on the laptop\n" {
		t.Errorf("the vault holds %q at the laptop's note after an hour, want the hourly retry to "+
			"be a whole network half so upstream changes still arrive (§7)", got)
	}
	if env.remoteHoldsYet("Daily/2026-08-24.md") {
		t.Error("the remote holds the vault's note, want the hourly retry's push rejected again")
	}
	if got, want := strings.Count(env.saidSoFar(), "level=ERROR"), 1; got != want {
		t.Errorf("obsync logged %d ERRORs over an hour of rejection, want %d — state entry is "+
			"said exactly once (§9); it said:\n%s", got, want, env.saidSoFar())
	}

	// The human changes the remote's rule, which is the only repair there is.
	env.removeHook("pre-receive")
	env.advance(61 * time.Minute)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written in the vault\n" {
		t.Errorf("the remote holds %q, want the commit it had refused, published once the rule "+
			"that refused it was changed", got)
	}
	if said := env.said(); !strings.Contains(said, `msg="the freeze cleared and obsync is syncing with the remote again" freeze="remote rejection"`) {
		t.Errorf("obsync said %q, want the rejection freeze cleared once the remote took the "+
			"push (§7, §9)", said)
	}
}

// The local half keeps committing while a rejection stands, and there is no cap
// on how far the vault runs ahead: the rejected pack grows and vault-side
// commits accumulate unpublished for as long as the human takes. That is the
// fail-open-locally rule working rather than a defect.
func TestTheVaultKeepsBeingCapturedWhileARejectionStands(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive", "#!/bin/sh\nexit 1\n")

	env.turn()
	env.awaitIdle()
	for day := 24; day < 30; day++ {
		env.writeNote("Daily/2026-08-"+strconv.Itoa(day)+".md", "written while the remote refused\n")
		env.advance(61 * time.Minute)
	}

	if got, want := env.commitsOn(env.vault), "7"; got != want {
		t.Errorf("the vault holds %s commits, want %s — the local half keeps committing while a "+
			"rejection stands, and nothing caps how far it runs ahead (§7)", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits, want %s — obsync offers no way past the commit the "+
			"remote refused", got, want)
	}
	if got := env.vaultFile("Daily/2026-08-29.md"); got != "written while the remote refused\n" {
		t.Errorf("the vault holds %q at the last note written under the freeze, want every byte "+
			"of it", got)
	}
}

// refusesObsyncsCommits is a real `pre-receive` hook enforcing a real policy:
// this remote takes the human's own commits from their other devices and
// refuses obsync's. It is what lets a test watch a rejection stand while
// upstream changes go on arriving over the top of it.
const refusesObsyncsCommits = `#!/bin/sh
while read -r old new ref; do
	if git log -1 --format=%an "$new" | grep -q obsync; then
		echo "policy: obsync may not push here" >&2
		exit 1
	fi
done
exit 0
`

// A wrong or expired credential is a labelled network-half failure on the
// ordinary backoff and never a freeze (§7, §8). PATs expire and get rotated, so
// a latched auth freeze would take away self-recovery for nothing — and the
// label is the whole of what its reporting adds, because git's own words may
// name a failure and only persistence may escalate one.
func TestACredentialTheRemoteRefusesIsLabelledAndNeverAFreeze(t *testing.T) {
	t.Parallel()

	env, _ := newAuthenticatedVault(t,
		writeCredential(t, "ghp_the_expired_token\n"), "ghp_the_live_token", "OBSYNC_LOG_LEVEL=debug")
	env.writeNote("Daily/2026-08-24.md", "written while the token was expired\n")

	env.wake()

	said := env.said()
	if strings.Contains(said, "freeze=") {
		t.Errorf("obsync said %q about a credential the remote refused, want no freeze of any "+
			"kind — a token that expires heals by being replaced (§7)", said)
	}
	if !strings.Contains(said, "this looks like a credential the remote would not accept") {
		t.Errorf("obsync said %q, want git's own words naming the failure so an operator is "+
			"looking at their token rather than at their vault (§7)", said)
	}
	if !strings.Contains(said, `tier="aborted run"`) {
		t.Errorf("obsync said %q, want the aborted run tier: the remote returned no verdict, so "+
			"obsync has been told nothing and the ordinary backoff retries it (§7)", said)
	}
	if got, want := env.commitsOn(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a credential is a network-half failure "+
			"and the local half keeps committing (§2)", got, want)
	}
}

// The other rejection §7 names, and the one no per-file ceiling predicts: a
// real `receive.maxInputSize` on the bare remote, enforced incrementally inside
// index-pack. git reports it as `[remote rejected] (unpacker error)` — the same
// enum member as a hook declining, and therefore the same disposition, which is
// the whole point of branching on the enum rather than on what it means.
func TestAPackOverTheRemotesInputSizeIsARejectionLikeAnyOther(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.remoteRefusesPacksOver(512)
	env.writeNote("Attachments/diagram.png", strings.Repeat("not really a png\n", 4096))

	env.wake()

	said := env.said()
	if !strings.Contains(said, `freeze="remote rejection"`) {
		t.Errorf("obsync said %q about a pack the remote would not unpack, want the same "+
			"remote-rejection freeze a declining hook gets (§7)", said)
	}
	if !strings.Contains(said, "unpacker error") {
		t.Errorf("obsync said %q, want git's own (<reason>) for this row relayed verbatim (§7)", said)
	}
	if !strings.Contains(said, "pack exceeds maximum allowed size") {
		t.Errorf("obsync said %q, want the remote's own words, which are the only place the "+
			"reason a human can act on ever appears (§7)", said)
	}
}

// **obsync relays, and never diagnoses.** The remedy is the same sentence
// whatever the remote said, and the only thing that differs between two
// rejections is the remote's own words — obsync adds no reading of them, and it
// cannot name the offending path even when the remote's prose does.
func TestObsyncRelaysWhatTheRemoteSaidAndAddsNoReadingOfIt(t *testing.T) {
	t.Parallel()

	remedyFor := func(t *testing.T, rejection string) string {
		t.Helper()

		env := newVault(t)
		env.installHook("pre-receive", "#!/bin/sh\necho '"+rejection+"' >&2\nexit 1\n")
		env.writeNote("Daily/2026-08-24.md", "written in the vault\n")
		env.wake()

		said := env.said()
		if !strings.Contains(said, rejection) {
			t.Fatalf("obsync said %q, want the remote's sentence %q relayed verbatim", said, rejection)
		}
		_, remedy, found := strings.Cut(said, "remedy=")
		if !found {
			t.Fatalf("obsync said %q with no remedy in it", said)
		}
		return remedy
	}

	named := remedyFor(t, "error: GH001: Large files detected. File: Attachments/video.mov is 214.00 MB")
	generic := remedyFor(t, "error: this branch is protected")

	if named != generic {
		t.Errorf("obsync's remedy for a rejection naming a file was\n%s\nand for one naming "+
			"nothing was\n%s\nwant one sentence whatever the remote said: obsync relays and never "+
			"diagnoses (§7, README's never-list)", named, generic)
	}
	if strings.Contains(generic, "Attachments/video.mov") {
		t.Error("obsync's remedy names a path the remote mentioned, want it never to name the " +
			"offending path — it has only prose to get one from (§7)")
	}
}
