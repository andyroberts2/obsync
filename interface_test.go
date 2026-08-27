package main

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/andyroberts2/obsync/internal/vault"
)

// docs/interface.md is the canonical statement of the declared surface (§10),
// and these tests are the half of it that can be checked rather than reviewed:
// the config surface and the subcommands are closed lists, so the page and the
// running binary can be made to disagree out loud.
//
// The design deliberately declined to *generate* the page — half the surface
// is not a struct, and a half-generated page is worse than either pure form —
// and CI's release check catches a page that moved without a "Surface changes"
// note. Neither of those catches the opposite drift: a knob or a subcommand the
// binary grew and the page never heard about. That is what is checked here, and
// it is checked through the startup line, which exists to be diffed against the
// page (§8) — so this is seam 1's observable output on one side and the page's
// own words on the other, and never obsync's internals.
//
// The page is read in process rather than through a subprocess, which is
// load-bearing rather than incidental: `go test` keys its result cache on the
// files a test itself opens, so an edit to the page re-runs these tests.

const interfacePage = "docs/interface.md"

// surfaceBlock is a complete environment block, one value per variable the page
// declares. It is written out here rather than derived from the page because
// only a human knows what a valid value for a new knob looks like — so a knob
// added to the page fails this test until a row is added, which is the
// row-per-case rule §10's closed tables carry everywhere else.
func surfaceBlock(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{
		"OBSYNC_REPO":         "ssh://git@git.lan/owner/vault.git",
		"OBSYNC_VAULT_PATH":   "/vaults/notes",
		"OBSYNC_BRANCH":       "vault-live",
		"OBSYNC_TOKEN_FILE":   credentialFile(t, "a-token"),
		"OBSYNC_USERNAME":     "oauth2",
		"OBSYNC_SIZE_CEILING": "40MB",
		"OBSYNC_AUTHOR_NAME":  "vault-bot",
		"OBSYNC_AUTHOR_EMAIL": "vault-bot@example.invalid",
		"OBSYNC_LOG_LEVEL":    "debug",
	}
}

// The config surface is nine variables and one required, and the page is where
// that is promised. A tenth knob, a renamed one, or one dropped from either
// side shows up here as the page and the startup line naming different things.
func TestTheDeclaredSurfacePageAndTheStartupLineNameTheSameKnobs(t *testing.T) {
	t.Parallel()

	block := surfaceBlock(t)
	declared := firstColumn(t, "Variable")

	for _, variable := range declared {
		if _, ok := block[variable]; !ok {
			t.Fatalf("%s declares %s and this test has no value for it — add a row to "+
				"surfaceBlock, because a knob nothing sets is a knob nothing checks",
				interfacePage, variable)
		}
	}
	for variable := range block {
		if !slices.Contains(declared, variable) {
			t.Fatalf("obsync reads %s and %s does not declare it; the config surface is part of "+
				"the declared surface (§10), so a knob that is not on the page is not on the surface",
				variable, interfacePage)
		}
	}

	loop := startLoop(t, environBlock(block)...)
	keys := logfmtKeys(loop.awaitLine(startupLine))

	// The page's own claim about the keys: the variable name, lowercased and
	// without the prefix. It is what makes the line diffable against a compose
	// file, and it is on the surface for that reason.
	page := pageSource(t)
	for _, variable := range declared {
		key := startupKey(variable)
		if !slices.Contains(keys, key) {
			t.Errorf("the startup line carries %v, want a %q key for %s — the line is what an "+
				"operator diffs against %s (§8)", keys, key, variable, interfacePage)
		}
	}
	for _, key := range keys {
		variable := "OBSYNC_" + strings.ToUpper(key)
		if slices.Contains(declared, variable) {
			continue
		}
		// A key that is not a variable is allowed — `remote` is the
		// normalisation gate 5 compares — but only if the page says what it
		// carries.
		if !strings.Contains(page, "`"+key+"`") {
			t.Errorf("the startup line carries a %q key and %s never names it; every field on that "+
				"line is on the surface, because the line exists to be diffed against the page",
				key, interfacePage)
		}
	}

	if stderr := loop.stderr(); strings.Contains(stderr, "unknown variable") {
		t.Errorf("obsync wrote %q for a block holding nothing but the variables %s declares, want "+
			"every name on the page to be one obsync reads", stderr, interfacePage)
	}
}

// A variable's default is as much of the declared surface as its name is (§10),
// and it is the half an operator actually reads: `95MB` and `/vault` are what
// they decide not to set. Nothing else pins the page's Default column — the
// suite pins obsync's defaults against literals of its own, so the page and the
// binary can move apart without either being wrong on its own terms. Measured:
// with the page's `/vault` changed to `/vaults` and its `95MB` to `90MB`, the
// whole suite stays green.
//
// The rule is total rather than partial, so the page cannot dodge it by turning
// a value into prose: a Default cell stating a value must be the value obsync
// echoes, and a cell stating anything else — an em dash, or "resolved at
// startup" — means obsync echoes nothing there.
func TestTheDeclaredSurfacePageStatesTheDefaultsObsyncResolves(t *testing.T) {
	t.Parallel()

	block := surfaceBlock(t)
	rows := variableRows(t)

	// Only the variables the page marks required are set, so every other key
	// on the startup line is a default obsync resolved for itself.
	required := make(map[string]string)
	for _, row := range rows {
		variable := strings.Trim(row[0], "`")
		if !isRequired(row) {
			continue
		}
		value, ok := block[variable]
		if !ok {
			t.Fatalf("%s marks %s required and this test has no value for it — add a row to "+
				"surfaceBlock", interfacePage, variable)
		}
		required[variable] = value
	}

	fields := logfmtFields(startLoop(t, environBlock(required)...).awaitLine(startupLine))

	for _, row := range rows {
		variable := strings.Trim(row[0], "`")
		if _, isSet := required[variable]; isSet {
			continue
		}
		key := startupKey(variable)
		want, stated := codeSpan(row[2])
		got, echoed := fields[key]
		if !echoed {
			// The knobs test above owns this direction; failing twice for one
			// cause buries the message that names the fix.
			continue
		}
		switch {
		case stated && got != want:
			t.Errorf("%s says %s defaults to %q and obsync resolved %q — the defaults are on the "+
				"declared surface (§10), and an operator who does not set a knob reads the page "+
				"to find out what they got", interfacePage, variable, want, got)
		case !stated && got != "":
			t.Errorf("obsync resolved %s to %q and %s states no default for it (%q) — a knob with "+
				"a value the page does not state is a promise nobody made", variable, got,
				interfacePage, row[2])
		}
	}
}

// "Nine environment variables, one required" is the page's headline claim about
// the config surface, and the table's Required column is where it is stated per
// row. Both halves are checked against the binary: the run above starts obsync
// on the required variables alone, which is what proves no row marked otherwise
// is secretly required, and this drives the rows that say "yes".
//
// A cell that is neither is a condition rather than a claim — OBSYNC_TOKEN_FILE
// is required iff the remote is http(s), which has its own row-per-case suite
// in config_test.go — so it is counted here and driven there.
func TestTheDeclaredSurfacePageStatesWhichVariablesAreRequired(t *testing.T) {
	t.Parallel()

	block := surfaceBlock(t)
	var required []string
	for _, row := range variableRows(t) {
		if isRequired(row) {
			required = append(required, strings.Trim(row[0], "`"))
		}
	}
	if len(required) != 1 {
		t.Fatalf("%s marks %d variables required (%s), want the one §8 puts on the exit path — "+
			"OBSYNC_REPO is the only value nothing can infer", interfacePage, len(required),
			strings.Join(required, ", "))
	}

	for _, variable := range required {
		t.Run(variable, func(t *testing.T) {
			t.Parallel()

			without := maps.Clone(block)
			delete(without, variable)
			_, stderr, exitCode := runObsync(t, environBlock(without))

			if exitCode == 0 {
				t.Errorf("obsync exited 0 with %s unset and %s marks it required; a missing "+
					"required variable is a config error decidable from the environment block "+
					"alone, so obsync exits 1 rather than parking (§8)", variable, interfacePage)
			}
			if !strings.Contains(stderr, variable) {
				t.Errorf("obsync refused a block with no %s and wrote %q, want it to name the "+
					"variable — an operator fixes what obsync names (§8)", variable, stderr)
			}
		})
	}
}

// The other direction on the subcommands, and the one nothing else covers: a
// subcommand the binary grew that the page never heard about. Measured — adding
// a fifth subcommand to run's case list and to the usage obsync prints leaves
// the whole suite green, because the table below only drives the page's rows
// through the binary and counts the page's own.
//
// Usage is obsync's own statement of its subcommands and the only enumeration
// of them observable at seam 1, which is what makes this checkable at all; a
// subcommand added to run and to nothing else is beyond what any test at this
// seam can see.
func TestEverySubcommandObsyncAdvertisesIsDeclaredOnThePage(t *testing.T) {
	t.Parallel()

	declared := firstColumn(t, "Subcommand")
	// The probe is a name no subcommand could take, so obsync answers it with
	// its usage rather than recognising it; a probe that grew into a real
	// subcommand would leave this test measuring nothing.
	_, stderr, _ := runObsync(t, nil, "not-a-subcommand")

	advertised := 0
	for _, line := range strings.Split(stderr, "\n") {
		// A usage line is the indented binary name; the subcommand, when there
		// is one, is the single space after it and then a word. The default
		// subcommand's line has its description in that column instead, and is
		// the page's one row with no name to type.
		rest, isUsage := strings.CutPrefix(line, "  obsync ")
		if !isUsage {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		if name == "" {
			continue
		}
		advertised++
		if !slices.Contains(declared, name) {
			t.Errorf("obsync advertises a %q subcommand and %s declares %s — the subcommands are "+
				"a closed list of four (§10), so a fifth is a surface change rather than an "+
				"addition", name, interfacePage, strings.Join(declared, ", "))
		}
	}

	named := 0
	for _, subcommand := range declared {
		if !strings.HasPrefix(subcommand, "_") {
			named++
		}
	}
	if advertised != named {
		t.Errorf("obsync advertises %d subcommands and %s declares %d beside the default; the page "+
			"and the binary state one surface or neither states it", advertised, interfacePage, named)
	}
}

// variableRows returns the body rows of the page's config-surface table, each
// carrying the four things §10 promises about a knob.
func variableRows(t *testing.T) [][]string {
	t.Helper()

	rows := pageTable(t, "Variable")
	for _, row := range rows {
		if len(row) < 4 {
			t.Fatalf("%s states %s in %d columns, want Variable, Required, Default and Accepted "+
				"form — a knob's required-ness and its default are as much of the declared "+
				"surface as its name (§10)", interfacePage, row[0], len(row))
		}
	}
	return rows
}

// isRequired reports whether a Variable row's Required cell is the page saying
// obsync will not start without it. Anything that is not that word is a
// condition or a "no", and neither promises an unconditional refusal.
func isRequired(row []string) bool {
	return strings.Trim(row[1], "*") == "yes"
}

// startupKey is the logfmt key a variable is echoed under: the name, lowercased
// and without the prefix. The page states that rule, and the knobs test above
// checks obsync keeps it.
func startupKey(variable string) string {
	return strings.ToLower(strings.TrimPrefix(variable, "OBSYNC_"))
}

// environBlock renders an environment block in the form the process boundary
// takes it.
func environBlock(block map[string]string) []string {
	environ := make([]string, 0, len(block))
	for variable, value := range block {
		environ = append(environ, variable+"="+value)
	}
	return environ
}

// §10's subcommands are a closed list of four: the default, healthcheck, status
// and credential-helper. A fifth is a surface change rather than an addition,
// which is why the count is asserted as well as the names.
func TestTheDeclaredSurfacePageNamesEverySubcommand(t *testing.T) {
	t.Parallel()

	declared := firstColumn(t, "Subcommand")
	if len(declared) != 4 {
		t.Fatalf("%s declares %d subcommands (%s), want the closed list of four (§10) — a fifth is "+
			"a surface change", interfacePage, len(declared), strings.Join(declared, ", "))
	}

	named := 0
	for _, subcommand := range declared {
		if strings.HasPrefix(subcommand, "_") {
			// The default subcommand has no name to type. It is driven by
			// TestTheSyncLoopRunsUntilSIGTERM, which is the only test that can
			// see a contract whose whole content is not finishing.
			continue
		}
		named++
		_, stderr, _ := runObsync(t, nil, subcommand)
		if strings.Contains(stderr, "unknown subcommand") {
			t.Errorf("%s declares %q and obsync refused it as a subcommand that does not exist",
				interfacePage, subcommand)
		}
	}
	if named != 3 {
		t.Errorf("%s names %d subcommands beside the default, want 3 — the default is the row with "+
			"no name to type", interfacePage, named)
	}
}

// pageTable returns the body rows of the page's table whose first column
// carries the given header, each row split into whitespace-trimmed cells.
func pageTable(t *testing.T, header string) [][]string {
	t.Helper()

	var rows [][]string
	inTable := false
	for _, line := range strings.Split(pageSource(t), "\n") {
		row, isRow := strings.CutPrefix(strings.TrimSpace(line), "|")
		if !isRow {
			inTable = false
			continue
		}
		cells := strings.Split(strings.TrimSuffix(row, "|"), "|")
		for i, cell := range cells {
			cells[i] = strings.TrimSpace(cell)
		}
		switch first := strings.Trim(cells[0], "`"); {
		case first == header:
			inTable = true
		case !inTable, strings.HasPrefix(first, "---"):
		default:
			rows = append(rows, cells)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no table under a %q column; the declared surface is stated in four parts "+
			"and this is one of them", interfacePage, header)
	}
	return rows
}

// firstColumn returns the first cell of every body row of the page's table with
// the given header, stripped of the backticks Markdown puts round a literal.
func firstColumn(t *testing.T, header string) []string {
	t.Helper()

	rows := pageTable(t, header)
	cells := make([]string, len(rows))
	for i, row := range rows {
		cells[i] = strings.Trim(row[0], "`")
	}
	return cells
}

// codeSpan returns the contents of a Markdown code span and whether the cell
// was one. It is what tells a cell stating a value — `95MB` — from a cell
// stating that there is none, whether it says so with an em dash or with prose.
func codeSpan(cell string) (string, bool) {
	value, ok := strings.CutPrefix(cell, "`")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(value, "`")
}

func pageSource(t *testing.T) string {
	t.Helper()

	// The test binary runs in its package's directory, which is the repo root
	// while obsync is one package.
	source, err := os.ReadFile(interfacePage)
	if err != nil {
		t.Fatalf("reading %s: %v", interfacePage, err)
	}
	return string(source)
}

// logfmtFields returns obsync's own attributes from one logfmt line, keyed by
// name, dropping the three the handler writes for every line.
//
// It splits on unquoted spaces rather than on every space, because a value
// holding one is quoted rather than broken across fields — the same reason
// obsync never splits git's output on a byte a vault path may contain. A quote
// inside a quoted value arrives escaped and closes nothing, which matters for
// the same reason: a knob's value is whatever an operator typed into a compose
// file.
func logfmtFields(line string) map[string]string {
	fields := make(map[string]string)
	quoted, escaped := false, false
	field := strings.Builder{}
	flush := func() {
		key, value, ok := strings.Cut(field.String(), "=")
		field.Reset()
		switch {
		case !ok, key == "time", key == "level", key == "msg":
			return
		}
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		}
		fields[key] = value
	}
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			flush()
			continue
		}
		field.WriteRune(r)
	}
	flush()
	return fields
}

// logfmtKeys returns the names logfmtFields found, sorted so a failure message
// reads the same way twice.
func logfmtKeys(line string) []string {
	return slices.Sorted(maps.Keys(logfmtFields(line)))
}

// The ignore floor's contents are part of the declared surface: changing them
// silently changes what a user's repo holds (§5, §10). This commit gave obsync
// a second copy of that list in code, and two copies of a promise is a promise
// that drifts — the same argument the git floor's one-file rule makes. So the
// page and `vault.IgnoreFloor` are pinned to each other, in order, entry for
// entry.
func TestTheIgnoreFloorOnThePageIsTheOneObsyncCarries(t *testing.T) {
	t.Parallel()

	got, want := fencedBlockAfter(t, "### The ignore floor"), vault.IgnoreFloor
	if !slices.Equal(got, want) {
		t.Errorf("%s lists the ignore floor as %v, and obsync carries %v: the floor is one closed "+
			"list on the declared surface, so the page and the code state it once each and never "+
			"differently (§5, §10)", interfacePage, got, want)
	}
}

// The refused-path list is on the surface for the same reason the floor is, and
// this page said so in as many words: changing either silently changes what a
// user's repo contains. So it is pinned the same way, entry for entry.
//
// The page writes it several names to a line, which is how an operator reads a
// closed list of filenames rather than a configuration file — so the block is
// read back as entries rather than as lines.
func TestTheRefusedPathListOnThePageIsTheOneObsyncCarries(t *testing.T) {
	t.Parallel()

	var got []string
	for _, line := range fencedBlockAfter(t, "### Refused paths") {
		for _, entry := range strings.Split(line, ",") {
			got = append(got, strings.TrimSpace(entry))
		}
	}
	if !slices.Equal(got, vault.RefusedPaths) {
		t.Errorf("%s lists the refused paths as %v, and obsync carries %v: the list is one closed "+
			"list on the declared surface, so the page and the code state it once each and never "+
			"differently (§5, §10)", interfacePage, got, vault.RefusedPaths)
	}
}

// fencedBlockAfter is the lines of the first ``` block below a heading, which
// is how the page states a closed list an operator reads as one.
func fencedBlockAfter(t *testing.T, heading string) []string {
	t.Helper()

	_, below, found := strings.Cut(pageSource(t), heading+"\n")
	if !found {
		t.Fatalf("%s has no %q heading", interfacePage, heading)
	}
	_, below, found = strings.Cut(below, "```\n")
	if !found {
		t.Fatalf("%s has no fenced block below %q", interfacePage, heading)
	}
	block, _, found := strings.Cut(below, "```")
	if !found {
		t.Fatalf("%s has an unclosed fenced block below %q", interfacePage, heading)
	}
	return strings.FieldsFunc(block, func(r rune) bool { return r == '\n' })
}
