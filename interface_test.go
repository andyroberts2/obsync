package main

import (
	"os"
	"slices"
	"strings"
	"testing"
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

	var environ []string
	for variable, value := range block {
		environ = append(environ, variable+"="+value)
	}
	loop := startLoop(t, environ...)
	keys := logfmtKeys(loop.awaitLine(startupLine))

	// The page's own claim about the keys: the variable name, lowercased and
	// without the prefix. It is what makes the line diffable against a compose
	// file, and it is on the surface for that reason.
	page := pageSource(t)
	for _, variable := range declared {
		key := strings.ToLower(strings.TrimPrefix(variable, "OBSYNC_"))
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

// firstColumn returns the first cell of every body row of the page's table with
// the given header, stripped of the backticks Markdown puts round a literal.
func firstColumn(t *testing.T, header string) []string {
	t.Helper()

	var cells []string
	inTable := false
	for _, line := range strings.Split(pageSource(t), "\n") {
		row, isRow := strings.CutPrefix(strings.TrimSpace(line), "|")
		if !isRow {
			inTable = false
			continue
		}
		first := strings.Trim(strings.TrimSpace(strings.Split(row, "|")[0]), "`")
		switch {
		case first == header:
			inTable = true
		case !inTable, strings.HasPrefix(first, "---"):
		default:
			cells = append(cells, first)
		}
	}
	if len(cells) == 0 {
		t.Fatalf("%s has no table under a %q column; the declared surface is stated in four parts "+
			"and this is one of them", interfacePage, header)
	}
	return cells
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

// logfmtKeys returns the keys obsync's own attributes carry on one logfmt line,
// dropping the three the handler writes for every line.
//
// It splits on unquoted spaces rather than on every space, because a value
// holding one is quoted rather than broken across fields — the same reason
// obsync never splits git's output on a byte a vault path may contain.
func logfmtKeys(line string) []string {
	var keys []string
	quoted := false
	field := strings.Builder{}
	flush := func() {
		key, _, ok := strings.Cut(field.String(), "=")
		field.Reset()
		switch {
		case !ok, key == "time", key == "level", key == "msg":
		default:
			keys = append(keys, key)
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
			field.WriteRune(r)
		case r == ' ' && !quoted:
			flush()
		default:
			field.WriteRune(r)
		}
	}
	flush()
	return keys
}
