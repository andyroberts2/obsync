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

// parseStatus reads `git status --porcelain=v2 -z -uall`.
//
// The record layout is git's, and the reason obsync asks for this format rather
// than the readable one is the vault: a note title carries spaces and unicode
// and may legally contain a newline, so a path is only unambiguous when it ends
// at a NUL and is the last field of its record.
func parseStatus(out []byte) ([]string, error) {
	records := splitNUL(out)
	var paths []string
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
			paths = append(paths, path)
		case '2':
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<origPath>
			// Both names are staged: a rename is a delete plus an add, and the
			// delete is at a path this record does not otherwise mention.
			path, err := fieldAfter(record, 9)
			if err != nil {
				return nil, err
			}
			i++
			if i >= len(records) {
				return nil, fmt.Errorf("git status reported a rename of %q with no original path", path)
			}
			paths = append(paths, path, records[i])
		case 'u':
			// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
			path, err := fieldAfter(record, 10)
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
		case '?':
			paths = append(paths, record[len("? "):])
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
