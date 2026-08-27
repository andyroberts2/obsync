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

**Tick-only mode**:
The mode obsync runs in when no watcher is available — the tick becomes the only
thing that wakes the loop. Latency degrades to the tick interval; what obsync
commits does not change. Entered wholesale rather than partially: a vault where
only some of the tree is watched would sync at two different speeds with nothing
to tell them apart.
_Avoid_: polling mode, fallback mode, degraded mode

**Local half**:
The part of a sync run that touches only the vault and its `.git` — status and
commit. Cannot fail for network reasons, and keeps running when the remote is
unreachable.

**Network half**:
The part of a sync run that talks to the remote — fetch, classify, push. Fails
and backs off independently of the local half.

### Timing

**Wake interval**:
The shortest gap the watcher leaves between two wake-ups. The first event after
a quiet spell wakes the sync loop at once and everything inside the interval
after it is folded into that one wake-up, so the cost of a bulk import is
bounded by the interval rather than by the vault. It bounds the watcher's own
output, never the loop's latency: what a wake-up says is that something
happened, and a second one saying the same thing carries nothing.
_Avoid_: event debounce, throttle, coalescing window

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

**Network backoff**:
How long the network half waits after a failure before the next wake-up is
allowed to try again — doubling from a floor, never past a longest wait, and
reset by any network success. A rate limit on the loop obsync already turns,
never a schedule of its own, and never a gate on the local half. Distinct from
the **backoff ceiling**, which is a health verdict rather than a wait.
_Avoid_: retry delay, cooldown, retry schedule

**Shutdown deadline**:
How long obsync has to exit after a SIGTERM. It refuses to start a new run and
finishes the one in flight, and the deadline cuts short the one thing in that
run which can be waiting on the outside world — a network git. A local git is
never timed out, at shutdown or at any other moment.
_Avoid_: grace period, drain timeout, stop timeout

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

**Bootstrap**:
The one decision obsync makes about a directory before it syncs it, and the
only moment it may create a repo or move the working tree wholesale: clone the
remote into an empty directory, attach to a directory that is already a repo,
refuse anything else. Each answer is given by the thing that has an opinion,
which is why the tracked branch comes from the remote in the first case and from
the vault in the second. Everything obsync promises never to do "after
bootstrap" — checking a branch out, above all — is bounded by it.
_Avoid_: init, setup, first run, adopt

**Sync state**:
How the tracked branch relates to its remote counterpart at classify time: one
of *equal*, *ahead*, *behind*, or *diverged*.
_Avoid_: drift, status, sync status

**Upstream counterpart**:
The tracked branch's own ref on the remote — the thing a sync state is measured
against, and the one whose *absence* is not a sync state at all. Without it
obsync has never pushed this branch anywhere, so it asks the remote what it
holds before any bytes go: a remote with no refs at all is a brand-new one and
the first push creates the branch, and a remote holding anything else does not
get a branch nobody agreed on.
_Avoid_: upstream, tracking branch, remote branch

**Divergence**:
The sync state where both sides hold commits the other lacks. The expected
consequence of an external push landing while the vault is being edited — a
normal event obsync resolves, not an error.

**Upstream rewrite**:
The remote tip ceasing to be a descendant of the tip obsync last saw. History
has been rewritten underneath it. Distinct from divergence, and never resolved
automatically.
_Avoid_: force-push, rewind, reset

**Remote rejection**:
A push the remote received, evaluated, and declined — a hook, a policy, a quota,
or a blob over a limit obsync cannot discover. Told apart from a lost race and
from an unreachable remote by git's documented porcelain summary, never by the
prose beside it. The one network failure that no amount of waiting repairs,
which is why it escalates on its first occurrence rather than on a streak: the
remote has already returned a verdict, and a second identical one carries
nothing new. obsync *refuses*, the remote *rejects* — two verbs, two actors,
never interchangeable.
_Avoid_: push failure, rejected push, refused push

### What obsync tracks

**Ignore floor**:
The fixed set of paths obsync excludes from the vault, written to the repo's
own exclude file at every startup and never committed. A default rather than a
rule — the vault's `.gitignore` outranks it, and belongs to the user alone.
_Avoid_: ignore list, exclusions, default gitignore

**Churn subset**:
The part of the ignore floor obsync takes out of the index once, in a vault
whose history already carries it — ignore rules only affect untracked paths, so
a workspace file already committed churns forever whatever the floor says. It is
the whole floor except plugin settings, which are left alone: untracking those
would delete deliberately-synced settings from every other clone and would not
unleak a key the remote's history already holds. `git rm --cached`, once, in one
loudly-messaged commit, every byte left on disk.
_Avoid_: cleanup commit, untracking pass, purge

**Refused path**:
A path obsync will not put in a commit, whatever its state — a
credential-shaped filename, or a file over the size ceiling. Skipped, never a
reason to stop: everything else still commits and pushes. A staging-time verb:
inside a merged tree, where every path must hold something, there is nothing for
it to mean.
_Avoid_: blocked file, rejected file, skipped file

**Size ceiling**:
The largest single file obsync will commit. Set from what the remote will
accept, not from taste — the one value in this area that is configured. Applied
wherever obsync would introduce new bytes to the remote: staging, and the one
blob a merge can invent. Never applied to bytes that have already passed it —
the remote's, or the vault's own at the `git add` — which is why a conflict copy
is exempt at any size, whichever side lost.
_Avoid_: file size limit, max blob

**Owned path**:
A path obsync declared it owns and rewrites wholesale rather than edits — the
repo's exclude file, obsync's own directory and refs under `.git/`, and the
attention note. Its counterpart is a file the *human* owns, which obsync reads
and never writes: the repo's config, holding their identity and their remote,
and the vault's `.gitignore`. The distinction is what makes removing obsync a
deletion rather than an untangling, and it is the honest form of "leaves no
trace" — obsync leaves plenty, all of it in namespaces it announced.
_Avoid_: internal file, private file, obsync file

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
The losing version of a conflicted path, written beside it as a byte-identical
sibling and committed. Almost always the remote's, because the keep-both rule
gives the canonical path to the vault — and the vault's own in the one row where
git rather than obsync decides which side keeps that path, a file on one side
where the other has a directory. Never annotated, never overwritten.
_Avoid_: sidecar, conflicted copy, duplicate

**Attention note**:
The vault-root note obsync writes when it needs a human to look at something —
live freezes first, then outstanding conflict copies, refused paths, and paths
that have stayed unsettled long enough to stop looking transient, in sections.
Every section is derived — from the vault for the rest, from live gate state for
the freezes — so the note is never authoritative over what it describes, and is
deleted when every section is empty. Never itself tracked. Writing it touches
the vault, not the repo, which is why a full freeze can still write one.
_Avoid_: marker file, conflict log, conflict note

### Configuration

**Config surface**:
The complete set of values obsync is configured with. It is a public interface
rather than an accumulation: a value belongs here only if it is a fact about the
deployment, and every retired name stays recognised so an upgrade can never
silently revert one to its default.
_Avoid_: settings, options, flags

**Knob**:
One value in the config surface. Earning one is a high bar — a fact about the
vault, the remote, or the credential qualifies; a preference about timing or
policy does not, and becomes a constant instead.
_Avoid_: setting, option, parameter

**Config error**:
A refusal decidable from the configured values alone, without touching the vault
or the remote. The only condition that makes obsync exit rather than park: a
container handed nonsense cannot be repaired in place, and its operator needs it
to fail visibly.
_Avoid_: startup failure, validation error, fatal error

**Startup line**:
The single line obsync logs once it has resolved its configuration, naming
every knob it ended up with — defaulted or set — and never the credential. It
is what an operator diffs against the declared surface to find out what obsync
thinks it was told, which is why it carries the knobs it defaulted as well as
the ones it was given.
_Avoid_: banner, config dump, startup echo

**Credential file**:
The file the remote's secret is read from — re-read every time git asks for it,
rather than held from startup or cached for the life of a run. What makes a rotated credential recover on
its own, with no restart, and the reason the secret is a file at all.
_Avoid_: token, secret, password file

**Private git config**:
The per-process `GIT_CONFIG_GLOBAL` obsync writes at startup and every git it
runs reads — the commit identity, a forced `core.askPass` and the two integrity
settings. Outranked by the vault's own `.git/config`, deliberately: that is the
escape hatch a repo carrying legacy-malformed objects uses, and the vault's
config is the human's file, which obsync only ever reads.
_Avoid_: global config, gitconfig, git settings

**Credential isolation**:
The private git config plus the one setting that cannot live in it — the
credential helper, which git accumulates rather than overrides, and which is
therefore pinned per invocation after emptying the list. That single key is the
one thing the vault's own `.git/config` does not outrank, because a second
helper there is not a human overriding obsync but a human's config being handed
obsync's credential.
_Avoid_: credential setup, git isolation, helper config

**Configured remote**:
The normalised host-and-path pair obsync was told to sync with, and the thing the
repo's own remote is checked against every run. Scheme, credentials, port, case
and a `.git` suffix are not part of it — they vary without changing where bytes
go. A mismatch is a human's job to resolve; obsync never re-points a remote
itself.
_Avoid_: remote URL, upstream, origin

**Commit identity**:
The git author obsync writes under. Where provenance lives, which is why the
name — not the address — is the part that carries meaning: it is what filtering
history by author matches on, and the only part guaranteed stable across a
container's lifetime.
_Avoid_: author, signature, attribution

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
Silent, but not unrecorded: consecutive aborts in the local half are what the
local failure streak counts.
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

**Failed-apply anchor**:
The ref holding the tree obsync computed but could not verify, written before
the freeze so nothing can prune the one artifact that explains it. Its existence
is itself a gate, which is what keeps the freeze from being a property of the
running process: a restart cannot clear it, and a human clears it deliberately
by deleting it once the tree is recovered.
_Avoid_: latch, quarantine, marker

### Damage

**Damaged repo**:
A repo obsync can no longer read — a corrupt or empty object, a truncated
index, a ref pointing at nothing. Categorically unlike the states the gates
catch: it is never a cheap conclusive fact, only something a command runs into,
and waiting does not repair it. obsync detects it by working, not by checking:
every sync run already reads exactly the objects obsync depends on, which is a
proportional integrity check no scan of the whole repo improves on.
_Avoid_: corruption, broken repo, bad repo

**Local failure streak**:
The count of consecutive sync runs whose local half failed, whatever the stated
reason. The only thing permitted to conclude that a failure is permanent —
git's own words may *name* a failure, but only persistence may *escalate* one,
so behaviour never hangs on prose written for humans. Reset by any run whose
local half completes.
_Avoid_: retry count, error count, failure counter

**Derived state**:
Repository state obsync may discard because it holds no history — the index,
and nothing else. Where the never-repair rule is drawn: obsync may discard
derived state, never history. This is the whole difference between rebuilding
an index, which loses only work obsync would have committed anyway, and
re-cloning, which throws away the unpushed commits obsync exists to have made.
_Avoid_: cache, scratch state, rebuildable state

**Probe**:
The single read-only look at the vault a frozen obsync takes each tick, to find
out whether the damage is still there. Every other freeze self-clears by
re-checking a gate; this is the one that self-clears by retrying the work, so
the probe stands in for a gate that cannot exist.
_Avoid_: retry, health check, poll

### Freezes

**Full freeze**:
obsync stops touching the repo entirely — no commits, no network. Reserved for
states where committing would do the wrong thing, or is no longer possible at
all. Released by whichever fact put obsync there ceasing to be true: a gate
that starts passing again, or a probe that succeeds.
_Avoid_: halt, stop, lock

**Network freeze**:
The network half stops while the local half keeps committing. The vault keeps
being captured; nothing leaves or enters. The degraded mode an unreachable
remote already produces, reused wherever the vault is sound but its
relationship to the remote is not.
_Avoid_: offline mode, paused

### Signal

**Health**:
obsync's answer to exactly one question — *does this need a human?* Deliberately
not "is everything working": a remote that is down and backing off is behaving
as designed and is healthy until the backoff ceiling, while a freeze, a remote
rejection, or a push that has never once succeeded, is not.
_Avoid_: status, liveness, uptime

**Backoff ceiling**:
The point at which a remote that has merely gone quiet stops being healthy. Not
a retry limit — obsync keeps backing off and retrying past it, and only the
health verdict changes. The one judgement an unbounded backoff cannot make for
itself: waiting is the correct behaviour and stays correct, so nothing about
the failure ever escalates it, and only elapsed time separates a remote that
will come back from one that will not.
_Avoid_: timeout, max backoff, retry limit, give-up

**Status file**:
The private record of the loop's own state, rewritten at the end of every
wake-up — whatever the outcome, because it exists to prove the loop is still
turning, and a run that gave up still turned. Where liveness lives, so the log
never has to carry it. Private on purpose: the subcommands that read it are the
interface, not its layout.
_Avoid_: state file, heartbeat file, pid file

### Release

**Declared surface**:
Everything obsync's version number makes a promise about: the config surface,
plus the subcommands, the health contract, and what obsync writes into the
vault. Strictly larger than the config surface, and named separately because
versioning is the only thing that needs the larger set — a daemon with no
library API still has a public interface, just not one a compiler can see.
_Avoid_: public API, contract, ABI

**Surface change**:
A release that moves the declared surface. The unit release notes are obliged
to name, stated even when there is none, because "nothing you set or pinned has
changed" is the assurance an unattended sidecar exists to give.
_Avoid_: breaking change, migration note

**Supported artifact**:
The container image — the one thing obsync is shipped as and tested as. A bare
binary is a non-goal rather than a prohibition: nothing may be built in a way
that makes one impossible, and nothing is promised about one that is.
_Avoid_: build, release, distribution

**Git floor**:
The oldest git obsync runs on. Defined as the oldest git its tests run against
and never read off a release note, so the number cannot become an aspiration —
the same baseline-obsync-owns sense as the push floor and the ignore floor.
_Avoid_: minimum version, required version, supported version

**Transcription**:
Code obsync carries over from prior art verbatim rather than reimplementing,
because being wrong about it is more expensive than the duplication. Quarantined
to a file of its own under its original licence and copyright, which is what
keeps it auditable as well as compliant.
_Avoid_: vendored code, copied code, fork

### Documentation

**Load-bearing documentation**:
A documented line that is the only thing standing where the design deliberately
declined to put code. The test is *the code chose not to do this*, never *this
is important* — which is why it names the decision that put it there, is never
cut for brevity, and is a defect rather than a gap when missing. Marked visibly
and in place: a class only the maintainer can see does half its work, and the
operator has a right to know which lines are the ones left to them.
_Avoid_: important note, warning, caveat, gotcha

**Reference compose**:
The compose file obsync publishes — normative rather than exemplary, because it
is the one document whose correctness an operator inherits by *copying* instead
of by reading. That is also why it is exercised in CI rather than reviewed:
load-bearing prose can only be read, a load-bearing file can be run.
_Avoid_: example compose, sample, template, quickstart
