package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
	"github.com/andyroberts2/obsync/internal/vault"
)

// Bootstrap opens the vault's repository, and is the one decision obsync makes
// about a directory before it syncs it (§3, gate 2):
//
//   - the directory holds a repo — attach to it, on the branch it is already
//     on;
//   - the directory shows nothing but the ignore floor — clone the remote into
//     it, on the branch the remote calls default;
//   - anything else — refuse, and never adopt a folder obsync cannot reason
//     about.
//
// Each case is answered by the thing that has an opinion, which is the whole
// reason there are three: always taking the remote's default would silently
// start syncing main on a vault sitting on vault-live.
//
// A refusal comes back as an error and never as an exit. The loop reports it
// and keeps turning, so repairing the cause releases obsync with no restart
// (§7) — which is why every refusal below names a fact rather than a guess.
func Bootstrap(ctx context.Context, cfg config.Config, log *slog.Logger, clk clock.Clock) (*Repo, error) {
	info, err := os.Stat(cfg.VaultPath)
	if err != nil {
		return nil, fmt.Errorf("the vault at %q cannot be read: %w", cfg.VaultPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("the vault at %q is not a directory", cfg.VaultPath)
	}

	credentialIsolation, err := newIsolation()
	if err != nil {
		return nil, err
	}

	repo := &Repo{
		vault:                 cfg.VaultPath,
		isolation:             credentialIsolation,
		credentialEnvironment: cfg.CredentialEnvironment(),
		log:                   log,
		clock:                 clk,
	}
	if err := repo.writeConfig(cfg.CommitIdentity); err != nil {
		_ = repo.Close()
		return nil, err
	}

	branch, err := repo.resolveTrackedBranch(ctx, cfg)
	if err != nil {
		_ = repo.Close()
		return nil, err
	}
	repo.branch = branch
	return repo, nil
}

// resolveTrackedBranch performs the bootstrap and answers with the branch
// obsync syncs, which is then fixed for the process lifetime (§3).
func (r *Repo) resolveTrackedBranch(ctx context.Context, cfg config.Config) (string, error) {
	// A .git that is a file rather than a directory is a worktree or a
	// submodule, and it is still a repo obsync attaches to: what makes this the
	// attach case is that git has an answer here, not what shape the answer
	// takes on disk.
	_, err := os.Lstat(filepath.Join(r.vault, ".git"))
	switch {
	case err == nil:
		return r.attach(cfg.Branch)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("the vault at %q cannot be read: %w", r.vault, err)
	}

	shows, path, err := vault.ShowsMoreThanIgnoreFloor(r.vault)
	if err != nil {
		return "", fmt.Errorf("the vault at %q cannot be read: %w", r.vault, err)
	}
	if shows {
		return "", fmt.Errorf("the vault at %q is not a git repository and holds %q, so obsync will "+
			"not adopt it: point obsync at an empty directory to clone into, or make this one a repo "+
			"with an origin", r.vault, path)
	}

	// Two rules meet here and they do not quite agree, so obsync says which one
	// it is refusing under. Gate 2 tolerates ignore-floor cruft — a .DS_Store
	// left by a volume that was once mounted on a Mac is not a folder obsync
	// cannot reason about — and git will not clone into any destination that is
	// not empty, cruft and empty folders included (measured on both matrix
	// points: exit 128, "already exists and is not an empty directory").
	//
	// So a directory holding only cruft gets a refusal naming the one entry in
	// the way rather than the refusal above, which tells an operator to move one
	// file rather than to reconsider their deployment. Cloning anyway and
	// relaying git's own words would say the same thing less usefully, and
	// emptying the directory for them is obsync writing where a human's files
	// are.
	if entry, notEmpty, err := firstEntry(r.vault); err != nil {
		return "", fmt.Errorf("the vault at %q cannot be read: %w", r.vault, err)
	} else if notEmpty {
		return "", fmt.Errorf("obsync clones into an empty directory and the vault at %q holds %q, "+
			"which git will not clone into even though obsync's ignore floor covers it: move or "+
			"delete it and obsync clones on its next run", r.vault, entry)
	}
	return r.clone(ctx, cfg)
}

// attach is the bootstrap case where the vault is already a repo: the tracked
// branch is the branch the vault is already on, because that is the thing with
// an opinion here (§3).
//
// The operator's override may only agree with it. obsync never runs git
// checkout after bootstrap — checking a branch out under a live Obsidian
// rewrites files a human has open — so an override naming a branch the vault is
// not on is a refusal rather than a switch, and the remedy is the human's own
// checkout.
func (r *Repo) attach(override string) (string, error) {
	head, err := r.headBranch()
	if err != nil {
		return "", err
	}

	branch := head
	if override != "" {
		if override != head {
			return "", fmt.Errorf("the vault's HEAD is on %q and obsync was told to track %q; obsync "+
				"never checks a branch out, so check %q out in the vault or point obsync at %q",
				head, override, override, head)
		}
		branch = override
	}

	// The tracked branch has to name a commit, and an unborn one is refused
	// rather than repaired: a .git left half-written by a killed clone has
	// config and HEAD but no commit, and obsync cannot tell that from a human
	// having broken HEAD in a repo that holds history (§7). The safe reading of
	// the pair is the one that touches nothing.
	if _, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", "refs/heads/" + branch},
	}); err != nil {
		return "", fmt.Errorf("the vault's %q holds no commits, so obsync has no tracked branch to "+
			"sync: that is what a clone killed halfway leaves behind, and obsync refuses such a repo "+
			"rather than repairing it", branch)
	}
	return branch, nil
}

// clone is the bootstrap case where the vault is an empty directory: obsync
// makes it a copy of the remote, on the branch the remote calls default (§3).
//
// It is one repo, one remote named origin and one tracked branch, and the
// refspec is one branch in each direction: --single-branch writes a fetch
// refspec naming exactly this branch and --no-tags keeps every tag out, so the
// vault obsync built holds nothing it did not ask for. Measured on both matrix
// points — a plain clone writes +refs/heads/*:refs/remotes/origin/* and brings
// every tag with it.
//
// This is obsync's only clone, and it happens once, into a directory that holds
// no repo. obsync never re-clones and never repairs a repo by replacing it
// (§7): a re-clone discards exactly the commits obsync exists to have made.
func (r *Repo) clone(ctx context.Context, cfg config.Config) (string, error) {
	if cfg.Branch == "" {
		// With no override the tracked branch comes from the remote's HEAD, so
		// a remote that has none to give is refused before anything is
		// written. That order is load-bearing, and it is not only the empty
		// remote it saves: a remote whose HEAD names a branch it does not hold
		// clones with exit 0 into a repo with no refs and an unborn HEAD
		// (measured on both matrix points) — the state obsync refuses on every
		// later run, in a directory that was empty a moment ago. So obsync
		// leaves the directory alone and says why, and the operator pushing a
		// vault to that remote releases it with no restart.
		//
		// The question is asked as a status rather than as a listing: git's
		// --exit-code gives 2 for "nothing matched", which is a conclusive
		// answer from a remote obsync reached, and 128 for a remote it did not
		// reach — so nothing here reads a ref name, and an unreachable remote
		// stays an ordinary network failure rather than becoming a verdict.
		if err := r.remoteHasAHead(ctx, cfg.RepoURL); err != nil {
			return "", err
		}
	}

	args := []string{"clone", "--quiet", "--single-branch", "--no-tags"}
	if cfg.Branch != "" {
		// An override names the branch to clone, and a remote that does not
		// hold it fails the clone rather than guessing — git cleans up after
		// itself there, leaving the directory as it found it (measured).
		args = append(args, "--branch", cfg.Branch)
	}
	if _, err := r.run(invocation{
		dir:      filepath.Dir(r.vault),
		args:     append(args, cfg.RepoURL, r.vault),
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	}); err != nil {
		return "", fmt.Errorf("obsync could not clone the remote into the vault at %q: %w", r.vault, err)
	}

	// The branch git resolved, read back the way the attach case reads it: one
	// value, from the repo obsync now has, rather than a second answer parsed
	// out of what the remote said.
	return r.headBranch()
}

// remoteHasAHead reports whether the remote has a HEAD to resolve a default
// branch from — the one thing obsync needs from a remote before it will clone
// it into an empty directory.
func (r *Repo) remoteHasAHead(ctx context.Context, repoURL string) error {
	_, err := r.run(invocation{
		dir:      filepath.Dir(r.vault),
		args:     []string{"ls-remote", "--exit-code", "--quiet", repoURL, "HEAD"},
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	})
	var command *CommandError
	if errors.As(err, &command) && command.ExitCode == noMatchingRefs {
		return fmt.Errorf("the remote has no branch for obsync to clone into the vault at %q: it "+
			"holds no refs at all, or its HEAD names a branch it does not hold. Push a vault to it, "+
			"or set OBSYNC_BRANCH to name the branch obsync should clone", r.vault)
	}
	return err
}

// noMatchingRefs is the status git's --exit-code gives when a listing found
// nothing, as against 128, which is git's everything-code and here means the
// remote was not reached at all. It is documented in git-ls-remote(1), which
// is what makes it a fact obsync may branch on rather than prose.
const noMatchingRefs = 2

// firstEntry names one thing the directory holds, and reports whether it holds
// anything at all. One name is read rather than the whole listing: the answer
// is conclusive after the first, and the directory may be a vault of a hundred
// thousand notes.
func firstEntry(dir string) (string, bool, error) {
	open, err := os.Open(dir)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = open.Close() }()

	names, err := open.Readdirnames(1)
	if errors.Is(err, io.EOF) || len(names) == 0 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return names[0], true, nil
}

// FirstPushStanding is what the remote holds when obsync has no upstream
// counterpart for the tracked branch — the state §3's sharpest rule keys on,
// and the only question a first push has to ask before it may create a branch.
type FirstPushStanding int

const (
	// RemoteHoldsTrackedBranch: there is an upstream counterpart after all, and
	// obsync simply had no remote-tracking ref for it. Pushing to it creates
	// nothing.
	RemoteHoldsTrackedBranch FirstPushStanding = iota
	// RemoteHoldsNoRefs: a brand-new empty remote, which is the one case where
	// a push may create the tracked branch (§3).
	RemoteHoldsNoRefs
	// RemoteHoldsOtherRefs: refs, but not ours. A full freeze — the branch name
	// came from local HEAD, so a stray branch or a typo'd override would
	// otherwise create a remote branch and sync an entire vault into it, and
	// the push would succeed (§3).
	RemoteHoldsOtherRefs
)

// StandingOfTrackedBranch asks the remote which of the three it is.
//
// Both questions are answered by a status rather than by a listing: git's
// --exit-code gives 2 for "no refs matched", which is a conclusive answer from
// a remote obsync reached, and 128 for a remote it did not reach — so nothing
// here reads a ref name, and an unreachable remote stays an ordinary network
// failure rather than being mistaken for a verdict.
func (r *Repo) StandingOfTrackedBranch(ctx context.Context) (FirstPushStanding, error) {
	holds, err := r.remoteMatches(ctx, "--heads", "refs/heads/"+r.branch)
	if err != nil {
		return 0, err
	}
	if holds {
		return RemoteHoldsTrackedBranch, nil
	}

	anyRefs, err := r.remoteMatches(ctx)
	if err != nil {
		return 0, err
	}
	if anyRefs {
		return RemoteHoldsOtherRefs, nil
	}
	return RemoteHoldsNoRefs, nil
}

// remoteMatches reports whether the remote holds any ref matching the patterns,
// and reads nothing it printed.
func (r *Repo) remoteMatches(ctx context.Context, patterns ...string) (bool, error) {
	args := append([]string{"ls-remote", "--exit-code", "--quiet", config.RemoteName}, patterns...)
	if _, err := r.run(invocation{
		dir:      r.vault,
		args:     args,
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	}); err != nil {
		var command *CommandError
		if errors.As(err, &command) && command.ExitCode == noMatchingRefs {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
