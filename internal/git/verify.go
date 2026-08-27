package git

import (
	"fmt"
	"strconv"
	"strings"
)

// Write-verify, and the failed-apply anchor (§7).
//
// After obsync applies a tree it computed, it checks that the vault holds that
// tree. It is the only thing between a botched apply and a *pushed* botched
// apply, and the only interlock whose failure means obsync can no longer trust
// its own view of the vault — every other one is a fact about the vault or the
// remote, and this one is a fact about obsync.
//
// It is the write side's half of a pair. Stage-verify (§6, internal/vault)
// re-stats what a run staged, so the tree obsync *reads out* of the vault is
// the one it sampled; write-verify compares what obsync *wrote into* the vault
// against the tree it computed. Together they mean obsync verifies both ends of
// every tree it touches.
//
// Two conclusive facts, and neither is a judgement:
//
//  1. The vault's HEAD is the commit obsync applied. An apply that reported
//     success and left HEAD somewhere else is one obsync has no account of.
//  2. Nothing the apply touched differs between the vault and that commit.
//     HEAD alone would not see §6's own worst case — an apply that wrote some
//     of its paths and not others leaves HEAD correct and the vault holding a
//     tree obsync never computed, which is exactly why the write side is
//     all-or-nothing rather than per-path.
//
// The scope is the paths the apply touched and never the whole tree, for the
// same reason the guard immediately before it has that scope: the vault is a
// live directory, a human's own edit at a path the apply never went near is
// ordinary and is what the next run commits, and checking everything would make
// write-verify fire on the vault working normally.
//
// What is left inside that scope is a window of a few git spawns between the
// apply and this check, in which something else may overwrite a path obsync
// has just written. That is the third writer, and it is deliberately read as a
// write-verify failure rather than excused: obsync cannot tell a third writer's
// bytes from its own apply having gone wrong, and the whole of what this
// interlock is for is refusing to publish a tree it cannot account for. The
// window is bounded by the guard that runs immediately before the apply, which
// has just watched every one of these paths hold still across the settle
// interval (§6).

// writeVerify is §7's write-verify, and it answers with the full freeze §7 asks
// for — after anchoring the commit obsync computed, and never before.
//
// applied is the commit obsync just put in the vault: the remote's tip on a
// fast-forward, and the merge commit obsync built on a divergence. touched is
// the paths that apply was going to change, which is the same list the guard
// before it refused over.
func (r *Repo) writeVerify(applied string, touched []string) error {
	head, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-parse", "--verify", "--quiet", "HEAD"},
	})
	if err != nil {
		return err
	}
	if at := strings.TrimSpace(string(head)); at != applied {
		return r.anchorTheFailedApply(applied, "obsync applied "+applied+" to the vault and the "+
			"vault's HEAD is "+at)
	}

	unapplied, err := r.differingFromTheVault(applied, touched)
	if err != nil {
		return err
	}
	if unapplied != "" {
		return r.anchorTheFailedApply(applied, "obsync applied "+applied+" to the vault and the "+
			"vault holds something else at "+strconv.Quote(unapplied))
	}
	return nil
}

// differingFromTheVault is the first of the paths an apply touched that the
// vault does not hold as the applied commit has it, in git's own order — which
// is what makes the path a freeze names the same one on every run rather than
// whichever a map handed back first.
//
// `diff-index` without --cached, so the question is about the working tree and
// not only about the index: what write-verify is asked about is the bytes in
// the vault. -z rather than a line per path, because a note title may legally
// contain a newline and git C-quotes one onto a single line without it
// (measured at both matrix points).
//
// The listing is asked of the whole repository and intersected here rather than
// asked with the touched paths as pathspecs. Two reasons, and the first is the
// binding one: a divergence that merges ten thousand paths cleanly is an
// ordinary bulk import from the other side, and that many pathspecs is an argv
// git may refuse to be handed at all. The second is that git already walks the
// index either way, so the pathspec would buy nothing but the risk.
//
// It reports nothing for a path git considers racily clean without reading it,
// which matters here more than anywhere else in obsync: the apply has just
// rewritten these files, so their mtimes sit in the same second as the index
// git wrote alongside them. Measured at both matrix points — a clean `reset
// --keep` and a clean `merge --ff-only` each leave this listing empty — because
// a false positive here is a freeze only a human can clear.
func (r *Repo) differingFromTheVault(applied string, touched []string) (string, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"diff-index", "-z", "--name-only", applied},
	})
	if err != nil {
		return "", err
	}
	differing := make(map[string]bool)
	for _, path := range splitNUL(out) {
		differing[path] = true
	}
	for _, path := range touched {
		if differing[path] {
			return path, nil
		}
	}
	return "", nil
}

// anchorTheFailedApply writes the commit obsync computed to the failed-apply
// anchor and answers with the freeze — in that order, because the freeze is
// what stops obsync, and a freeze declared before the anchor exists is a window
// in which the artifact that explains it does not.
//
// The commit is a real object and, on the merge path, an unreachable one, so
// nothing but this ref keeps a later gc from pruning the one thing that could
// explain and undo the mess. It sits outside refs/heads/, so obsync's
// one-branch-each-direction refspec can never carry it to the remote (§3).
//
// obsync attempts no corrective action of any kind here, and that is the whole
// posture rather than an omission: a tool that has just proved it cannot apply
// a tree correctly is the last thing that should try again unsupervised. It
// does not re-apply, does not reset the vault back, does not delete the ref,
// and does not push. The next run stops at gate 9 and every run after it does
// the same, until a human deletes the ref.
//
// The freeze is gate 9's own, by name, and that is deliberate: the ref this
// writes is the fact gate 9 reads, so the run that writes it and every run that
// re-reads it are one state an operator is told about once (§9).
func (r *Repo) anchorTheFailedApply(computed, fact string) error {
	if _, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"update-ref", FailedApplyAnchor, computed},
	}); err != nil {
		// A repository that cannot be written a ref is not one obsync can latch
		// a freeze in, and saying it had would be worse than saying nothing:
		// the latch is a ref precisely so that it survives what memory does
		// not. So this travels as an ordinary failed run, which is what the
		// local failure streak counts and what five of in a row is the full
		// freeze §7 already names (#34, unbuilt).
		return fmt.Errorf("write-verify failed and obsync could not anchor %s at %s: %w",
			computed, FailedApplyAnchor, err)
	}
	return &InterlockFailure{
		Interlock: freezeFailedApplyAnchor,
		Fact:      fact + ", so obsync can no longer trust its own view of the vault",
		Remedy:    failedApplyRemedy,
	}
}
