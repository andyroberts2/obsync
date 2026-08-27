// Package status is obsync's status file and the health verdict read out of
// it: the private record of the loop's own state, rewritten at the end of every
// wake-up, and the one answer `obsync healthcheck` and `obsync status` are
// built on (§9).
//
// Two words, kept apart on purpose, because CONTEXT.md keeps them apart. The
// **status file** is this file — where liveness lives, so the log never has to
// carry it. **Health** is obsync's answer to exactly one question, *does this
// need a human?*, and nothing here calls that a status. The package is named
// after the file and after the subcommand that prints it, never after the
// verdict.
//
// The verdict is derived from facts rather than written down as a conclusion,
// and it is derived here rather than at each reader, so that the loop repeating
// itself hourly, the healthcheck Docker calls and the report a suspicious
// operator runs cannot disagree about what obsync's state means. What the file
// carries beside those facts is the two windows the verdict needs — how long
// the loop may go without writing, and how long an unreachable remote stays
// healthy — because the loop is where those constants live and a reader that
// re-declared them would be a second definition free to drift from the first.
//
// The layout is private and nothing outside obsync should parse it: `.git/obsync/`
// is a declared owned path, what obsync writes inside it is not, and the
// subcommands are the interface (§10).
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/andyroberts2/obsync/internal/git"
)

// File is the status file's contents: what the loop knew about itself at the
// end of its last wake-up.
//
// Every field is a fact the loop held rather than a conclusion it drew, and
// Health is the only thing that draws one. The times are the loop's own clock
// — the injected one (§21's testing decisions) — and they are compared against
// the clock of whichever process reads them, which in the container is the same
// wall clock.
type File struct {
	// WrittenAt is the liveness timestamp, refreshed by every wake-up whatever
	// the run turned out to be. Liveness is a fact about the loop rather than
	// about the outcome: a run that keeps losing to a third writer's
	// index.lock is aborting, which is not news, but it is unambiguously
	// alive. Separating the two is what makes staleness measure precisely one
	// thing — has the loop stopped turning — which is the only failure mode
	// nothing else can detect (§9).
	WrittenAt time.Time `json:"written_at"`

	// Branch is the tracked branch, carried so the report names what obsync is
	// syncing rather than making an operator infer it.
	Branch string `json:"branch,omitempty"`

	// The two freezes obsync is in, or nil. They carry the fact and the remedy
	// the freeze was entered with, so that the hourly repeat and the report say
	// what the entry line said rather than a second wording of it.
	FullFreeze    *Freeze `json:"full_freeze,omitempty"`
	NetworkFreeze *Freeze `json:"network_freeze,omitempty"`

	// PushAttempted and LastPush are §9's three states rather than two.
	// Attempted with no LastPush is *attempted, never succeeded* — nobody has
	// ever seen this deployment work — and that is what makes a wrong-scoped
	// token findable without a startup probe. Not attempted at all is *never
	// attempted*: nothing has changed yet, which is not a failure and is never
	// reported as one.
	PushAttempted bool      `json:"push_attempted"`
	LastPush      time.Time `json:"last_push"`

	// LastCommit is when the local half last captured the vault. It answers
	// the question a suspicious operator actually has — *has this been
	// working* — and it is a fact rather than a verdict: a vault nobody has
	// typed into for a week has nothing to commit and is perfectly healthy.
	LastCommit time.Time `json:"last_commit"`

	// NetworkFailingFor is how long the network half had been failing when this
	// was written, and zero while it is working. It is what the backoff ceiling
	// is measured against: only elapsed time separates a remote that will come
	// back from one that will not.
	//
	// It is an elapsed duration rather than the moment it started, and that is
	// the one place this file is careful about *whose* clock a number is on.
	// obsync's clock is injected (§21's testing decisions) and a reader is a
	// different process; a verdict that subtracted one clock's instant from
	// another's would be reading a difference between two frames. So the loop
	// measures this against its own clock and the reader compares the answer
	// against a threshold, which is frame-free. The one comparison that is
	// deliberately across the two is staleness, because real elapsed time since
	// obsync last spoke is precisely what it asks about.
	NetworkFailingFor time.Duration `json:"network_failing_for"`

	// StaleAfter and BackoffCeiling are the loop's own constants, carried so
	// that a reader in another process asks the same question the loop does
	// without holding a second copy of the number.
	StaleAfter     time.Duration `json:"stale_after"`
	BackoffCeiling time.Duration `json:"backoff_ceiling"`
}

// Freeze is one live freeze as the run that entered it described it: the name
// an operator reads, the conclusive fact behind it, the remedy, and when obsync
// entered it.
type Freeze struct {
	Name   string    `json:"name"`
	Fact   string    `json:"fact"`
	Remedy string    `json:"remedy"`
	Since  time.Time `json:"since"`
}

// Health is obsync's answer to exactly one question: does this need a human?
//
// Deliberately not "is everything working" (§9). A remote that is down and
// backing off is behaving as designed and is healthy for a day; a remote that
// has *rejected* a push is unhealthy at once. The two are opposites and both
// are surprising, which is why the docs carry them.
type Health struct {
	// NeedsHuman is the whole verdict. Everything else is what to tell them.
	NeedsHuman bool
	// State is what a human is needed for, named as the thing they have to
	// deal with. Empty when they are not needed.
	State  string
	Fact   string
	Remedy string
}

// Health is the verdict this file answers for at now.
//
// Both lists are closed (§9). Unhealthy: any full freeze, any network freeze
// including a remote rejection immediately, a push attempted but never once
// succeeded, a merely-unreachable remote past the backoff ceiling, and a status
// file staler than its window. Healthy: everything else — including an
// unreachable remote inside the ceiling, an aborted run, and any amount of
// backoff.
//
// The order is the order an operator would want to be told in, and staleness
// comes first because it is the one row the reader establishes for itself: a
// file naming a freeze that stopped being refreshed an hour ago is a loop that
// stopped turning, and saying "remote rejection" about it would be reporting
// the last thing obsync knew as though it were the current one.
//
// A conflict is nowhere in either list, and that is a decision rather than an
// omission: under the keep-both rule a conflict is normal operation, and
// reserving unhealthy for freezes keeps this signal meaning "a human must act"
// (§9).
func (f File) Health(now time.Time) Health {
	switch {
	case f.stale(now):
		return Health{
			NeedsHuman: true,
			State:      stateLoopStopped,
			Fact: "obsync last finished a sync run " + since(f.WrittenAt, now) +
				", and it writes this file at the end of every wake-up whatever the run turned " +
				"out to be",
			Remedy: "look at the container's log for what it was doing when it stopped, and at the " +
				"vault's disk. obsync refuses no work quietly — a freeze keeps ticking and says " +
				"so — so a loop that has stopped writing has stopped turning" + git.SelfClearing,
		}
	case f.FullFreeze != nil:
		return f.FullFreeze.health()
	case f.NetworkFreeze != nil:
		return f.NetworkFreeze.health()
	case f.PushAttempted && f.LastPush.IsZero():
		return Health{
			NeedsHuman: true,
			State:      stateNeverPushed,
			Fact: "obsync has tried to push " + f.branchName() + " and no push has ever " +
				"succeeded since it started",
			Remedy: neverPushedRemedy,
		}
	case f.unreachablePastTheCeiling():
		return Health{
			NeedsHuman: true,
			State:      stateNetworkFailingPastTheCeiling,
			Fact: "obsync's network half has been failing for " + plainly(f.NetworkFailingFor) +
				", which is past the " + plainly(f.BackoffCeiling) + " a remote that is merely " +
				"down stays healthy for",
			Remedy: pastTheCeilingRemedy,
		}
	}
	return Health{}
}

// The states a human is needed for that are not a freeze's own name. A freeze
// carries the name it was entered under, because the log line, the report and
// the attention note (#38) should all name one thing.
const (
	stateLoopStopped = "the sync loop has stopped turning"
	stateNeverPushed = "a push has been attempted and has never once succeeded"
	// Named after what obsync observed rather than after the commonest cause of
	// it. Everything on the ordinary backoff arrives here, and §7 puts more than
	// a dead host on that tier: a credential the remote will not take fails the
	// same way, and calling that "unreachable" would send an operator to look at
	// a network with nothing wrong with it.
	stateNetworkFailingPastTheCeiling = "the network half has been failing past the backoff ceiling"
	stateNoRecord                     = "obsync cannot read its own record of itself"
)

// neverPushedRemedy is the middle of §9's three states, and it is the reason
// obsync runs no startup push-capability probe: `push --dry-run` cannot prove
// what matters, and the failure it would catch is this one, found at the first
// real push instead (§8).
const neverPushedRemedy = "check what the credential is allowed to do. A token that can read but " +
	"not write clones and fetches perfectly and fails only here, so a deployment that has never " +
	"worked looks exactly like one that has nothing to do — which is why obsync says this at the " +
	"first push rather than at the first time you look. docs/credentials.md states the minimum " +
	"scope for each remote. Nothing is lost meanwhile: the vault goes on being committed locally, " +
	"and the first push that lands clears this" + git.SelfClearing

// pastTheCeilingRemedy is the backoff ceiling, and the first thing it says is
// that obsync has not given up — the ceiling is a health verdict rather than a
// retry limit (§9).
//
// It names three places to look rather than one, because everything on the
// ordinary backoff arrives here and obsync cannot tell which: a day of a
// network half that never got an answer it could act on is a dead host, a
// container that cannot reach it, or a credential the far end will not take.
// That is where to look, not what is wrong — obsync relays and never diagnoses
// (§7).
const pastTheCeilingRemedy = "look at the remote, at this container's network, and at what the " +
	"credential is allowed to do — a day of getting no answer obsync could act on is any of the " +
	"three, and obsync cannot tell them apart from here. obsync has not " +
	"stopped trying and will not: it keeps backing off and retrying past this point, and only the " +
	"health verdict changed, because waiting is the correct behaviour and stays correct. Nothing " +
	"is lost meanwhile — the vault goes on being committed locally, and everything obsync has " +
	"committed goes out on the first fetch and push that get through" + git.SelfClearing

func (f Freeze) health() Health {
	return Health{NeedsHuman: true, State: f.Name, Fact: f.Fact, Remedy: f.Remedy}
}

// stale reports whether the loop has stopped turning: the file was last written
// longer ago than the loop's own staleness window, which is five ticks (§9).
//
// A zero window means a file obsync did not write this way, which is not a
// question this can answer, so it is not asked.
func (f File) stale(now time.Time) bool {
	return f.StaleAfter > 0 && now.Sub(f.WrittenAt) > f.StaleAfter
}

// unreachablePastTheCeiling is the backoff ceiling: a remote that has merely
// gone quiet stops being healthy once it has been quiet for longer than a
// deployment could reasonably call weather.
func (f File) unreachablePastTheCeiling() bool {
	return f.BackoffCeiling > 0 && f.NetworkFailingFor > f.BackoffCeiling
}

func (f File) branchName() string {
	if f.Branch == "" {
		return "the tracked branch"
	}
	return f.Branch
}

// Unavailable is the health of an obsync whose record of itself cannot be got
// to at all, and it is the reason siting the file under `.git/obsync/` makes
// §9's failure modes fall out rather than need coding: the mount drops and the
// file goes with it, gate 2 refuses a directory that is not a repository and
// there is no `.git` to write into, and a fresh container has not run yet. All
// of them read as unhealthy, correctly, with no special case — as does an
// obsync that exited on a configuration it could not use, which is why the
// reader checks none of that surface itself (main.look): it never started, so
// it never wrote one.
func Unavailable(problem error) Health {
	return Health{
		NeedsHuman: true,
		State:      stateNoRecord,
		Fact:       problem.Error(),
		Remedy: "check that obsync's configuration is the one you meant, that the vault is mounted " +
			"where obsync was told it is, that it holds a git repository, and that this container " +
			"has been running long enough to have finished a sync run. obsync writes this file " +
			"inside the vault's `.git`, so a vault that is not there and a directory that is not a " +
			"repository both arrive here" + git.SelfClearing,
	}
}

// Of is what a subcommand asks: the status file at path, and the health it
// answers for as of now.
//
// A file that is not there, cannot be read, or does not parse is unhealthy
// rather than an error to report — the question was never "did this read
// succeed", it was "does this need a human", and every way of failing to read
// obsync's own record of itself is a yes.
func Of(path string, now time.Time) (File, Health) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The commonest of the three, and the one an operator meets on a
			// first deployment, so it says what it is rather than quoting a
			// syscall at them.
			return File{}, Unavailable(fmt.Errorf("obsync has not written one yet at %s", path))
		}
		return File{}, Unavailable(err)
	}
	var file File
	if err := json.Unmarshal(content, &file); err != nil {
		return File{}, Unavailable(fmt.Errorf("%s: %w", path, err))
	}
	return file, file.Health(now)
}

// Encode is the bytes the loop writes. Indented because the one human who ever
// reads it directly is debugging obsync itself, and the file is small enough
// that nothing else cares.
func (f File) Encode() ([]byte, error) {
	content, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// Report is `obsync status`: what a suspicious operator runs through
// `docker exec`, and the most direct answer to "has this been working" (§9).
//
// It prints the verdict first and the facts under it, because the verdict is
// the question they came with. It always prints the build version, which is the
// other thing this subcommand exists for — the version identifies the bytes of
// the image they pinned (§12).
func Report(version, vaultPath string, file File, health Health, now time.Time) string {
	var report strings.Builder
	fmt.Fprintf(&report, "obsync %s\n\n", version)

	if health.NeedsHuman {
		fmt.Fprintf(&report, "needs a human: yes — %s\n", health.State)
		fmt.Fprintf(&report, "        fact: %s\n", health.Fact)
		fmt.Fprintf(&report, "      remedy: %s\n", health.Remedy)
	} else {
		report.WriteString("needs a human: no\n")
	}

	fmt.Fprintf(&report, "        vault: %s\n", vaultPath)
	if file.Branch != "" {
		fmt.Fprintf(&report, "       branch: %s\n", file.Branch)
	}
	if !file.WrittenAt.IsZero() {
		fmt.Fprintf(&report, "     last run: %s\n", at(file.WrittenAt, now))
		fmt.Fprintf(&report, "  last commit: %s\n",
			atOrNever(file.LastCommit, now, "obsync has committed nothing yet"))
		fmt.Fprintf(&report, "    last push: %s\n", lastPush(file, now))
	}
	// The freezes are printed even when they are not what the verdict named,
	// because a network freeze standing under a full one is a second thing to
	// repair and an operator who fixes only the one they were told about would
	// find obsync still stopped.
	printFreeze(&report, "full freeze", file.FullFreeze, health, now)
	printFreeze(&report, "network freeze", file.NetworkFreeze, health, now)
	return report.String()
}

func printFreeze(report *strings.Builder, label string, freeze *Freeze, health Health, now time.Time) {
	if freeze == nil || freeze.Name == health.State {
		return
	}
	fmt.Fprintf(report, "\n%s: %s, since %s\n", label, freeze.Name, at(freeze.Since, now))
	fmt.Fprintf(report, "        fact: %s\n", freeze.Fact)
	fmt.Fprintf(report, "      remedy: %s\n", freeze.Remedy)
}

// lastPush is §9's three states said in the one place a human reads them, so
// that "never" is never ambiguous between *nothing has changed yet* and *nobody
// has ever seen this work*.
func lastPush(file File, now time.Time) string {
	switch {
	case !file.LastPush.IsZero():
		return at(file.LastPush, now)
	case file.PushAttempted:
		return "never — obsync has tried and no push has succeeded"
	default:
		return "never — obsync has had nothing to push"
	}
}

// atOrNever is a moment that may not have happened yet: when it was, or the
// word an operator reads instead, which says which kind of never this is.
func atOrNever(moment, now time.Time, nothing string) string {
	if moment.IsZero() {
		return "never — " + nothing
	}
	return at(moment, now)
}

// at is a moment as both of the things a human wants from one: when it was, and
// how long ago that is.
func at(moment, now time.Time) string {
	return moment.UTC().Format(time.RFC3339) + " (" + since(moment, now) + ")"
}

// since is how long ago a moment was, rounded to the second — obsync's own
// cadence is measured in tens of seconds at its finest, so anything under one
// is noise in a line a human reads.
func since(moment, now time.Time) string {
	elapsed := now.Sub(moment)
	if elapsed < 0 {
		// The reader's clock is behind the writer's, which in a container
		// means somebody stepped one of them. Saying "in 4s" would be worse
		// than saying nothing about how long.
		return "just now"
	}
	return plainly(elapsed) + " ago"
}

func plainly(d time.Duration) string {
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}
