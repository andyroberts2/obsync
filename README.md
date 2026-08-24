# obsync

Bidirectional git sync for a self-hosted Obsidian vault, as a Docker sidecar.

obsync watches a mounted Obsidian vault, commits and pushes local edits on a
sensible cadence, and pulls upstream changes back down — keeping the vault and
a remote GitHub repo continuously in step.

It exists because the tools either side of this gap only do half the job:
[`kubernetes/git-sync`](https://github.com/kubernetes/git-sync) pulls but never
pushes, and [`simonthum/git-sync`](https://github.com/simonthum/git-sync) is a
one-shot script with no daemon, debounce, or credential handling.

## Status

**Design phase.** Nothing is implemented yet. The design is being worked as a
[wayfinder map](../../issues?q=label%3Awayfinder%3Amap) — a tracked set of
decision tickets, resolved one at a time until the spec is ready to build from.

## Reference deployment

The primary target is [ignis](https://github.com/Nystik-gh/ignis), which runs
Obsidian in a browser with the vault on the server. obsync runs as a sidecar
sharing the same vault volume. This is the deployment ignis itself currently
recommends for git sync ([ignis#14](https://github.com/Nystik-gh/ignis/issues/14)).

obsync is not coupled to ignis, though — it assumes an Obsidian vault on a
mounted volume, and ignis is the documented reference stack.

## Research

Prior art and constraints, gathered before any design decisions were made:

- [`docs/research/kubernetes-git-sync.md`](docs/research/kubernetes-git-sync.md)
  — architecture, the worktree/symlink model, auth, and why it can't be forked
  or reused for a live vault.
- [`docs/research/simonthum-git-sync.md`](docs/research/simonthum-git-sync.md)
  — the sync algorithm, and its safety gates, which are the valuable part.
- [`docs/research/ignis-and-obsidian-vaults.md`](docs/research/ignis-and-obsidian-vaults.md)
  — how ignis touches the filesystem, and Obsidian vault git conventions.

## Licence

Not yet chosen.
