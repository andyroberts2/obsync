// Package vault holds what obsync knows about a vault directory itself, as
// opposed to the repository inside it: the ignore floor, and the one question
// bootstrap asks of a directory that holds no repo (§5, §7's gate 2).
//
// Nothing here runs git. A directory obsync has not adopted yet has no repo to
// ask, which is exactly why gate 2 has to be answerable by looking.
package vault

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IgnoreFloor is the fixed set of paths obsync excludes from the vault (§5).
// It is a default rather than a rule — the vault's own .gitignore outranks it,
// and that file belongs to the user alone — and its contents are part of the
// declared surface, because changing them silently changes what a user's repo
// holds (§10, docs/interface.md).
//
// The entries are gitignore patterns, in the order they are written to the
// repo's exclude file. Writing that file is #28's; what this package needs
// them for is gate 2, which has to tell "a directory holding a vault" from "a
// directory holding the cruft a volume accumulates".
var IgnoreFloor = []string{
	".obsidian/workspace.json",
	".obsidian/workspace-mobile.json",
	".obsidian/workspaces.json",
	".obsidian/plugins/*/data.json",
	".trash/",
	".DS_Store",
	"Thumbs.db",
	".vscode/",
	".idea/",
	".obsidian-git-data",
	"obsync-attention.md",
}

// ShowsMoreThanIgnoreFloor reports whether a directory holds anything the
// ignore floor does not cover, and names the first such path.
//
// It is half of gate 2: a directory that holds no repo and shows more than the
// floor is one obsync refuses to adopt, because it cannot reason about a folder
// someone else's tool may own (§7). A directory that shows nothing but floor
// entries is one obsync bootstraps into — `.DS_Store` on a volume that was once
// mounted on a Mac is not a vault, and refusing a deployment over it would be
// the floor's whole purpose inverted.
//
// Only files count. git tracks files rather than directories, so a folder
// holding nothing shows nothing, and `.obsidian/` holding only a workspace file
// shows nothing either.
//
// It stops at the first path that shows: the answer is already conclusive, and
// walking the rest of what may be a hundred thousand notes to reach the same
// answer is a cost paid on every run of a frozen obsync.
func ShowsMoreThanIgnoreFloor(dir string) (bool, string, error) {
	shown := ""
	err := filepath.WalkDir(dir, func(entry string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, entry)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		// .git is the repo, and a directory holding one is the attach case
		// rather than this one. Skipped rather than refused so that the answer
		// this function gives does not depend on the caller having looked
		// first.
		if relative == ".git" {
			return fs.SkipDir
		}
		if info.IsDir() {
			if matchesIgnoreFloor(relative, true) {
				return fs.SkipDir
			}
			return nil
		}
		if matchesIgnoreFloor(relative, false) {
			return nil
		}
		shown = relative
		return fs.SkipAll
	})
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	return shown != "", shown, nil
}

// matchesIgnoreFloor reports whether a path relative to the vault root is
// covered by the ignore floor, under gitignore's own matching rules for the
// shapes the floor uses:
//
//   - an entry ending in / matches a directory and everything under it;
//   - an entry containing no / — before that trailing slash or otherwise —
//     matches that name at any depth;
//   - an entry containing a / is anchored to the vault root, and * matches
//     within one path segment.
//
// The floor is a closed list, so this is a reading of eleven known patterns
// rather than a gitignore implementation: negation, ** and character classes
// are not in it and are not handled.
//
// A trailing slash does not anchor, which is the one rule here that had to be
// measured rather than read: with the floor in an exclude file, git ignores
// Notes/.trash/ exactly as it ignores .trash/ (both matrix points). Anchoring
// it would make gate 2 refuse a directory the floor covers, which is the
// floor's whole purpose inverted (§5) — and #28 writes this same list into
// .git/info/exclude, where git is what applies it, so a floor with two readings
// is a floor that means two things.
func matchesIgnoreFloor(relative string, isDir bool) bool {
	relative = filepath.ToSlash(relative)
	for _, entry := range IgnoreFloor {
		directory, namesDirectory := strings.CutSuffix(entry, "/")
		if !namesDirectory {
			if covers(entry, relative) {
				return true
			}
			continue
		}
		if isDir && covers(directory, relative) {
			return true
		}
		// ...and everything under it. The walk skips a directory that matched,
		// so nothing reaches here today; the rule is stated in full anyway,
		// because a partial one that is only true because of how its one caller
		// walks is the next caller's surprise.
		for ancestor := path.Dir(relative); ancestor != "."; ancestor = path.Dir(ancestor) {
			if covers(directory, ancestor) {
				return true
			}
		}
	}
	return false
}

// covers matches one gitignore pattern that names no directory against a path
// from the vault root: anchored and segment by segment when the pattern carries
// a /, which is what keeps * inside one segment as gitignore has it, and by
// name at any depth when it does not.
func covers(pattern, relative string) bool {
	if !strings.Contains(pattern, "/") {
		matched, err := path.Match(pattern, path.Base(relative))
		return err == nil && matched
	}
	patternSegments := strings.Split(pattern, "/")
	pathSegments := strings.Split(relative, "/")
	if len(patternSegments) != len(pathSegments) {
		return false
	}
	for i, segment := range patternSegments {
		matched, err := path.Match(segment, pathSegments[i])
		if err != nil || !matched {
			return false
		}
	}
	return true
}
