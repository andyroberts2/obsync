package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The never-list, checked rather than remembered.
//
// The rules are phrased as absolutes — "never checkout after bootstrap" rather
// than "checkout only in situation X" — specifically so that a grep can decide
// them, and git subcommand names are kept as plain string literals at the call
// site so that the grep is a proof rather than a hint. This is that grep, run
// by the suite, because a proof nobody runs is a convention.
//
// It reads obsync's own source in process, which is load-bearing rather than
// incidental: `go test` keys its result cache on the files a test opens, so
// reading them here is what makes a source file that broke one of these rules
// re-run this test rather than return a cached pass.

// forbidden is every argv obsync promises never to hand git, spelled as the Go
// string literal it would have to be written as. Each names the promise it
// keeps and, where the promise has one, the file where the single exception
// lives.
var forbidden = []struct {
	literal string
	only    string
	named   string
	promise string
}{
	{literal: `"checkout"`, named: "**runs `git checkout` after bootstrap**", promise: "obsync never runs git checkout after bootstrap: checking a " +
		"branch out rewrites the working tree under a live Obsidian with files open (§3)"},
	{literal: `"rebase"`, named: "**rebases**", promise: "obsync never rebases: a rebase walks the vault through one " +
		"checkout per replayed commit while ignis is still writing into it (§3)"},
	{literal: `"--force"`, named: "not `--force`", promise: "obsync never force-pushes, unconditionally and with no flag " +
		"to turn it off (§3)"},
	{literal: `"--force-with-lease"`, named: "not `--force-with-lease`", promise: "obsync never force-pushes, and --force-with-lease " +
		"is named separately because it is the one someone reaches for when --force is refused (§3)"},
	{literal: `"-f"`, named: "**force-pushes**", promise: "obsync never force-pushes, in any spelling (§3)"},
	{literal: `"--hard"`, named: "**hard-resetting**", promise: "obsync never discards history: following a rewritten remote by " +
		"hard-resetting onto it is the mirror image of force-pushing (§3)"},
	{literal: `"stash"`, named: "**stashes**", promise: "obsync never stashes: a stash reverts the working tree to HEAD, " +
		"so the human's most recent edits would vanish out of their open vault for the duration " +
		"of a merge (§3)"},
	{literal: `"set-url"`, named: "never runs `git remote set-url`", promise: "obsync never re-points a remote, and never writes the vault's " +
		".git/config at all (§8)"},
	{literal: `"fsck"`, named: "**runs `git fsck`**", promise: "obsync never runs git fsck, at startup or at any cadence: damage " +
		"is found by working, never by scanning (§7)"},
	{literal: `"clone"`, only: "internal/git/bootstrap.go", named: "**re-clones or self-repairs a damaged repo**", promise: "obsync clones once, at " +
		"bootstrap, into a directory that holds no repo; it never re-clones, because a re-clone " +
		"discards exactly the commits obsync exists to have made (§7)"},
}

func TestObsyncNeverHandsGitTheArgvItPromisedNotTo(t *testing.T) {
	t.Parallel()

	for path, source := range obsyncSource(t) {
		for _, rule := range forbidden {
			if !strings.Contains(source, rule.literal) {
				continue
			}
			if rule.only != "" && path == rule.only {
				continue
			}
			t.Errorf("%s contains %s. %s", path, rule.literal, rule.promise)
		}
	}
}

// The one exception is one, and it is where it says it is. A promise with an
// exception is only checkable while the exception is a single named place.
func TestTheOneCloneIsTheOneBootstrapMakes(t *testing.T) {
	t.Parallel()

	clones := 0
	for _, source := range obsyncSource(t) {
		clones += strings.Count(source, `"clone"`)
	}
	if clones != 1 {
		t.Errorf("obsync's source names the clone subcommand %d times, want exactly one — the "+
			"bootstrap case that makes an empty directory a copy of the remote (§3, §7)", clones)
	}
}

// The list is a promise to an operator before it is a check on a maintainer, so
// the page they read it on and the check that enforces it may not drift apart.
// §10 puts the promise at the front door (user story 63) and docs/interface.md
// points at it by name; this pins that pointer to something, the same direction
// interface_test.go pins the ignore floor in.
//
// One direction only, deliberately: the README's never-list is broader than a
// grep can decide — it also promises not to overwrite a conflict copy or exit on
// a sync failure — so what is checkable is that every rule this file enforces is
// stated there, not that nothing else is.
func TestEveryArgvTheNeverListEnforcesIsOnThePageAnOperatorReads(t *testing.T) {
	t.Parallel()

	list := neverListOnThePage(t)
	for _, rule := range forbidden {
		if !strings.Contains(list, rule.named) {
			t.Errorf("the README's never-list does not say %q, which %s is the enforcement of. %s",
				rule.named, rule.literal, rule.promise)
		}
	}
}

// neverListOnThePage is the README's never-list section and nothing else, so
// that "it is on the list" is what is asserted rather than "the word appears
// somewhere in the README".
func neverListOnThePage(t *testing.T) string {
	t.Helper()

	const heading = "## What obsync will never do"
	source, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading the README the never-list is promised on: %v", err)
	}
	_, after, found := strings.Cut(string(source), heading)
	if !found {
		t.Fatalf("the README carries no %q section, and docs/interface.md points an operator at "+
			"it by name: the list is the front door this project asks to be trusted at (§10)", heading)
	}
	list, _, _ := strings.Cut(after, "\n## ")
	return list
}

// obsyncSource is every Go file obsync ships, by path relative to the module
// root. Test files are not among them: a test that drives a human's own git in
// a vault is a test doing what a human does, and the never-list is about what
// obsync does.
func obsyncSource(t *testing.T) map[string]string {
	t.Helper()

	source := map[string]string{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && path != "." && strings.HasPrefix(entry.Name(), "."):
			return fs.SkipDir
		case entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source[filepath.ToSlash(path)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("reading obsync's own source: %v", err)
	}
	if len(source) < 2 {
		t.Fatalf("found %d source files to check the never-list against, want obsync's own", len(source))
	}
	return source
}
