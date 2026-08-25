# obsync

obsync keeps a mounted Obsidian vault continuously in step with a remote git
repository — committing and pushing edits made through the vault, and pulling
upstream changes back down. This file is the project's glossary: the words the
design uses, and the ones it deliberately doesn't.

## Language

### The vault and its writers

**Vault**:
The Obsidian vault directory obsync manages — one mounted folder containing
notes, attachments, and `.obsidian/`, with its own `.git` at the root.
_Avoid_: repo, workspace, directory

**Writer**:
Any process that mutates vault files. obsync assumes more than one, always.
_Avoid_: client, editor

**Third writer**:
A writer obsync can neither see nor coordinate with — canonically ignis's
Headless Sync plugin, which spawns `ob sync --continuous` against the same
vault. The reason obsync trusts git's view of the tree over any event stream.

### The sync loop

**Sync loop**:
The single serialized process that reconciles the vault with the remote. Only
one run of it is ever in flight.
_Avoid_: daemon, worker, scheduler

**Sync run**:
One pass of the loop: read the tree's state, commit if dirty, fetch, classify,
push. The unit of work, and the unit that produces at most one commit.
_Avoid_: sync, cycle, iteration

**Tick**:
The periodic wake-up that starts a sync run when nothing else has. Its interval
is the upper bound on how stale obsync's view of both the vault and the remote
can be.
_Avoid_: poll, interval, period

**Watcher**:
The filesystem watch over the vault. Its only role is to wake the loop sooner
than the next tick would; it never tells obsync *what* changed.
_Avoid_: listener, observer

**Local half**:
The part of a sync run that touches only the vault and its `.git` — status and
commit. Cannot fail for network reasons, and keeps running when the remote is
unreachable.

**Network half**:
The part of a sync run that talks to the remote — fetch, classify, push. Fails
and backs off independently of the local half.

### Timing

**Quiet window**:
How long the vault must go unmodified before a sync run is allowed to commit.
Distinguishes "the human paused" from "the human is mid-sentence."
_Avoid_: debounce, settle time, idle timeout

**Max-wait cap**:
The ceiling on how long the quiet window may defer a commit. Guarantees forward
progress on a vault that is never quiet.
_Avoid_: max debounce, deadline

**Push floor**:
An optional lower bound on time between pushes. A rate limit on the network
half, not a second schedule.
_Avoid_: push interval, push period

### Torn writes

**Settle guard**:
The per-path check that a file is not still being written: its size and mtime,
sampled twice across the settle interval, must be unchanged. Distinct from the
quiet window in kind, not degree — the quiet window is about history
readability and the max-wait cap may waive it; the settle guard is about valid
bytes and nothing waives it.
_Avoid_: settle window, write debounce, stability check

**Settle interval**:
The gap between the settle guard's two samples. Not configurable — a constant
read off what the filesystem does, not a preference.
_Avoid_: settle delay, stability threshold

**Unsettled path**:
A path whose size or mtime moved across the settle interval. Neither committed
nor overwritten while it stays that way. The second thing the committable set
subtracts, alongside refused paths — and, once a path stays unsettled long
enough to stop looking transient, a section of the attention note.
_Avoid_: dirty file, in-flight file, busy file

**Stage-verify**:
The check that nothing moved on disk while a sync run was staging it. The read
side's counterpart to write-verify: together they mean obsync verifies both ends
of every tree it touches. Its subject is the third writer, whose writes no
sampling window can anticipate.
_Avoid_: recheck, restat, double-read

### The branch and the remote

**Tracked branch**:
The single branch obsync syncs. Resolved once at startup — the vault's current
HEAD when attaching to an existing repo, the remote's default branch when
cloning fresh — and fixed for the process lifetime.
_Avoid_: sync branch, target branch, main

**Sync state**:
How the tracked branch relates to its remote counterpart at classify time: one
of *equal*, *ahead*, *behind*, or *diverged*.
_Avoid_: drift, status, sync status

**Divergence**:
The sync state where both sides hold commits the other lacks. The expected
consequence of an external push landing while the vault is being edited — a
normal event obsync resolves, not an error.

**Upstream rewrite**:
The remote tip ceasing to be a descendant of the tip obsync last saw. History
has been rewritten underneath it. Distinct from divergence, and never resolved
automatically.
_Avoid_: force-push, rewind, reset

### What obsync tracks

**Ignore floor**:
The fixed set of paths obsync excludes from the vault, written to the repo's
own exclude file at every startup and never committed. A default rather than a
rule — the vault's `.gitignore` outranks it, and belongs to the user alone.
_Avoid_: ignore list, exclusions, default gitignore

**Refused path**:
A path obsync will not put in a commit, whatever its state — a
credential-shaped filename, or a file over the size ceiling. Skipped, never a
reason to stop: everything else still commits and pushes.
_Avoid_: blocked file, rejected file, skipped file

**Size ceiling**:
The largest single file obsync will commit. Set from what the remote will
accept, not from taste — the one value in this area that is configured.
_Avoid_: file size limit, max blob

**Committable set**:
The paths a sync run would actually stage, once the ignore floor, refused paths,
and unsettled paths are taken out. What "dirty" means to the loop: a tree
holding nothing but refused and unsettled paths is quiet, and produces no
commit.
_Avoid_: changed files, working set, staged set

### Conflicts

**Keep-both rule**:
The single rule for resolving a conflicted path: the vault's view of the path
wins — including absence, where the vault deleted it — and the remote's losing
bytes are preserved as a conflict copy. There is exactly one rule, and it is not
configurable.
_Avoid_: conflict resolution, merge strategy

**Out-of-tree merge**:
Computing and resolving a merge entirely outside the working tree, so the vault
only ever sees the finished result. The reason raw conflict markers can never
reach a note.
_Avoid_: dry run, staged merge, shadow merge

**Conflict copy**:
The losing remote version of a conflicted path, written beside it as a
byte-identical sibling and committed. Never annotated, never overwritten.
_Avoid_: sidecar, conflicted copy, duplicate

**Attention note**:
The vault-root note obsync writes when it needs a human to look at something —
outstanding conflict copies, refused paths, and paths that have stayed unsettled
long enough to stop looking transient, in sections. Derived from what is in the
vault, never authoritative over it, deleted when every section is empty,
and never itself tracked.
_Avoid_: marker file, conflict log, conflict note

### Interlocks

**Gate**:
A condition that must hold before obsync is allowed to act on the vault at
all. Every gate is a conclusive fact rather than a judgement, and gates are
re-checked at the top of every sync run — so a gate that starts failing stops
obsync, and a gate the human repairs releases it, with no restart in between.
_Avoid_: check, guard, precondition, validation

**Aborted run**:
A sync run that gives up before changing anything, leaving the next run to try
again. Distinct from a freeze: nothing is wrong with the vault or the remote,
this pass simply lost a race. Never reported — a transient loss is not news.
_Avoid_: failed run, retry, skipped run

**Vault sentinel**:
The presence of `.obsidian/` as obsync's proof that the vault is really there.
Its absence means the mount is gone or misdirected, not that the human deleted
their notes — the one distinction that separates infrastructure failure from a
legitimate, syncable edit.
_Avoid_: health check, liveness probe, canary

**Write-verify**:
The check that the tree obsync applied to the vault is the tree it computed.
The last interlock before a result becomes a pushed result, and the only one
whose failure means obsync can no longer trust its own view of the vault.
_Avoid_: validation, assertion, sanity check

### Freezes

**Full freeze**:
obsync stops touching the repo entirely — no commits, no network. Reserved for
states where even committing would do the wrong thing.
_Avoid_: halt, stop, lock

**Network freeze**:
The network half stops while the local half keeps committing. The vault keeps
being captured; nothing leaves or enters. The degraded mode an unreachable
remote already produces, reused wherever the vault is sound but its
relationship to the remote is not.
_Avoid_: offline mode, paused
