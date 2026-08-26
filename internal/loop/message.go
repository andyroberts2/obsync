package loop

import (
	"strconv"
	"strings"

	"github.com/andyroberts2/obsync/internal/git"
)

// bodyCap is how many paths a commit body lists before it starts counting
// instead (§2). Fifty is a body a human scrolls past once; a bulk import of
// three thousand notes would otherwise put a screenful of git log between every
// pair of commits, and the count says the same thing.
const bodyCap = 50

// commitMessage is what a sync run's commit says it did.
//
// The message summarises the change and nothing else: provenance lives in the
// commit identity, so there is no date, no template token and no "vault backup"
// — which on a commit that deleted forty notes is actively misleading (§2).
func commitMessage(changes []git.Change) string {
	var message strings.Builder
	message.WriteString(subject(changes))
	message.WriteString("\n")

	// One path is already named in the subject, so a body would be the same
	// line twice.
	if len(changes) < 2 {
		return message.String()
	}

	message.WriteString("\n")
	listed := 0
	for _, kind := range []struct {
		kind   git.Kind
		marker string
	}{
		{git.Added, "+"},
		{git.Modified, "~"},
		{git.Deleted, "-"},
	} {
		for _, change := range changes {
			if change.Kind != kind.kind {
				continue
			}
			if listed == bodyCap {
				continue
			}
			message.WriteString(kind.marker)
			message.WriteString(" ")
			message.WriteString(change.Path)
			message.WriteString("\n")
			listed++
		}
	}
	if beyond := len(changes) - listed; beyond > 0 {
		message.WriteString("… and " + count(beyond) + " more\n")
	}
	return message.String()
}

// subject names the file when exactly one path changed and counts them
// otherwise, with the verb varying by what happened to them (§2).
//
// The three verbs are the three §2 names. A single added note reads as an
// import of one rather than as an update, because the verb describes the
// operation and inventing a fourth for the one-path case would be a judgement
// this design already made.
func subject(changes []git.Change) string {
	verb := "Update"
	switch {
	case all(changes, git.Added):
		verb = "Import"
	case all(changes, git.Deleted):
		verb = "Delete"
	}
	if len(changes) == 1 {
		return verb + " " + changes[0].Path
	}
	return verb + " " + count(len(changes)) + " notes"
}

func all(changes []git.Change, kind git.Kind) bool {
	for _, change := range changes {
		if change.Kind != kind {
			return false
		}
	}
	return len(changes) > 0
}

// count writes a number the way §2's examples do — 3,201 rather than 3201 —
// because the number is being read by a human scrolling git log.
func count(n int) string {
	digits := strconv.Itoa(n)
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteString(",")
		}
		grouped.WriteRune(digit)
	}
	return grouped.String()
}
