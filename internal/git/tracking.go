package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andyroberts2/obsync/internal/vault"
)

// excludeFileMarker is the comment the ignore floor is written under, and the
// first bytes of the file obsync owns. It exists so that a human opening the
// file finds out whose it is and where their own rules go, in the place they
// are already looking.
const excludeFileMarker = "# obsync's ignore floor. This file belongs to obsync and is rewritten\n" +
	"# wholesale every time obsync starts. Put your own rules in the vault's\n" +
	"# .gitignore, which git reads first and which obsync never writes — an\n" +
	"# entry there outranks every line below.\n"

// writeIgnoreFloor writes the ignore floor to the repo's exclude file (§5).
//
// The exclude file rather than the vault's .gitignore, and the difference is
// the whole design of this: nothing here is ever committed, so it cannot
// conflict, cannot be clobbered by an external push, and cannot be disabled by
// editing a note. The .gitignore is content, it is the user's, and obsync never
// writes it.
//
// Wholesale rather than merged, because the exclude file is an owned path and
// an owned path is one obsync rewrites rather than edits (§10) — a merge would
// need a marker to delimit obsync's region, a rule for what happens when a
// human moves it, and an answer for a half-written file, all to preserve
// entries whose supported home is the .gitignore that outranks this one anyway.
//
// It is written at every startup and it is idempotent: the bytes are a function
// of a closed list, so a second startup writes the same file, and the floor
// moving is the only thing that moves it.
func (r *Repo) writeIgnoreFloor() error {
	floor := excludeFileMarker + strings.Join(vault.IgnoreFloor, "\n") + "\n"
	if err := r.writeOwnedFile(r.excludeFile, []byte(floor)); err != nil {
		return fmt.Errorf("obsync could not write its ignore floor to %q: %w", r.excludeFile, err)
	}
	return nil
}

// resolveOwnedPaths asks git where the two owned paths inside .git are, once,
// and is the only thing here that ever asks.
//
// git is asked rather than joining ".git" onto the vault path, because a .git
// that is a *file* is a worktree or a submodule and bootstrap attaches to those
// deliberately — the repo is wherever the vault's own .git says it is.
// --git-path also knows which of these live in the common directory of a
// worktree set, which info/exclude does and obsync's own directory does not.
//
// Once, at bootstrap, because neither answer can move under a Repo: the vault
// path is fixed for the process lifetime, and asking per write would put a git
// in front of every status file (#37) and every attention note (#38).
func (r *Repo) resolveOwnedPaths() error {
	exclude, err := r.gitPath("info/exclude")
	if err != nil {
		return fmt.Errorf("obsync could not find the repository's exclude file: %w", err)
	}
	staging, err := r.gitPath("obsync/tmp")
	if err != nil {
		return fmt.Errorf("obsync could not find where to stage its own writes: %w", err)
	}
	r.excludeFile, r.staging = exclude, staging
	return nil
}

// TrackedPluginData is every plugin data file the vault's history already
// carries.
//
// obsync leaves them alone and says so, loudly, once (§5, §9). Untracking one
// would delete deliberately-synced plugin settings from every other clone, and
// it would not unleak a key the remote's history already holds — so the rule is
// never to be the thing that commits a key for the first time: refuse to add,
// never remove.
//
// The pathspec is git's rather than obsync's reading of the same shape, because
// git is what applies it on the add that refuses one.
func (r *Repo) TrackedPluginData() ([]string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"ls-files", "-z", "--cached", "--", ":(glob)" + vault.PluginData},
	})
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// TrackedChurnSubset is every tracked path in the churn subset: the workspace
// state and OS cruft a vault's history may already carry, which ignore rules
// can do nothing about because they only ever affect untracked paths (§5).
//
// Two questions are asked and each is answered by whoever owns it. git answers
// "is this ignored", over the exclude file obsync wrote plus the vault's own
// .gitignore, in that precedence — so a human who put `!.obsidian/workspace.json`
// in their .gitignore because they want their workspace synced is not
// overruled by the one-shot either. obsync answers "is this mine to untrack",
// which bounds the act to the churn subset: git's list also holds whatever the
// human ignored for reasons of their own, and untracking those would be obsync
// making a structural commit nobody asked for.
func (r *Repo) TrackedChurnSubset() ([]string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"ls-files", "-z", "--cached", "--ignored", "--exclude-standard"},
	})
	if err != nil {
		return nil, err
	}

	var subset []string
	for _, path := range splitNUL(out) {
		if vault.InChurnSubset(path) {
			subset = append(subset, path)
		}
	}
	return subset, nil
}

// Untrack takes paths out of the index and leaves every byte of them on disk.
//
// This is obsync's only `git rm`, it is always `--cached`, and the two facts
// are one promise: obsync never deletes a file from the vault of its own
// accord. What this stages is the index no longer carrying paths whose contents
// are still exactly where the human left them.
//
// The pathspecs arrive as NUL-separated literals on stdin for the reason Stage
// gives: a note title may hold a space, a newline or a glob character, and any
// of the three would otherwise be read as something other than the name of the
// file it is.
func (r *Repo) Untrack(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := r.run(invocation{
		dir:   r.vault,
		stdin: literalPathspecs(paths),
		args:  []string{"rm", "--cached", "--quiet", "--pathspec-from-file=-", "--pathspec-file-nul"},
	})
	return err
}

// Unstage puts the index back to what HEAD holds for the given paths, and
// writes nothing to disk.
//
// It is how a refused path stays out of a commit obsync did not build the whole
// index of. `git add` is not the only way a path reaches the index — a human's
// own `git add -A` in their vault is muscle memory, and so is every plugin that
// drives git for them — and `git commit` records the index rather than what
// obsync staged, so without this a refused path somebody else staged rides out
// in obsync's commit and its push. §5 admits no exception: the list never
// enters a commit, whatever the vault's state, and the escape hatch is renaming
// the file rather than staging it.
//
// A pathspec-limited reset, which cannot move HEAD and never writes the working
// tree — measured at both matrix points, exit 0, the file's bytes exactly where
// the human left them. What the index carries goes back to what the last commit
// had, which is the stated consequence of a refusal: the remote holds the last
// version that passed and the vault holds a newer one. There is no --hard here
// and there is none anywhere.
func (r *Repo) Unstage(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := r.run(invocation{
		dir:   r.vault,
		stdin: literalPathspecs(paths),
		args:  []string{"reset", "--quiet", "--pathspec-from-file=-", "--pathspec-file-nul"},
	})
	return err
}

// gitPath is one such answer. One trailing newline is taken off it and nothing
// else is: this is a single value rather than a listing, and a path is never
// split on anything.
func (r *Repo) gitPath(name string) (string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--path-format=absolute", "--git-path", name},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// ownedFileMode is what obsync leaves an owned path readable as: 0644, which is
// what git itself creates info/exclude as and what an editor creates a note as.
// A file that arrived at 0600 because it came through a staging directory would
// make obsync's ignore floor apply to obsync's UID and to nobody else running
// git in the same vault, and would leave the attention note (#38) unreadable to
// a human who is not obsync. Nothing written through here is a secret — obsync
// never reads the credential at all, the helper does (§8).
const ownedFileMode = 0o644

// writeOwnedFile writes one of obsync's owned paths, write-then-rename through
// .git/obsync/tmp/ (§6).
//
// Staging inside .git wins twice: the vault's own file watcher already
// hardcodes ignoring .git, so no phantom temp file flashes in the user's file
// tree, and .git is outside the working tree, so git status can never see it
// and no floor entry is needed. The rename is same-filesystem by construction,
// which is what makes it atomic — a reader sees the previous bytes or the new
// ones and never a half-written file.
//
// It is the rule for every file obsync writes, and the status file (#37) and
// the attention note (#38) are written through it too.
//
// The mode is set at ownedFileMode rather than left to os.CreateTemp's 0600,
// which is the one thing a temporary file's default gets wrong for a
// destination that is not one.
func (r *Repo) writeOwnedFile(path string, content []byte) error {
	if err := os.MkdirAll(r.staging, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(r.staging, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	// A crash between here and the rename leaves debris inside .git, which is
	// swept at startup (#32); it is never a half-written file at the
	// destination.
	defer func() { _ = os.Remove(temporary.Name()) }()

	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(ownedFileMode); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
