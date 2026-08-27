package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/andyroberts2/obsync/internal/git"
)

// The git floor is one file that both the binary and CI read (§12, #32).
//
// This is not a test of behaviour and does not pretend to be — gate 7 firing is
// seam 2's, because a git below the floor is a property of the image and the
// sandbox has no such git to run against. What it checks is the structural
// claim the design makes instead: **drift must be impossible, not merely
// detected.** Two independently-typed numbers is a promise a human keeps, and a
// promise a human keeps is not a definition.
//
// It is read here, in process, and that is load-bearing rather than incidental,
// for the reason gomod_test.go states: `go test` keys its result cache on the
// files a test itself opens, so reading them makes a changed file re-run this.
const gitFloorFile = "internal/git/GIT_FLOOR"

func TestTheGitFloorFileHoldsNothingButAVersion(t *testing.T) {
	t.Parallel()

	// A workflow reads this with `cat` and the binary embeds it, so anything
	// but a bare version is a format two readers would have to agree on — and
	// the whole point is that there is nothing to agree about.
	raw, err := os.ReadFile(gitFloorFile)
	if err != nil {
		t.Fatalf("reading %s: %v", gitFloorFile, err)
	}
	floor := strings.TrimSpace(string(raw))
	if floor != git.GitFloor {
		t.Errorf("%s holds %q and the binary embeds %q, want the same number", gitFloorFile, floor,
			git.GitFloor)
	}
	if floor == "" {
		t.Fatalf("%s is empty, and the git floor is what gate 7 refuses below", gitFloorFile)
	}
	for _, field := range strings.Split(floor, ".") {
		if _, err := strconv.Atoi(field); err != nil {
			t.Errorf("%s holds %q, want a bare dotted version and nothing else — no comment, no "+
				"prefix, no `v`: a workflow reads this file with `cat`", gitFloorFile, floor)
		}
	}
}

func TestCIReadsTheGitFloorFromThatOneFileRatherThanRepeatingIt(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("reading the CI workflow: %v", err)
	}
	if !strings.Contains(string(workflow), gitFloorFile) {
		t.Errorf("the CI workflow does not read %s, so the matrix's lower point and the version "+
			"gate 7 refuses below are two numbers rather than one (§12)", gitFloorFile)
	}
	if strings.Contains(string(workflow), git.GitFloor) {
		t.Errorf("the CI workflow spells the git floor %q itself; it is meant to read %s, so that "+
			"moving the floor is one edit and cannot leave the matrix testing a version obsync no "+
			"longer promises (§12)", git.GitFloor, gitFloorFile)
	}
}
