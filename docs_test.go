package main

import (
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/git"
)

// The operator documentation set (§11, #42).
//
// Two readers, ranked: the operator, then the contributor. What can be checked
// rather than reviewed is checked here, and it is the same half of the job that
// interface_test.go does on the surface page: a document is prose, but *which*
// documents exist, which lines are load-bearing, and whether the running system
// and the page it sends an operator to agree, are all facts.
//
// Every file below is read in process rather than through a subprocess, which
// is load-bearing rather than incidental, for the reason gomod_test.go states:
// `go test` keys its result cache on the files a test itself opens, so an edit
// to a document re-runs the tests that read it rather than returning a cached
// pass.

// The seven pieces §11 names, and the job each one does. They are written out
// here rather than discovered, because a document that is missing is exactly
// what this test is for — a set derived from what is on disk can never be
// short of a member.
var documentationSet = []struct {
	path string
	job  string
}{
	{"README.md", "what obsync is, whether it fits, and the quickstart — the operator's first 30 seconds"},
	{"compose.yaml", "the reference stack, ignis and obsync, normative rather than exemplary"},
	{"docs/interface.md", "the declared surface (§10)"},
	{"docs/credentials.md", "minimum scopes, the four remotes, and SSH"},
	{"docs/operations.md", "the tiers, the freezes, the note, the recipes — the operator at 3am"},
	{"SECURITY.md", "a private disclosure route"},
	{"CONTRIBUTING.md", "the transcription rule, and nothing else"},
}

func TestTheDocumentationSetIsTheSevenPiecesTheDesignNames(t *testing.T) {
	t.Parallel()

	for _, piece := range documentationSet {
		if _, err := os.Stat(piece.path); err != nil {
			t.Errorf("%s is missing, and it is one of the seven pieces of obsync's documentation "+
				"set: %s (§11)", piece.path, piece.job)
		}
	}
}

// A remedy that names a document is obsync sending an operator somewhere at the
// moment they most need to arrive, so the document has to be there. Every one of
// these remedies was written slices before the page it names existed, which is
// exactly the drift this catches — and it caught it: this test was red until
// this commit.
func TestEveryDocumentObsyncSendsAnOperatorToExists(t *testing.T) {
	t.Parallel()

	sent := map[string][]string{}
	for path, source := range obsyncSource(t) {
		for _, doc := range documentsNamedIn(source) {
			sent[doc] = append(sent[doc], path)
		}
	}
	if len(sent) == 0 {
		t.Fatal("obsync's source names no document at all, and several of its remedies are " +
			"written to send an operator to one: a remedy that names no recipe is a freeze with " +
			"nowhere to go (§11)")
	}
	for doc, from := range sent {
		if _, err := os.Stat(doc); err != nil {
			t.Errorf("%s names %s in a remedy an operator reads in the attention note, and there "+
				"is no such file: %v", strings.Join(from, ", "), doc, err)
		}
	}
}

// documentsNamedIn is every `docs/<name>.md` a file mentions. It is a plain
// scan rather than a parse because that is how the name reaches an operator
// too: obsync's remedies name a path in prose, and a path that does not exist
// is wrong wherever it is written.
func documentsNamedIn(source string) []string {
	found := map[string]bool{}
	var docs []string
	for _, doc := range documentPath.FindAllString(source, -1) {
		if !found[doc] {
			found[doc] = true
			docs = append(docs, doc)
		}
	}
	return docs
}

var documentPath = regexp.MustCompile(`docs/[a-z0-9-]+\.md`)

// The load-bearing class (§11), and the one mechanical check the design asks
// for: **every marker carries a resolvable issue link.** That is what makes
// "its absence is a defect rather than a gap" something the suite can decide
// rather than something a reviewer has to remember, and it is the whole reason
// the class is marked in place instead of collected into an index — an index of
// prose is a second copy of prose, and the marker greps.
const loadBearingMarker = "load-bearing documentation"

// A link is resolvable when it names an issue in this repository, in either of
// the two spellings the repository already uses: the absolute URL a file
// outside GitHub's rendering needs, and the relative one a Markdown page can
// use. Both resolve; neither is checked over the network, because a suite that
// needs one cannot run on a fork's pull request.
var issueLink = regexp.MustCompile(`(https://github\.com/andyroberts2/obsync/issues/|\.\./\.\./issues/)([1-9][0-9]*)`)

func TestEveryLoadBearingMarkerCarriesAResolvableTicket(t *testing.T) {
	t.Parallel()

	markers := 0
	for _, path := range markedFiles(t) {
		for _, callout := range calloutsIn(t, path) {
			markers++
			if !issueLink.MatchString(callout) {
				t.Errorf("%s marks a line load-bearing and the callout names no issue:\n\n%s\n\n"+
					"A load-bearing line names the decision that put it there, with a resolvable "+
					"link, because it is the only thing standing where the design declined to put "+
					"code — and a reader has to be able to reach the argument for it (§11)",
					path, callout)
			}
		}
	}
	if markers == 0 {
		t.Fatal("nothing in obsync's documentation is marked load-bearing, and the design names " +
			"eight members: a class only the maintainer can see does half its work (§11)")
	}
}

// The eight members §11 names, plus the one this project found afterwards — the
// passwd line an SSH remote needs, which the image's own review measured and
// handed here. Each is identified by where it stands and by a phrase that has to
// be inside the callout marking it, written out rather than discovered, for the
// reason the documentation set above is: a member that has gone missing is
// exactly what this test is for.
//
// A tenth marker is not a failure. The class is a rule about what must be
// marked rather than a closed list — what is closed is that every line here is
// one the code deliberately declined to be, so none of them may quietly stop
// being marked.
var loadBearingLines = []struct {
	file   string
	phrase string
	why    string
}{
	{"compose.yaml", `user: "1000:1000"`, "the UID and GID mapping that replaces a PUID/PGID knob (§8)"},
	{"compose.yaml", "WRITE_COALESCE_MS", "ignis's write coalescing, pinned to zero on the neighbour that owns it (§11)"},
	{"docs/credentials.md", "/etc/passwd", "the passwd line an SSH remote needs, which ssh reads ~ out of rather than HOME (§8)"},
	{"README.md", "Headless Sync", "the warning that decides whether obsync is for you at all (§11)"},
	{"README.md", "obsync never", "the never-list, the one member load-bearing in the other direction (§11)"},
	{"docs/operations.md", "The recovery recipe", "what replaces the repair of a damaged repository obsync will not do (§7)"},
	{"docs/operations.md", "relays, never diagnoses", "the remote-rejection recipe, which is entirely the human's (§7)"},
	{"docs/operations.md", selfClearing, "the highest-value sentence obsync writes anywhere (§9, §11)"},
	{"CONTRIBUTING.md", "transcription", "the quarantine that keeps the licence obligation auditable (§12)"},
}

func TestTheKnownLoadBearingLinesAreMarkedInPlace(t *testing.T) {
	t.Parallel()

	for _, line := range loadBearingLines {
		found := false
		for _, callout := range calloutsIn(t, line.file) {
			if strings.Contains(callout, line.phrase) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s does not mark %q as load-bearing documentation: it is %s. A load-bearing "+
				"line is marked in place and visibly, is never cut for brevity, and its absence is "+
				"a defect rather than a gap (§11)", line.file, line.phrase, line.why)
		}
	}
}

// markedFiles is every document and every source file obsync ships, which is
// where a marker may be. The class is about documentation, and one of its
// members is a sentence that lives in obsync's own source because that is where
// the line is written from.
func markedFiles(t *testing.T) []string {
	t.Helper()

	files := []string{"compose.yaml"}
	for _, piece := range documentationSet {
		if strings.HasSuffix(piece.path, ".md") {
			files = append(files, piece.path)
		}
	}
	for path := range obsyncSource(t) {
		files = append(files, path)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// calloutsIn is every load-bearing callout in a file: the marker, and the block
// that carries it. The block is bounded by a line count rather than by
// punctuation, because the three places this class appears in — a Markdown
// blockquote, a YAML comment and a Go doc comment — have no delimiter in
// common, and inventing one for each would make this check a parser of three
// grammars rather than a grep for a marker.
func calloutsIn(t *testing.T, path string) []string {
	t.Helper()

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s, which obsync's documentation set names: %v", path, err)
	}
	lines := strings.Split(string(source), "\n")

	var callouts []string
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), loadBearingMarker) {
			continue
		}
		end := min(i+calloutLines, len(lines))
		callouts = append(callouts, strings.Join(lines[i:end], "\n"))
	}
	return callouts
}

// calloutLines is how much of a file below the marker counts as the callout. It
// is generous on purpose: what is being checked is that a reader who found the
// marker can reach the decision behind it and the line it stands in front of,
// and both of those are within a screen of it in every case here.
const calloutLines = 40

// Every entry names its owning ticket, so softening one is visibly an amendment
// rather than an edit. That is what the never-list is for: it is the list this
// project asks to be trusted at, and an entry whose argument cannot be reached
// is one a later reader can talk themselves out of.
func TestEveryNeverListEntryNamesItsOwningTicket(t *testing.T) {
	t.Parallel()

	entries := 0
	for _, entry := range strings.Split(neverListOnThePage(t), "\n- ")[1:] {
		entries++
		if !issueLink.MatchString(entry) {
			t.Errorf("this never-list entry names no ticket:\n\n- %s\n\nEvery entry names the "+
				"decision that made it a promise, so softening one is visibly an amendment "+
				"rather than an edit (§11)", strings.TrimSpace(entry))
		}
	}
	if entries < len(forbidden) {
		t.Errorf("the README's never-list has %d entries and %d argv rules are enforced against "+
			"it: the list an operator reads may not be shorter than the one the suite checks (§11)",
			entries, len(forbidden))
	}
}

// A documentation set is a set of links as well as a set of files, and a link
// that goes nowhere is the same defect as a remedy naming a page that does not
// exist. Links out to GitHub — an issue, or somebody else's repository — are
// not checked: a suite that needs the network cannot run on a fork's PR.
func TestEveryRelativeLinkInTheDocSetResolves(t *testing.T) {
	t.Parallel()

	for _, piece := range documentationSet {
		if !strings.HasSuffix(piece.path, ".md") {
			continue
		}
		source, err := os.ReadFile(piece.path)
		if err != nil {
			t.Fatalf("reading %s: %v", piece.path, err)
		}
		for _, link := range markdownLink.FindAllStringSubmatch(string(source), -1) {
			target, _, _ := strings.Cut(link[2], "#")
			switch {
			case target == "", strings.Contains(link[2], "://"), strings.HasPrefix(link[2], "../../"):
				continue
			}
			if _, err := os.Stat(resolve(piece.path, target)); err != nil {
				t.Errorf("%s links to %q, and there is nothing there: %v", piece.path, link[2], err)
			}
		}
	}
}

var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// resolve is a link's target as a path from the repository root, which is where
// the test binary runs. Only the one shape the doc set uses is handled — a page
// in docs/ linking to a sibling or to the root — because a link this cannot
// resolve fails the test above rather than passing it quietly.
func resolve(from, target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	return path.Clean(path.Join(path.Dir(from), target))
}

// The running system enumerates; the page teaches the shape (§11).
//
// The attention note's freeze section is derived from live state on every run
// and is obliged to name the freeze, the conclusive fact and the remedy — so a
// page that reproduced that list would take on a drift obligation for content
// an operator gets *more accurately* from the thing actually running. This
// drives a vault through three freezes it can be in, takes the names obsync
// itself wrote in the vault, and asserts the page reproduces none of them.
//
// It is one vault driven through three states rather than three vaults, because
// what is asserted is about the page and each state is only here to make obsync
// name one.
func TestOperationsTeachesTheShapeRatherThanEnumeratingWhatTheNoteDerives(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()

	var named []string
	freeze := func(cause, repair func()) {
		cause()
		env.advance(70 * time.Second)
		named = append(named, freezesNamedIn(t, env.attentionNote())...)
		repair()
		env.advance(70 * time.Second)
	}

	// The vault sentinel, gate 4 and gate 3: three of the freezes whose whole
	// account an operator reads out of their own vault. A `.git` that has gone
	// is deliberately not among them — that freeze writes no note at all, since
	// the staging directory obsync renames its writes out of lives inside it.
	freeze(env.theVaultGoesEmpty, env.theVaultComesBack)
	freeze(env.theHumanLeavesAMergeHalfFinished, func() {
		env.mustGit(env.vault, append(append([]string{}, humanIdentity...), "merge", "--abort")...)
	})
	freeze(func() {
		env.mustGit(env.vault, "checkout", "--quiet", "--detach")
	}, func() {
		env.mustGit(env.vault, "checkout", "--quiet", "main")
	})

	slices.Sort(named)
	if named = slices.Compact(named); len(named) < 3 {
		t.Fatalf("obsync named %d distinct freezes across the three states it was driven into (%v), "+
			"so this test is not standing on the state it is about", len(named), named)
	}
	page := flattened(t, "docs/operations.md")
	for _, freeze := range named {
		if strings.Contains(page, freeze) {
			t.Errorf("docs/operations.md names the freeze %q, which obsync writes into the vault "+
				"itself, derived from live state on every run. The page states the tiers and the "+
				"four things that are not derivable; it does not reproduce what the note "+
				"enumerates, because a second copy takes on a drift obligation for content an "+
				"operator gets more accurately from the thing running (§11)", freeze)
		}
	}
}

// freezesNamedIn is the names obsync gave the freezes it is standing in, read
// out of the note's own first section — `- **<name>** — <fact>`.
func freezesNamedIn(t *testing.T, note string) []string {
	t.Helper()

	section, _, _ := strings.Cut(after(t, note, "## Freezes"), "\n## ")
	var names []string
	for _, line := range freezeLine.FindAllStringSubmatch(section, -1) {
		names = append(names, line[1])
	}
	return names
}

var freezeLine = regexp.MustCompile(`(?m)^- \*\*(.+?)\*\* —`)

func after(t *testing.T, source, heading string) string {
	t.Helper()

	_, below, found := strings.Cut(source, heading)
	if !found {
		t.Fatalf("no %q section in:\n%s", heading, source)
	}
	return below
}

// The four things the running system cannot tell an operator itself, and what
// each one has to carry. Where the fact obsync writes is a constant, the
// constant is what is asserted rather than a second spelling of it: gate 9's
// remedy names a ref, and a page naming a different one sends a human to the
// wrong place with a command that silently does nothing.
func operationsOwes(t *testing.T) []struct{ fact, why string } {
	t.Helper()

	return []struct{ fact, why string }{
		{git.FailedApplyAnchor, "gate 9's remedy: the ref holding the tree obsync could not verify"},
		{"update-ref -d " + git.FailedApplyAnchor, "gate 9's remedy: the one freeze a human clears deliberately"},
		{selfClearing, "the sentence every remedy closes on, and the reason a restart is the wrong reflex"},
		{"restart", "that restarting destroys the diagnosis (user story 48)"},
		{"once an hour", "that a frozen obsync retries hourly, so waiting is a plan"},
		{"obsync-attention.md", "read the note — it is the first thing to do and the most accurate"},
		{"obsync status", "run the subcommand: the most direct answer to \"has this been working\""},
		{"aborted run", "the first of the three tiers"},
		{"network freeze", "the second of the three tiers"},
		{"full freeze", "the third of the three tiers"},
		{"`git status`", "the damage freeze self-clears by retrying a read-only probe, not by re-checking a gate"},
		{".git/index", "obsync rebuilds the index itself, so staged-but-uncommitted work can be dropped"},
		{"relays, never diagnoses", "the remote-rejection recipe, and what obsync will not add to it"},
		{"Swarm", "Swarm acts on health status"},
		{"Compose", "plain Compose ignores it"},
	}
}

func TestOperationsCarriesTheFourThingsTheRunningSystemCannotDerive(t *testing.T) {
	t.Parallel()

	page := flattened(t, "docs/operations.md")
	for _, owed := range operationsOwes(t) {
		if !strings.Contains(page, owed.fact) {
			t.Errorf("docs/operations.md does not say %q, and it is what carries %s (§11)",
				owed.fact, owed.why)
		}
	}
}

// The recovery recipe for a damaged repository, in the four moves that make it
// a recipe rather than a sympathy card. Each is a thing obsync deliberately
// does not do for the operator, so each one missing is a defect.
func TestTheDamagedRepositoryRecipeIsTheOneObsyncDeclinedToAutomate(t *testing.T) {
	t.Parallel()

	page := strings.ToLower(flattened(t, "docs/operations.md"))
	for _, move := range []struct{ phrase, why string }{
		{"keep the old .git", "preserve it rather than deleting it: the unpushed commits are in it"},
		{"clone", "re-clone beside the vault rather than in place, and reattach"},
		{"un-freezes on its own", "obsync releases itself once `git status` succeeds — no restart"},
		{"builds one back from head", "obsync rebuilds .git/index itself, so staged work can be dropped"},
	} {
		if !strings.Contains(page, move.phrase) {
			t.Errorf("the damaged-repository recipe does not say %q — %s. obsync never re-clones "+
				"and never repairs a repository by replacing it, and this recipe is the whole of "+
				"what replaces that code (§7, §11)", move.phrase, move.why)
		}
	}
}

// Every repo form obsync accepts is a form somebody has to get a credential
// for, so the credentials page answers all of them — including the two that
// need none. The forms are taken from the surface page rather than typed again
// here, so a form added to the surface is a row this page owes.
func TestTheCredentialsPageAnswersEveryRepoFormTheSurfaceAccepts(t *testing.T) {
	t.Parallel()

	page := flattened(t, "docs/credentials.md")
	forms := 0
	for _, line := range fencedBlockAfter(t, "### The repo, and the remote") {
		form, _, _ := strings.Cut(line, "#")
		form = strings.TrimSpace(form)
		if form == "" {
			continue
		}
		forms++
		if !strings.Contains(page, form) {
			t.Errorf("docs/credentials.md says nothing about %s, which %s accepts as a repo URL: "+
				"a form obsync supports and this page never mentions is an operator with no way "+
				"to find out what credential it takes (§8, §11)", form, interfacePage)
		}
	}
	if forms == 0 {
		t.Fatalf("%s lists no accepted repo form, so this test is checking nothing", interfacePage)
	}
}

// The three warnings this documentation set owes are not three of the same
// thing, and the placement rule is the design's: **a warning about the
// neighbour goes where the neighbour is configured.** Each is checked where it
// belongs and nowhere else.
func TestEachRequiredWarningStandsWhereItsSubjectIsConfigured(t *testing.T) {
	t.Parallel()

	for _, warning := range []struct{ file, phrase, why string }{
		{"compose.yaml", "WRITE_COALESCE_MS", "ignis's write coalescing is set on the ignis service, which is where it is configured"},
		{"README.md", "Headless Sync", "whether you run it decides whether obsync is for you at all, which is the fit section's question"},
		{"docs/credentials.md", "http://", "the plain-http warning stands beside the credential it sends in the clear"},
	} {
		if !strings.Contains(flattened(t, warning.file), warning.phrase) {
			t.Errorf("%s does not carry the %q warning: %s (§11)", warning.file, warning.phrase, warning.why)
		}
	}
}

// flattened is a document with its line wrapping and its blockquote furniture
// taken out, which is what makes a phrase a page carries findable. Every
// document here wraps at 80 columns, so a sentence a reader sees as one line is
// two in the file, and a check that asked for it verbatim would be a check on
// where the wrap happened to fall.
func flattened(t *testing.T, path string) string {
	t.Helper()

	return strings.Join(strings.Fields(strings.ReplaceAll(readDocument(t, path), ">", " ")), " ")
}

func readDocument(t *testing.T, path string) string {
	t.Helper()

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s, which obsync's documentation set names: %v", path, err)
	}
	return string(source)
}
