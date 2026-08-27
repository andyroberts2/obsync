package git

import (
	_ "embed"
	"strconv"
	"strings"
)

// gitFloorFile is the git floor, and it is one file that both the binary and CI
// read (§12, the testing decisions).
//
// Embedded here for gate 7 and read by `.github/workflows/ci.yml` to build the
// matrix's lower point, so the version obsync refuses below is the version its
// tests actually ran on. Two independently-typed numbers is a promise a human
// keeps, and a promise a human keeps is not a definition: drift has to be
// structurally impossible rather than merely detected.
//
// The number is 2.38.5 because `git merge-tree --write-tree` landed in 2.38 and
// §4's whole out-of-tree merge stands on it, and because the floor is defined
// as *the oldest git obsync's tests run against* rather than read off a release
// note — which is what keeps it from becoming an aspiration. The file holds a
// bare version and nothing else, because a workflow reads it with `cat` and a
// format needs two readers to agree on it.
//
//go:embed GIT_FLOOR
var gitFloorFile string

// GitFloor is the oldest git obsync runs on.
var GitFloor = strings.TrimSpace(gitFloorFile)

// gitAtOrAboveTheFloor is gate 7: the git obsync is driving is at or above the
// git floor.
//
// Below the floor obsync's plumbing is not there to be driven at all, and the
// failure without this gate is not a clean one — `merge-tree --write-tree` on a
// git that has no such option exits with git's everything-code, so the first
// divergence a vault ever has would present as an unexplained failed run rather
// than as the one sentence that explains every run after it.
//
// `git --version` is the one git output obsync reads that has no machine form,
// because there is not one to ask for: git-version(1) defines the line, and it
// is `git version <number>` with an optional suffix a distribution may add. So
// the number is taken as the leading digits-and-dots of the third field and the
// suffix is discarded, which is what makes `2.39.5 (Apple Git-154)` a version
// rather than a parse failure.
//
// A version obsync cannot read at all passes the gate rather than failing it,
// and that is deliberate: the gate is a *conclusive fact*, and a string obsync
// does not recognise is not conclusive evidence that git is too old. The
// commands that need the floor announce themselves the moment obsync needs
// them, which is the same reasoning that keeps `fsck` out of the design.
func (r *Repo) gitAtOrAboveTheFloor() (*GateFailure, error) {
	out, err := r.run(invocation{dir: r.vault, args: []string{"--version"}})
	if err != nil {
		return nil, err
	}
	version := gitVersion(string(out))
	if version == "" {
		r.log.Debug("obsync could not read a version out of git's own version line, so gate 7 has "+
			"no conclusive fact to refuse on", "said", strings.TrimSpace(string(out)))
		return nil, nil
	}
	if !below(version, GitFloor) {
		return nil, nil
	}
	return &GateFailure{
		Gate: freezeGitBelowTheFloor,
		Fact: "the git obsync is driving is " + version + " and obsync's floor is " + GitFloor,
		Remedy: "run an image carrying git " + GitFloor + " or newer. obsync computes every merge " +
			"outside the vault with `git merge-tree --write-tree`, which is what fixes the floor " +
			"where it is, and a vault synced by a git without it would meet that as an " +
			"unexplained failure at its first divergence" + SelfClearing,
	}, nil
}

// gitVersion is the number out of `git version <number>[ <suffix>]`, or empty
// when the line is not that shape.
func gitVersion(line string) string {
	line, _, _ = strings.Cut(line, "\n")
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "git version ")
	if !found {
		return ""
	}
	number := strings.TrimSpace(rest)
	if cut := strings.IndexFunc(number, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	}); cut >= 0 {
		number = number[:cut]
	}
	if _, err := strconv.Atoi(strings.SplitN(number, ".", 2)[0]); err != nil {
		return ""
	}
	return number
}

// below compares two dotted version numbers field by field, a missing field
// counting as zero, and reports whether the first is older than the second.
//
// A field that is not a number makes the comparison stop there and answer "not
// older", for gate 7's own reason: what is not conclusively old is not refused.
func below(version, floor string) bool {
	have, want := strings.Split(version, "."), strings.Split(floor, ".")
	for i := 0; i < len(have) || i < len(want); i++ {
		mine, err := field(have, i)
		if err != nil {
			return false
		}
		theirs, err := field(want, i)
		if err != nil {
			return false
		}
		if mine != theirs {
			return mine < theirs
		}
	}
	return false
}

func field(fields []string, i int) (int, error) {
	if i >= len(fields) || fields[i] == "" {
		return 0, nil
	}
	return strconv.Atoi(fields[i])
}
