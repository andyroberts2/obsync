package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/andyroberts2/obsync/internal/config"
)

// The push, and what the remote did with it (§7, #35).
//
// A push is the one command obsync runs that carries back a *verdict* rather
// than an answer. A fetch that failed leaves obsync having been told nothing,
// which is what waiting repairs; a push the remote received, evaluated and
// declined has been answered by the party whose opinion is the whole question,
// and a second identical answer carries nothing new.
//
// git ships that distinction in a machine field, which is the only reason
// obsync can make it: `git-push(1)` defines the `--porcelain` `<summary>` as a
// closed enum and the trailing `(<reason>)` as "a human-readable explanation".
// **obsync branches on the enum and relays the parenthetical**, which is one of
// the two sharpenings §1 makes to "never parse git output meant for humans" —
// neither of them an exception, because both are reading what git documented as
// machine-readable and treating the rest as words for a human.
//
// The table, measured at both matrix points, 2.38.5 and 2.52.0, and the two
// agree exactly:
//
//	pushed                        exit 0    ` ` / `*` / `=`                          success
//	lost the race (non-ff)        exit 1    `!` `[rejected] (fetch first)`           aborted run
//	pre-receive hook declined     exit 1    `!` `[remote rejected] (pre-receive hook declined)`  network freeze
//	receive.maxInputSize exceeded exit 1    `!` `[remote rejected] (unpacker error)` network freeze
//	remote failed to report       exit 1    `!` `[remote failure] (…)`               network backoff
//	host unreachable / auth       exit 128   stdout empty                            network backoff
//	git refused it on this side   exit 1     stdout empty                            failed run
//
// The second-to-last row is the whole of what exit 128 means here: **no verdict
// was ever returned**, so obsync has been told nothing and waiting is what
// repairs it. The last row is §7's table read exactly rather than nearly: a push
// can produce no ref line without ever having asked the remote anything — the
// human's own `pre-push` hook declining — and that is not a fact about the
// remote and not one waiting repairs, so it is a run that failed rather than an
// aborted one. neverReachedTheRemote is where the two are separated.

// Push sends the tracked branch to the remote, and is the one network command
// in this build that writes.
//
// The refspec is written out in full and both sides are named: obsync pushes
// one branch in each direction (§3) and never sets an upstream, because -u
// writes the vault's .git/config, which belongs to the human. There is no
// --force here and there is none anywhere, not even --force-with-lease: every
// write to the remote is a fast-forward or it does not happen.
//
// --no-follow-tags is what keeps "one branch in each direction" true against
// the vault's own config: push.followTags there — the human's file, which
// outranks obsync's private one — otherwise sends every annotated tag reachable
// from the pushed commit along with it. Measured on both matrix points.
//
// --porcelain is what makes the answer machine-readable, and what obsync does
// with it is dispositionOf below.
func (r *Repo) Push(ctx context.Context) error {
	ref := "refs/heads/" + r.branch
	_, err := r.run(invocation{
		dir:      r.vault,
		args:     []string{"push", "--porcelain", "--no-follow-tags", config.RemoteName, ref + ":" + ref},
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	})
	var command *CommandError
	if !errors.As(err, &command) {
		// A deadline, a shutdown, or a git that could not be started at all.
		// None of them is a verdict either, and each already says what it is.
		return err
	}
	return dispositionOf(command)
}

// ErrLostTheRace is the remote already holding a commit obsync had not seen when
// it pushed: the push was a non-fast-forward and git refused it locally.
//
// It is an **aborted run** (§7): nothing is reported above debug, and the next
// run fetches, classifies as diverged, merges and pushes — which is the
// designed-for case rather than an anomaly (§3). It does not back the network
// half off, because nothing about the remote is wrong: it answered, and what it
// answered is that obsync is behind.
var ErrLostTheRace = errors.New("the remote had moved on and the push was not a fast-forward")

// ErrRemoteFailure is the remote failing to say what it did with a ref — git's
// `[remote failure]`, whose documented reason is "remote failed to report
// status".
//
// It is not a verdict: the remote never reported one. So it is an **aborted
// run** on the ordinary network backoff, like every other way the far end can
// stop mid-sentence, and it is deliberately not the freeze `[remote rejected]`
// gets — the whole difference between the two rows is that one of them is an
// answer.
var ErrRemoteFailure = errors.New("the remote did not report what it did with the push")

// ErrRefspecMatchedNothing is git's `[no match]`: the refspec obsync handed git
// matched nothing on this side. obsync always pushes the tracked branch it
// resolved at bootstrap and holds for the process lifetime (§3), so this is a
// fact about obsync rather than about the remote, and it deliberately has no
// tier row — a failure obsync has no rule for is reported as exactly that.
var ErrRefspecMatchedNothing = errors.New("the refspec obsync pushed matched no ref in the vault")

// RemoteRejection is a push the remote received, evaluated and declined — a
// hook, a policy, a quota, or a blob over a limit obsync cannot discover
// (CONTEXT.md). It is the one network failure no amount of waiting repairs.
//
// It carries the remote's words and none of obsync's: **obsync relays, and
// never diagnoses.** GitHub's rejection does name the offending path, but only
// in prose, and a tool that guessed at which path from a sentence written for a
// human would be wrong on the day the sentence changed — quietly, and about the
// one thing the operator is trying to find.
type RemoteRejection struct {
	// Reason is git's trailing (<reason>) verbatim, which git-push(1) defines
	// as "a human-readable explanation". Nothing branches on it.
	Reason string

	// Said is what the far end said for itself: the lines git prefixes
	// `remote: ` because it received them over the sideband from the other
	// side. It is where a real remote puts everything an operator actually
	// needs — GitHub's `GH001: Large files detected`, a hook's own sentence —
	// and it is kept as the lines it arrived as so that whatever renders it
	// can render them as lines.
	Said []string
}

// Relayed is the remote's own words as the lines they arrived as: git's
// trailing (<reason>), which git-push(1) defines as "a human-readable
// explanation", followed by whatever the far end said for itself.
//
// It exists beside Error rather than instead of it because the two places a
// human reads this want two shapes. A log line is one line, so Error joins them
// with a separator. §9 obliges the attention note to carry them **verbatim in a
// fenced block, labelled as the remote's words rather than obsync's** — and a
// block is a thing made of lines, which is exactly what a hook's own multi-line
// sentence is.
//
// Nothing branches on any of it, here or anywhere: obsync relays and never
// diagnoses.
func (r *RemoteRejection) Relayed() []string {
	relayed := make([]string, 0, len(r.Said)+1)
	if r.Reason != "" {
		relayed = append(relayed, r.Reason)
	}
	return append(relayed, r.Said...)
}

func (r *RemoteRejection) Error() string {
	relayed := "the remote received obsync's push, evaluated it and rejected it. Its own " +
		"explanation, relayed and not interpreted: " + quoted(r.Reason)
	if len(r.Said) > 0 {
		relayed += ", and the remote also said: " + quoted(strings.Join(r.Said, "; "))
	}
	return relayed
}

func quoted(s string) string { return `"` + s + `"` }

// dispositionOf sorts a failed push into §7's table, and it is the only place
// obsync decides what a remote did with one.
//
// It branches on the `<summary>` enum alone. The four failure summaries are
// git-push(1)'s closed list and are spelled here as the four cases a reader can
// check against it; the parenthetical beside each is carried and never read,
// which is why `[rejected]` is one row rather than one row per reason git might
// give for refusing a non-fast-forward.
func dispositionOf(command *CommandError) error {
	summary, reason, answered := refStatus(command.Stdout)
	if !answered {
		if command.ExitCode == neverReachedTheRemote {
			// The table's last row: the connection, the host or the credential
			// failed before the remote ever evaluated anything, so no verdict
			// was returned. That is the same fact a failed fetch carries and it
			// takes the same tier — obsync has been told nothing, which is
			// precisely the state waiting repairs.
			return unanswered(command)
		}
		// A push git refused on this side, before any of it went anywhere. It
		// is not the row above and must not be sorted into it: that row is the
		// abort tier, which says nothing above debug, and this is a refusal no
		// amount of waiting repairs. obsync has no rule for it and says so by
		// having none.
		return command
	}
	switch summary {
	case "[rejected]":
		return fmt.Errorf("%w: %w", ErrLostTheRace, command)
	case "[remote rejected]":
		return &RemoteRejection{Reason: reason, Said: whatTheRemoteSaid(command.Stderr)}
	case "[remote failure]":
		return fmt.Errorf("%w: %w", ErrRemoteFailure, command)
	case "[no match]":
		return fmt.Errorf("%w: %w", ErrRefspecMatchedNothing, command)
	}
	// A push that failed while reporting a summary that is not one of the four
	// failures git documents. obsync has no rule for it and says so by having
	// none: it travels on as an ordinary failed run rather than being sorted
	// into whichever tier is nearest.
	return command
}

// neverReachedTheRemote is git's everything-code, and for a push with no ref
// line on stdout it is what separates the two ways one can produce none.
//
// §7's last row is exit **128** with empty stdout — the host, the connection or
// the credential failing before the remote evaluated anything, so no verdict was
// ever returned. A push git refuses on this side produces no ref line either and
// exits **1**: measured at both matrix points, 2.38.5 and 2.52.0, and the two
// agree — a `pre-push` hook in the human's own repository declining, and a src
// refspec matching nothing locally, each exit 1 with stdout empty. Neither is a
// fact about the remote, and neither is repaired by waiting, so reading them as
// the row above would put a vault that has permanently stopped being backed up
// on the tier that says nothing above debug.
const neverReachedTheRemote = 128

// refStatus is the ref line git wrote for the push, split into the two fields
// git-push(1) documents: `<flag> \t <from>:<to> \t <summary> (<reason>)`.
//
// It answers false when there is no ref line at all, which is the disposition
// table's own last row rather than a parse failure.
//
// **This splits git's output on newlines and tabs, and that is safe here for a
// reason that does not generalise.** The rule against it is about paths, and
// there is no path in this listing: the middle field is a pair of ref names,
// and git-check-ref-format(1) forbids a space, a newline and every other
// control character in one. The `To <url>` line — the one field that could hold
// something wilder — is never read, because it has no tab in it. A `(<reason>)`
// that somehow held a newline would leave the relayed sentence short and the
// branch above unchanged, because the summary is decided before it.
func refStatus(stdout []byte) (summary, reason string, answered bool) {
	for _, line := range strings.Split(string(stdout), "\n") {
		flag, rest, tabbed := strings.Cut(line, "\t")
		if !tabbed || len(flag) != 1 {
			continue
		}
		_, status, tabbed := strings.Cut(rest, "\t")
		if !tabbed {
			continue
		}
		if !strings.HasPrefix(status, "[") {
			// A successful ref's summary is `<old>..<new>` rather than one of
			// the bracketed enum members. Carried whole, so that the caller's
			// switch simply does not match it.
			return status, "", true
		}
		end := strings.Index(status, "]")
		if end < 0 {
			return status, "", true
		}
		reason = strings.TrimSpace(status[end+1:])
		reason = strings.TrimSuffix(strings.TrimPrefix(reason, "("), ")")
		return status[:end+1], reason, true
	}
	return "", "", false
}

// whatTheRemoteSaid is the far end's own words: the lines git prefixes
// `remote: ` because it received them over the sideband from the other side.
//
// Relayed and never read. This is the one place obsync carries a remote's prose
// at all, and it does exactly what §7 asks of it — the operator is handed the
// sentence the remote wrote, labelled as the remote's, and obsync draws no
// conclusion from it. Everything a real remote says about *why* lives here
// rather than in the parenthetical: `pre-receive hook declined` is all git
// itself knows, and `GH001: Large files detected` is what the human needs.
//
// The trailing spaces are git's, not the remote's: the sideband pads a line out
// to clear whatever progress output was on it.
func whatTheRemoteSaid(stderr string) []string {
	var said []string
	for _, line := range strings.Split(stderr, "\n") {
		rest, prefixed := strings.CutPrefix(line, "remote: ")
		if !prefixed {
			continue
		}
		if trimmed := strings.TrimRight(rest, " \t\r"); trimmed != "" {
			said = append(said, trimmed)
		}
	}
	return said
}
