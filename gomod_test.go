package main

import (
	"os"
	"strings"
	"testing"
)

// The dependency rule and the missing toolchain directive are part of this
// project's deliverable rather than notes about it, so they are checked here
// as well as written down in go.mod: a third direct dependency, or a toolchain
// directive added by a `go get` that felt harmless, fails the suite.
//
// go.mod is parsed by hand rather than with golang.org/x/mod, which would make
// the file that records the two-dependency rule the file that broke it.

func TestGoModDeclaresAtMostTwoDirectDependencies(t *testing.T) {
	t.Parallel()

	direct := directDependencies(t)
	if len(direct) > 2 {
		t.Errorf("go.mod declares %d direct dependencies (%s), want at most two — "+
			"a filesystem-notification library and golang.org/x/sys (§1). A third is argued for in "+
			"the commit that adds it, and that argument includes this line moving.",
			len(direct), strings.Join(direct, ", "))
	}
}

func TestGoModCarriesNoToolchainDirective(t *testing.T) {
	t.Parallel()

	for _, line := range directives(t) {
		if strings.HasPrefix(line, "toolchain ") {
			t.Errorf("go.mod carries %q; a toolchain directive triggers a download, "+
				"which makes a pinned release builder not pinned (§12)", line)
		}
	}
}

// directDependencies returns the module paths go.mod requires without an
// "// indirect" marker.
func directDependencies(t *testing.T) []string {
	t.Helper()

	var direct []string
	inRequireBlock := false
	for _, line := range directives(t) {
		switch {
		case line == "require (":
			inRequireBlock = true
		case inRequireBlock && line == ")":
			inRequireBlock = false
		case inRequireBlock:
			if module, ok := requirement(line); ok {
				direct = append(direct, module)
			}
		case strings.HasPrefix(line, "require "):
			if module, ok := requirement(strings.TrimPrefix(line, "require ")); ok {
				direct = append(direct, module)
			}
		}
	}
	return direct
}

// requirement reports the module path of one require line, and whether that
// requirement is a direct one.
func requirement(line string) (string, bool) {
	if strings.Contains(line, "// indirect") {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// directives returns go.mod's lines with blank lines and whole-line comments
// dropped, each trimmed of surrounding space.
func directives(t *testing.T) []string {
	t.Helper()

	// The test binary runs in its package's directory, which is the module root
	// while obsync is one package; the module file is the thing under test.
	source, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	var lines []string
	for _, raw := range strings.Split(string(source), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
