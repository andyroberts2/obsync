// Package vault holds what obsync knows about a vault directory itself, as
// opposed to the repository inside it: the ignore floor, the refusal layer, the
// settle guard, and the one question bootstrap asks of a directory that holds
// no repo (§5, §6, §7's gate 2).
//
// Nothing here runs git, and that is the shape of the package rather than an
// accident of it. A directory obsync has not adopted yet has no repo to ask,
// which is exactly why gate 2 has to be answerable by looking; and a file
// something else is still writing is a fact about the filesystem that git has
// no opinion about at all, which is why the settle guard is two stats and a
// gap.
package vault

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
)

// IgnoreFloor is the fixed set of paths obsync excludes from the vault (§5).
// It is a default rather than a rule — the vault's own .gitignore outranks it,
// and that file belongs to the user alone — and its contents are part of the
// declared surface, because changing them silently changes what a user's repo
// holds (§10, docs/interface.md).
//
// The entries are gitignore patterns, in the order they are written to the
// repo's exclude file at every startup — where git, rather than anything here,
// is what applies them. What this package reads them for itself is gate 2,
// which has to tell "a directory holding a vault" from "a directory holding the
// cruft a volume accumulates", in a directory that holds no repo to ask.
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
	return coveredBy(IgnoreFloor, relative, isDir)
}

// coveredBy reports whether a path relative to the vault root is covered by any
// of a closed list of gitignore patterns, under the rules matchesIgnoreFloor
// documents. The ignore floor, the churn subset and the refused-path list are
// all such lists, and reading all three the same way is what keeps obsync's
// answer and git's answer one answer.
func coveredBy(patterns []string, relative string, isDir bool) bool {
	relative = filepath.ToSlash(relative)
	for _, entry := range patterns {
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
		// ...and everything under it, which is what the churn subset's own
		// caller asks: git names the tracked file `.trash/gone.md`, and it is
		// the directory above it that the floor covers. Gate 2's walk never
		// reaches here, because it skips a directory that matched.
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

// PluginData is the one ignore-floor entry that is also applied as a pathspec
// exclusion on the `git add` itself, which no .gitignore can negate (§5).
//
// It is the design's one place where obsync cannot be overruled, and the reason
// is asymmetric: overriding a noisy default is a preference, and committing a
// credential is unrecoverable. `.obsidian/plugins/*/data.json` is where
// community plugins keep their API keys, and this audience points obsync at
// repos that may be public.
//
// It is also the one floor entry the churn subset leaves out — refuse to add,
// never remove (§5).
const PluginData = ".obsidian/plugins/*/data.json"

// ChurnSubset is the part of the ignore floor obsync untracks once, in a vault
// whose history already carries it: ignore rules only affect untracked paths,
// so a vault that has ever committed its workspace file churns forever
// regardless of what the floor says (§5).
//
// It is the whole floor except PluginData. Untracking an already-tracked
// data.json would delete deliberately-synced plugin settings from every other
// clone, and would not unleak a key the remote's history already holds; the
// rule is never to be the thing that commits a key for the first time.
var ChurnSubset = churnSubset()

func churnSubset() []string {
	subset := make([]string, 0, len(IgnoreFloor)-1)
	for _, entry := range IgnoreFloor {
		if entry != PluginData {
			subset = append(subset, entry)
		}
	}
	return subset
}

// RefusedPaths is the closed list of filenames obsync will not put in a commit,
// whatever their state (§5). Its contents are part of the declared surface,
// because changing them silently changes what a user's repo holds (§10,
// docs/interface.md).
//
// The entries are gitignore patterns that name no directory, so each matches
// that name at any depth in the vault, exactly as the floor's own bare-name
// entries do.
//
// Name-matching only — no content scanning, ever. Sniffing for a private-key
// header would catch a key saved as notes.md, but this is a note-taking vault:
// people write *about* keys, paste example blocks, and keep security runbooks.
// A refusal is silent by omission, so a false positive is the expensive error,
// and nobody names a note id_rsa. The list stays short precisely so that no
// escape hatch is needed — someone who genuinely wants a .pem in their vault
// renames the file.
var RefusedPaths = []string{
	".env", ".env.*",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	"*.pem", "*.key", "*.p12", "*.pfx",
	".netrc", ".npmrc", ".pypirc",
	"credentials",
}

// Refusal is one path obsync will not commit, and the conclusive fact that
// makes it so — which is what the WARN says and what the attention note will
// carry (§9, #38).
type Refusal struct {
	Path   string
	Reason string
}

// Committable is what one sync run may put in a commit, and what it took out on
// the way there (§5's committable set).
type Committable struct {
	// Paths is the committable set itself. "Commit if dirty" is "commit if
	// this is non-empty": a tree holding nothing but refused and unsettled
	// paths is quiet, and produces no commit, no push and no repeated warning.
	Paths []string

	// Refused and Unsettled are the two subtractions obsync made, kept so the
	// loop can say what it is not committing and why. They are different in
	// kind: a refusal is a standing decision about a path, and an unsettled
	// path is a fact about the last second.
	Refused   []Refusal
	Unsettled []string

	root    string
	sampled map[string]sample
}

// CommittableSet is the paths a sync run would actually stage (§5).
//
// One of the three subtractions the glossary names has already happened by the
// time changed arrives here, and it is not obsync's arithmetic: the ignore
// floor is git's, applied through the exclude file obsync wrote at startup.
// That is load-bearing rather than incidental — the floor is a default rather
// than a rule, and it is git reading .gitignore first that lets a vault's own
// `!.obsidian/workspace.json` overrule it. A second subtraction here, in
// obsync's own code, would be a floor that cannot be overruled, which is the
// floor's whole purpose inverted.
//
// The other two happen here, in this order, and the order is the cheap question
// first: a path obsync will not commit whatever its state is not worth spending
// a settle interval on.
//
//   - The refusal layer, which is the one exclusion obsync applies itself: a
//     credential reaching a repo that may be public is the one unrecoverable
//     mistake in this design, and a mechanism a human can switch off is not one
//     that prevents it. A refusal skips the path and never freezes the loop —
//     if a 200MB video lands in the vault and obsync stops, every note the user
//     writes stops reaching the remote because of an attachment, and a stopped
//     sidecar is the failure that gets a sync tool uninstalled. The stated
//     consequence is that while a tracked path is refused, the remote holds the
//     last version that passed and the working tree holds a newer one: stale,
//     consistent, and visible.
//   - The settle guard (§6), which leaves out whatever is still being written.
//     An unsettled path is excluded from *this* commit and the run carries on;
//     it is never an aborted run. Aborting is disqualified outright: during
//     continuous typing the quiet window never clears *and* the hot file is
//     never settled, so every capped run would abort and nothing would commit
//     for the whole session. A commit missing a file is a valid state, and a
//     commit containing torn bytes is an invalid one.
func CommittableSet(clk clock.Clock, root string, changed []string, sizeCeiling int64) Committable {
	set := Committable{root: root}

	candidates := make([]string, 0, len(changed))
	for _, relative := range changed {
		if reason := refuses(root, relative, sizeCeiling); reason != "" {
			set.Refused = append(set.Refused, Refusal{Path: relative, Reason: reason})
			continue
		}
		candidates = append(candidates, relative)
	}

	before, after := acrossTheSettleInterval(clk, root, candidates)
	set.Paths = make([]string, 0, len(candidates))
	for _, relative := range candidates {
		if !before[relative].same(after[relative]) {
			set.Unsettled = append(set.Unsettled, relative)
			continue
		}
		set.Paths = append(set.Paths, relative)
	}
	set.sampled = after
	return set
}

// OnDisk reports whether the settle guard's second sample — the one taken
// immediately before the `git add` — found this path there.
//
// It is the guard's own answer to a question the caller cannot ask git: a path
// git reported as changed and has no index entry for matches no pathspec once
// it is gone, and naming it is fatal to the whole add (git.ChangedPath). The
// fact is already paid for, so asking costs no stat.
func (c Committable) OnDisk(relative string) bool {
	return c.sampled[relative].present
}

// StageVerify is §6's stage-verify: the first of the paths obsync just staged
// that moved on disk while it was doing so, or "" when none did.
//
// It re-stats against the sample the settle guard finished on, which is the one
// taken immediately before the `git add`. Its constituency is the third writer,
// whose writes no sampling window can anticipate — a path it touches can be
// genuinely cold when sampled and hot during the add.
//
// The staged paths rather than the whole committable set, which is §6's own
// scope and is narrower twice over: a committable path whose change the index
// already holds in full is not read from disk by this run at all, so movement
// under it says nothing about the bytes the commit will carry, and aborting on
// it would be an abort the run did not earn.
//
// Anything that moved aborts the run, and aborting is safe here in a way it is
// not on the read side, because these paths were just verified stable across the
// settle interval. With write-verify (§7), obsync verifies both ends of every
// tree it touches.
func (c Committable) StageVerify(staged []string) string {
	for _, relative := range staged {
		if !c.sampled[relative].same(sampleOf(c.root, relative)) {
			return relative
		}
	}
	return ""
}

// Refusals is which of these paths obsync will not commit, and why (§5). It is
// the refusal layer on its own, for the caller that has to ask the question of
// an index somebody else wrote rather than of a working tree obsync sampled.
func Refusals(root string, paths []string, sizeCeiling int64) []Refusal {
	var refused []Refusal
	for _, relative := range paths {
		if reason := refuses(root, relative, sizeCeiling); reason != "" {
			refused = append(refused, Refusal{Path: relative, Reason: reason})
		}
	}
	return refused
}

// refuses answers why obsync will not commit a path, or "" when it will.
//
// "Whatever its state" is meant literally, and the case worth naming is a
// refused path that is being *deleted*: obsync does not commit that either. It
// is the same instinct as leaving an already-tracked data.json alone — obsync
// never puts a credential in a repo and never takes a human's decision about
// one out of their hands — and it is honest about what it would buy, since a
// key the remote's history already holds is not unleaked by a commit removing
// it. Removing it is one `git rm` and one push, by the human, deliberately.
func refuses(root, relative string, sizeCeiling int64) string {
	if coveredBy(RefusedPaths, relative, false) {
		return "its name is on obsync's refused-path list, which obsync matches by name and never " +
			"by content"
	}

	// The size is read from the vault rather than from git, because the bytes
	// that would reach the remote are the ones on disk. A path obsync cannot
	// stat is one git has just reported and something else has since moved —
	// the third writer, or an ordinary deletion — and there is no fact here to
	// refuse on. Refusing on the absence of a fact is what would turn a
	// vanished file into a path that silently stops syncing.
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= sizeCeiling {
		return ""
	}
	return "it is " + config.FormatSize(info.Size()) + " and obsync's size ceiling is " +
		config.FormatSize(sizeCeiling)
}

// InChurnSubset reports whether a tracked path is one of the ones obsync
// untracks in its one-shot: the ignore floor minus PluginData, read the way git
// reads it.
func InChurnSubset(relative string) bool {
	return coveredBy(ChurnSubset, relative, false)
}
