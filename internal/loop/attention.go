package loop

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andyroberts2/obsync/internal/git"
	"github.com/andyroberts2/obsync/internal/vault"
)

// The attention note (§9, #38): the channel that actually works, because it is
// the one this user is already looking at.
//
// Every other signal obsync manufactures is somewhere an operator has to go —
// `docker ps`, `docker exec`, `docker logs`. This one is a note in their vault,
// and its being there at all is the signal: it is written when obsync needs a
// human and **deleted, not emptied**, when it does not.
//
// Nothing here is remembered. Every section is worked out again from live state
// on every wake-up — the freezes from what the interlocks and the merge found
// this run, and the other three from the vault — so the note is never
// authoritative over what it describes and cannot drift from it for longer than
// one run. That is what lets a human delete a conflict copy, or fix a mount, and
// have obsync agree with them a tick later without being told.

// reconcileAttentionNote is the one function that reconciles the note against
// live state, and it is asked at the end of every wake-up, beside the status
// file (§9).
//
// One call site rather than the two §9 names, and the reading is worth stating
// because it is a reading. §9 asks for the reconcile on the merge path *before*
// `commit-tree` "so a conflict event is one commit", and again on the human's
// resolution path where "the local half picks the note up as an ordinary dirty
// file". Both of those exist to keep the note out of a commit of its own — and
// the note is in the ignore floor (§5), so **no write of it can produce a commit
// at all**, and there is no standalone attention-note commit to prevent. What
// the two triggers were for, the end of a wake-up gives: a run that merged has
// its copies in the vault by the time this is asked, and the run that commits a
// human's deletion of one reconciles the note in the same breath. It also gives
// the thing neither of them can, and the first section is the whole point of the
// note — a full freeze returns from a sync run long before the local half, so a
// note written only from those two places would never once name a freeze.
//
// A failure is a debug line and nothing more, for the reason the status file's
// write is: the note is how obsync reports things, and reporting the loss of it
// once a tick is noise about noise. The consequence is visible where it should
// be — the note stops agreeing with the vault, and the freeze it would have
// carried is in the log and in the health verdict regardless.
func (l *Loop) reconcileAttentionNote(now time.Time) {
	// The vault, not the repository. The copies are found by the filename
	// pattern that is §4's whole recovery state, so this answers under a
	// damaged repository too — which is exactly the freeze whose note an
	// operator needs most.
	copies, err := l.repo.OutstandingConflictCopies()
	if err != nil {
		// Reported and then carried on from rather than returned on. A section
		// obsync could not derive is a section short; a freeze obsync could not
		// say is the note's entire reason for existing.
		l.log.Debug("obsync could not look through the vault for the conflict copies its "+
			"attention note names", "problem", err)
	}
	if err := l.repo.ReconcileAttentionNote(l.attentionNote(now, copies)); err != nil {
		l.log.Debug("obsync could not reconcile its attention note with what it can see",
			"problem", err)
	}
}

// attentionNote is what the note would say right now, or nil when there is
// nothing to say and the note should not exist at all.
//
// The four sections are in §9's fixed order — live freezes, outstanding conflict
// copies, refused paths, paths that have stopped looking transient — and each is
// left out entirely when it is empty, so the note gains and loses sections as
// reality changes rather than carrying four headings and three shrugs.
func (l *Loop) attentionNote(now time.Time, copies []git.ConflictCopy) []byte {
	freezes := l.liveFreezes()
	refused := l.refusedNow()
	unsettled := l.unsettledForLongNow(now)
	if len(freezes)+len(copies)+len(refused)+len(unsettled) == 0 {
		return nil
	}

	var note strings.Builder
	note.WriteString(attentionPreamble)
	writeFreezes(&note, freezes)
	writeConflictCopies(&note, copies)
	writeRefusedPaths(&note, refused)
	writeUnsettledPaths(&note, unsettled)
	return []byte(note.String())
}

// attentionPreamble is what the note says before it says anything obsync found,
// and it is two facts a reader needs before they read the rest: this file is
// obsync's and it goes away on its own, and nothing in it is a record — it is
// what obsync could see on its last time round.
const attentionPreamble = "# obsync needs you to look at something\n" +
	"\n" +
	"obsync writes this note in your vault when it needs a human, and deletes it again when it\n" +
	"does not — so the fact that you are reading it is itself the signal. Everything below is\n" +
	"worked out again from scratch on every sync run, from your vault and from what obsync can\n" +
	"see right now, so nothing here is more than one run old and nothing here is in charge of\n" +
	"what it describes: fix the thing and this note follows you. obsync never commits it, and it\n" +
	"reaches no other clone of your vault.\n"

// The four headings, in §9's fixed order. They are named after the glossary's
// own words, because the note, the log line and `obsync status` should be three
// renderings of one vocabulary rather than three vocabularies (CONTEXT.md).
const (
	freezesHeading        = "\n## Freezes\n"
	conflictCopiesHeading = "\n## Conflict copies\n"
	refusedPathsHeading   = "\n## Refused paths\n"
	unsettledPathsHeading = "\n## Paths that will not settle\n"
)

// writeFreezes is the note's first section: every freeze obsync is standing in,
// each with the conclusive fact that caused it and the remedy — closing, as
// every remedy obsync writes does, with the sentence that is the highest-value
// thing it can say anywhere (git.SelfClearing).
//
// The full freeze comes first where both are live, which is §7's own ordering:
// full wins. Neither is summarised or reworded here — what an operator reads in
// their vault is the same fact and the same remedy the ERROR carried and
// `obsync status` prints, because three wordings of one state read as three
// states.
func writeFreezes(note *strings.Builder, freezes []liveFreeze) {
	if len(freezes) == 0 {
		return
	}
	note.WriteString(freezesHeading)
	note.WriteString("\nobsync has stopped doing something until this is repaired, and it is still " +
		"running and\nstill checking. Repair the cause and it starts again on its own.\n\n")
	for _, frozen := range freezes {
		fmt.Fprintf(note, "- **%s** — %s\n", frozen.name, frozen.fact)
		if len(frozen.relayed) > 0 {
			// The remote's words, verbatim, in a block, and labelled as the
			// remote's rather than obsync's (§9). obsync relays and never
			// diagnoses: it does not name the offending path, because the only
			// place the remote named one is a sentence written for a human.
			note.WriteString("\n  The remote's own words, verbatim and as the lines they arrived " +
				"as. These are the\n  remote's and not obsync's, and obsync never guesses at " +
				"which file or which rule is\n  the problem:\n\n")
			writeFenced(note, "  ", frozen.relayed)
		}
		fmt.Fprintf(note, "\n  What to do: %s.\n\n", frozen.remedy)
	}
}

// writeConflictCopies is the note's second section: every copy still standing in
// the vault, wikilinked beside the note it is a copy of, so that a human
// resolves the pair from inside Obsidian rather than from a terminal (§4).
//
// One line of instruction for the whole section rather than one per copy,
// because there is one instruction: the recovery state is the filename, so
// deleting the copy is the whole of finishing.
func writeConflictCopies(note *strings.Builder, copies []git.ConflictCopy) {
	if len(copies) == 0 {
		return
	}
	note.WriteString(conflictCopiesHeading)
	note.WriteString("\nBoth sides changed these notes, so obsync kept both rather than choosing " +
		"between them.\nEdit the two together, then delete the copy — that is the whole of it, " +
		"and the ordinary\nsync loop commits it like any other edit.\n\n")
	for _, copied := range copies {
		fmt.Fprintf(note, "- %s — the other version of %s\n", asLink(copied.Path), asLink(copied.Of))
	}
	note.WriteString("\n")
}

// writeRefusedPaths is the note's third section, and it is the standing signal
// the once-per-path WARN deliberately is not (§5, §9): a refused path stays
// refused, so the log says it once and this says it for as long as it is true.
func writeRefusedPaths(note *strings.Builder, refused []vault.Refusal) {
	if len(refused) == 0 {
		return
	}
	note.WriteString(refusedPathsHeading)
	note.WriteString("\nobsync will not put these in a commit, and the rest of your vault keeps " +
		"syncing without\nthem. While that stands the remote holds the last version that passed " +
		"and your vault holds\na newer one. Renaming the file is the whole of the escape hatch — " +
		"obsync matches names\nand never reads what is inside them.\n\n")
	for _, refusal := range refused {
		fmt.Fprintf(note, "- %s — %s\n", asPath(refusal.Path), refusal.Reason)
	}
	note.WriteString("\n")
}

// writeUnsettledPaths is the note's fourth section: the paths the settle guard
// has left out of every commit for long enough to stop looking like latency
// (§6).
//
// Transient exclusion is silent and stays silent — a note somebody is typing
// into arrives the moment they pause. What this names is the other thing: a file
// something is rewriting faster than obsync can ever see it still, which without
// this is a path that silently never reaches the remote at all.
func writeUnsettledPaths(note *strings.Builder, unsettled []unsettledFor) {
	if len(unsettled) == 0 {
		return
	}
	note.WriteString(unsettledPathsHeading)
	note.WriteString("\nSomething has moved these on disk every single time obsync has looked at " +
		"them, for long\nenough to stop looking like ordinary latency, so obsync is leaving them " +
		"out of its commits\nand they are not reaching the remote. Everything else in your vault " +
		"is syncing normally,\nand each of these commits on its own as soon as whatever is " +
		"writing it stops.\n\n")
	for _, moving := range unsettled {
		fmt.Fprintf(note, "- %s — moving every time obsync has looked at it since %s\n",
			asPath(moving.path), moving.since.UTC().Format(unsettledStamp))
	}
	note.WriteString("\n")
}

// liveFreezes is the freezes obsync is standing in, full first (§7: where two
// are live at once, full wins).
func (l *Loop) liveFreezes() []liveFreeze {
	var live []liveFreeze
	for _, frozen := range []liveFreeze{l.frozen, l.networkFrozen} {
		if frozen.name != "" {
			live = append(live, frozen)
		}
	}
	return live
}

// refusedNow is what obsync is currently declining to commit, sorted by path.
//
// It is the same live set the WARN fires off, re-established by every run's
// local half from what git reports: a path that stops being refused is dropped
// from it, so this section loses a line on the run after the human renames the
// file. Sorted because the note is compared against what the vault holds before
// it is written: a list that came out in a different order each time would be a
// write every run over a state nobody changed.
func (l *Loop) refusedNow() []vault.Refusal {
	refused := make([]vault.Refusal, 0, len(l.refused))
	for path, reason := range l.refused {
		refused = append(refused, vault.Refusal{Path: path, Reason: reason})
	}
	slices.SortFunc(refused, func(a, b vault.Refusal) int { return strings.Compare(a.Path, b.Path) })
	return refused
}

// unsettledFor is one path this section names, and when obsync first saw it
// moving.
//
// The instant rather than the elapsed time, and that is what lets the section
// sit still. obsync's own writes are not suppressed from the watcher (§4), so a
// line counting upwards would differ on every wake-up, defeat
// ReconcileAttentionNote's byte comparison from inside the note's own content,
// and make each run's write the wake that starts the next one. On the vault
// this section is most likely to be about — one whose churn is a writer on
// another host over NFS or SMB — inotify sees nothing of that writer, so the
// note's own write is the only event there is, and a 60s tick becomes a
// permanent quiet-window cycle over a vault nobody local is touching. The fact
// obsync holds is the instant, and it does not move while the path does not
// settle.
type unsettledFor struct {
	path  string
	since time.Time
}

// unsettledStamp is how that instant is written: UTC at minute precision, which
// is as fine as a stamp a human reads off a note is worth and the same
// precision a conflict copy's own name carries.
const unsettledStamp = "2006-01-02 15:04 MST"

// unsettledForLongNow is the paths that have stayed unsettled past the point
// where exclusion stops being latency and starts being news, with when obsync
// first saw each of them move.
//
// The threshold is unsettledForLong and the reason for the number lives beside
// it in cadence.go. It is asked of the elapsed time rather than of whether the
// WARN has fired, so that the section and the warning are two readings of one
// fact rather than one depending on the other having happened.
func (l *Loop) unsettledForLongNow(now time.Time) []unsettledFor {
	var unsettled []unsettledFor
	for path, record := range l.unsettled {
		if now.Sub(record.since) >= unsettledForLong {
			unsettled = append(unsettled, unsettledFor{path: path, since: record.since})
		}
	}
	slices.SortFunc(unsettled, func(a, b unsettledFor) int { return strings.Compare(a.path, b.path) })
	return unsettled
}

// asLink is a vault path as Obsidian links it, so that a human clicks through
// to the note rather than reading its name.
//
// The extension is dropped for a note and kept for anything else, which is
// Obsidian's own rule: `[[Daily/2026-08-24]]` opens the note and
// `[[Attachments/diagram.png]]` opens the image. A path holding one of the
// characters a wikilink cannot carry is written as a plain path instead — a
// broken link is worse than a name, because it looks like obsync got the name
// wrong.
//
// Both halves of a pair are the human's own name: a conflict copy is the
// canonical name with a marker in it (§4), so `Notes/[draft] plan.md` and its
// copy alike fall to a plain path. A newline is on the list for asPath's
// reason rather than Obsidian's — a vault path may legally hold one (§1), and a
// newline inside a list item ends the list item, so a link carrying one would
// break the pair into two lines and lose the half a human is told to edit
// against.
func asLink(relative string) string {
	target := strings.TrimSuffix(relative, ".md")
	if target == "" || strings.ContainsAny(target, "[]|#^\n\r") {
		return asPath(relative)
	}
	return "[[" + target + "]]"
}

// asPath is a vault path in a line a human reads: an inline code span, or a
// quoted one where the path holds something a single line cannot carry.
//
// A vault path may contain spaces, unicode and — legally — a newline (§1), and
// a newline in the middle of a list item is a list item that stops being one.
// Quoting is Go's own spelling of the same escape git uses for the same reason,
// and it is applied only where it is needed, so the ordinary path stays the
// thing a human can copy.
func asPath(relative string) string {
	if strings.ContainsAny(relative, "\n\r") {
		return inlineCode(strconv.Quote(relative))
	}
	return inlineCode(relative)
}

// inlineCode is a string as a markdown code span, with a fence long enough for
// whatever backticks the string itself holds — a note title may hold one, and
// obsync writes the human's own names back to them unaltered.
func inlineCode(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	// A span whose content starts or ends with a backtick needs one space of
	// padding, which markdown strips again on the way back out.
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// writeFenced writes somebody else's lines as a fenced block, indented far
// enough to stay inside the list item above it.
//
// The fence is longer than the longest run of backticks in what it encloses, so
// that words obsync did not write and cannot vet — a remote's hook can say
// anything at all — are rendered rather than allowed to end the block early and
// turn the rest of the note into whatever they happen to be.
func writeFenced(note *strings.Builder, indent string, lines []string) {
	fence := "```"
	for _, line := range lines {
		for strings.Contains(line, fence) {
			fence += "`"
		}
	}
	note.WriteString(indent + fence + "text\n")
	for _, line := range lines {
		note.WriteString(indent + line + "\n")
	}
	note.WriteString(indent + fence + "\n")
}
