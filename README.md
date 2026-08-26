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

**Early implementation.** The design is settled and written up as one spec,
[#21](../../issues/21), which was worked as a
[wayfinder map](../../issues?q=label%3Awayfinder%3Amap) — a tracked set of
decision tickets, resolved one at a time. What exists today is the project
skeleton, the config surface — nine environment variables, one required, echoed
in a startup line — and the walking skeleton of the sync loop: a wake-up makes
obsync ask git what changed in the vault, commit it as one commit whose message
says what changed, and push it to the tracked branch. That loop now keeps its
own time: it ticks every 60s so a change nothing reported still arrives, waits
out an unreachable remote from 60s to 15 minutes while carrying on committing
locally, and finishes the run in flight before it exits. The rest of the
cadence — commit ten seconds after the vault goes quiet, commit anyway every
five minutes while someone is still typing — is written and tested and waiting
on the watcher to tell it the vault is being written to; until then obsync runs
in tick-only mode, which is a mode it has anyway. The **declared surface** —
everything a version number will make a promise about — is written down ahead
of the code that implements it.

Not yet: the filesystem watcher, the safety interlocks, the settle guard,
conflicts, the attention note, the status file and the container image. obsync
is not something to point at a vault yet.

## Reference deployment

The primary target is [ignis](https://github.com/Nystik-gh/ignis), which runs
Obsidian in a browser with the vault on the server. obsync runs as a sidecar
sharing the same vault volume. This is the deployment ignis itself currently
recommends for git sync ([ignis#14](https://github.com/Nystik-gh/ignis/issues/14)).

obsync is not coupled to ignis, though — it assumes an Obsidian vault on a
mounted volume, and ignis is the documented reference stack.

## Documentation

- [`docs/interface.md`](docs/interface.md) — the declared surface: the nine
  environment variables, the four subcommands, the health contract, and what
  obsync writes into your vault. It is what SemVer is measured over, and what
  every release's "Surface changes" note is about.

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

[Apache-2.0](LICENSE), for the whole repo rather than dual-licensed. It is the
licence of the `kubernetes/git-sync` credential-isolation code obsync
transcribes verbatim into one quarantined file, and it carries a patent grant,
which is not nothing for something handed a write-scoped credential.

## Contributing

obsync is **stdlib plus exactly two direct dependencies** — a filesystem-
notification library, and `golang.org/x/sys` for `flock` and `statfs`. A third
is not forbidden, it is argued for: the commit that adds it says what stdlib
could not do. The rule is written down in `go.mod`, where a third dependency
would be added, and the test suite fails if the count moves.
