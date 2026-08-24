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
