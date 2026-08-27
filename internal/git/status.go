package git

import (
	"fmt"
	"strings"
)

// Kind is what happened to a path, as far as one commit is concerned. The three
// of them are what a commit message renders as +, ~ and - (§2).
type Kind int

const (
	Added Kind = iota
	Modified
	Deleted
)

// Change is one path in a commit, and its kind.
type Change struct {
	Path string
	Kind Kind
}

// ChangedPath is one path git reports as changed, plus the two facts that
// decide whether `git add` may be given it.
//
// Being changed and being addable are not the same question, and the
// difference is a run obsync cannot finish. A path a human took out of the
// index with `git rm --cached` while their .gitignore covers it is reported as
// changed — the commit carries the deletion, and the index already holds it in
// full — and there is nothing in the working tree to stage, because to git the
// file is now untracked and ignored. Naming such a path on an add is fatal: git
// refuses to add an ignored path and refuses the *whole* add with it, staging
// nothing (measured at both matrix points, exit 1). One of them would take
// every note in the vault out of the same commit, on every run from then on,
// because nothing clears the record until something commits it — which is the
// same shape as the rename record parseStatus reads the destination of, one
// column along.
type ChangedPath struct {
	Path string
	// InWorkingTree is git's second status column: the working tree against
	// the index. A "." there is a change the index already holds in full, and
	// an add naming it is a no-op at best.
	InWorkingTree bool
	// Untracked is a path git has no index entry for at all — status's `?`
	// record. It is the second way an add can be fatal to the whole commit,
	// and the one the settle guard's own answer settles: a pathspec matches
	// either a file on disk or an index entry, so a tracked path that was
	// deleted still stages its deletion (measured at both matrix points, exit
	// 0), while an untracked path that is no longer there matches nothing —
	// `fatal: pathspec did not match any files`, exit 128, and the whole add
	// refused with it (measured at both matrix points; --ignore-errors does
	// not soften it).
	Untracked bool
}

// parseStatus reads `git status --porcelain=v2 -z -uall`.
//
// The record layout is git's, and the reason obsync asks for this format rather
// than the readable one is the vault: a note title carries spaces and unicode
// and may legally contain a newline, so a path is only unambiguous when it ends
// at a NUL and is the last field of its record.
func parseStatus(out []byte) ([]ChangedPath, error) {
	records := splitNUL(out)
	var paths []ChangedPath
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		switch record[0] {
		case '1':
			// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			path, err := fieldAfter(record, 8)
			if err != nil {
				return nil, err
			}
			inWorkingTree, err := worktreeColumn(record)
			if err != nil {
				return nil, err
			}
			paths = append(paths, ChangedPath{Path: path, InWorkingTree: inWorkingTree})
		case '2':
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<origPath>
			//
			// Only the destination is a path to stage, and the original is
			// read solely so it is not mistaken for the next record. Measured
			// on a `git mv` a human ran in their own vault: the index already
			// holds the rename, so the original is in neither the index nor
			// the working tree, and a pathspec naming it matches no file —
			// `git add` exits 128, and because the record persists until
			// something commits it, that is every run from then on rather
			// than one. Staging the destination alone commits the delete and
			// the add both, which is what the index was already holding.
			path, err := fieldAfter(record, 9)
			if err != nil {
				return nil, err
			}
			inWorkingTree, err := worktreeColumn(record)
			if err != nil {
				return nil, err
			}
			i++
			if i >= len(records) {
				return nil, fmt.Errorf("git status reported a rename of %q with no original path", path)
			}
			paths = append(paths, ChangedPath{Path: path, InWorkingTree: inWorkingTree})
		case 'u':
			// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
			//
			// An unmerged path's XY names which side is missing rather than
			// what the working tree holds, so the add is given it and lets git
			// answer. It is gate 4's state anyway (#32).
			path, err := fieldAfter(record, 10)
			if err != nil {
				return nil, err
			}
			paths = append(paths, ChangedPath{Path: path, InWorkingTree: true})
		case '?':
			// ? <path>. Read with the same helper as every other record
			// rather than by slicing a fixed prefix off: a record too short to
			// hold one is a parse error, and slicing it would be a panic on
			// the sync-loop path.
			path, err := fieldAfter(record, 1)
			if err != nil {
				return nil, err
			}
			paths = append(paths, ChangedPath{Path: path, InWorkingTree: true, Untracked: true})
		case '!':
			// Only ever present with --ignored, which obsync does not pass.
			continue
		default:
			return nil, fmt.Errorf("git status reported a record obsync does not know: %q", record)
		}
	}
	return paths, nil
}

// parseNameStatus reads `git diff-index --cached --name-status -z`, whose
// fields alternate between a status letter and the path it applies to.
func parseNameStatus(out []byte) ([]Change, error) {
	fields := splitNUL(out)
	var changes []Change
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			continue
		}
		i++
		if i >= len(fields) {
			return nil, fmt.Errorf("git diff-index reported the status %q with no path", status)
		}
		path := fields[i]

		switch status[0] {
		case 'A':
			changes = append(changes, Change{Path: path, Kind: Added})
		case 'M', 'T', 'U':
			changes = append(changes, Change{Path: path, Kind: Modified})
		case 'D':
			changes = append(changes, Change{Path: path, Kind: Deleted})
		case 'R', 'C':
			// obsync asks for no rename or copy detection, so these arrive
			// only from a git that found some anyway. Both names are carried
			// rather than one, because the second path is a file the commit
			// really does add.
			i++
			if i >= len(fields) {
				return nil, fmt.Errorf("git diff-index reported %q from %q with no destination", status, path)
			}
			if status[0] == 'R' {
				changes = append(changes, Change{Path: path, Kind: Deleted})
			}
			changes = append(changes, Change{Path: fields[i], Kind: Added})
		default:
			return nil, fmt.Errorf("git diff-index reported a status obsync does not know: %q", status)
		}
	}
	return changes, nil
}

// splitNUL splits NUL-separated git output into its fields, dropping the empty
// field a trailing NUL leaves behind. It is the only splitting obsync does of
// git's output, and it never splits on a newline: a newline is a character a
// note title may contain.
func splitNUL(out []byte) []string {
	trimmed := strings.TrimSuffix(string(out), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

// worktreeColumn reads the second half of a changed record's XY field: git's
// answer for the working tree against the index, as against X, which is the
// index against HEAD. It is the one field that says whether an add has anything
// to do for this path (ChangedPath).
func worktreeColumn(record string) (bool, error) {
	rest, err := fieldAfter(record, 1)
	if err != nil {
		return false, err
	}
	xy, _, _ := strings.Cut(rest, " ")
	if len(xy) != 2 {
		return false, fmt.Errorf("git status reported a record obsync could not read: %q", record)
	}
	return xy[1] != '.', nil
}

// fieldAfter returns what follows the first n spaces of a status record — the
// path, which is the last field and the only one allowed to contain a space.
func fieldAfter(record string, n int) (string, error) {
	rest := record
	for range n {
		_, after, found := strings.Cut(rest, " ")
		if !found {
			return "", fmt.Errorf("git status reported a record obsync could not read: %q", record)
		}
		rest = after
	}
	if rest == "" {
		return "", fmt.Errorf("git status reported a record with no path: %q", record)
	}
	return rest, nil
}
