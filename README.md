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
says what changed, and push it to the tracked branch. It also knows how to
start: point it at an empty directory and it clones, at a vault that is already
a repo and it attaches on the branch that vault is on, and at anything else and
it refuses rather than adopting a folder it cannot reason about. That push
authenticates: obsync is its own git credential helper, so the token file an
operator mounts is re-read every time git asks for it and rotating it needs no
restart. Sync now runs both ways: every run fetches, works out how the vault
and the remote stand, and fast-forwards the vault when someone else pushed —
merged, never rebased. A remote whose history was rewritten underneath obsync
is detected rather than merged, and stops the network half until a human says
which history wins. When both sides changed at once, both survive: the merge is
computed entirely outside the vault, so a conflicted state never exists in it
and conflict markers never reach a note. Your version stays exactly where it is
and the other side's lands beside it as an ordinary note named
`Note (obsync conflict 2026-08-24 1403).md`, byte for byte, committed in the
same commit — resolve it by editing the two together and deleting the copy, and
the ordinary loop commits that like any other edit. What obsync will not do is
improvise: a conflict its rules have no answer for, a merge conflicting at more
paths than a person can be asked to read, and a merge whose result is a file
bigger than the remote will take each stop the half that would publish a guess,
apply nothing at all, and say what to look at — while the vault goes on being
committed locally and obsync picks itself up on the next tick once it is
settled. The loop keeps its own time too: it ticks every 60s so a change nothing
reported still arrives, waits out an unreachable remote from 60s to 15 minutes
while carrying on committing locally, and finishes the run in flight before it
exits. And it is woken by the vault itself: obsync holds an inotify watch on
every directory in it, kept in step as folders come and go, so an edit is
committed ten seconds after the vault goes quiet rather than at the next tick —
and every five minutes anyway while someone is still typing. The watch only ever
wakes the loop and never says what changed, which is why a kernel with no
watches left costs latency and nothing else: obsync says which sysctl to raise
and falls back to its tick. obsync also knows what belongs in the repo and what
does not: an ignore floor it writes into the repo's own exclude file keeps
workspace churn, the trash and OS cruft out of every commit while the rest of
`.obsidian/` is tracked, so a fresh clone is the same vault; your own
`.gitignore` outranks that floor, except for plugin settings, which are where
API keys live and which obsync refuses on the `git add` itself. A short closed
list of credential-shaped filenames, and any file over the size ceiling, are
never committed — skipped, said once, and never a reason to stop syncing
everything else. And a file that is still being written is left out of *this*
commit rather than committed in half: obsync samples each changed path twice a
second apart and leaves out anything that moved, so a note caught mid-save or a
40MB attachment still copying arrives whole on the next run instead of torn.
Nothing waives that check, and an incoming change is never applied over a path
being written at all. Standing between all of that and your vault are nine
gates and a sentinel, re-checked at the top of every run: obsync will not touch
a vault whose mount has dropped, whose `.git` has gone, whose HEAD is detached
or is mid-rebase, whose `origin` is not the remote it was given, whose branch
names no commit, or that a second obsync already holds. Each one stops obsync,
says the fact and the remedy, and clears on its own within a tick once the
cause is repaired — no restart, and obsync never exits, because a crash-looping
container buries the one message that matters. The one that matters most is
`.obsidian/`: its absence means the vault is not there, and any amount of note
deletion with it intact is you editing your vault, which obsync syncs without
comment. And after obsync writes an incoming change into your vault, it checks
that the vault holds the tree it meant to put there: a tree it cannot account
for is anchored at a ref for you to look at, and obsync stops rather than
pushing it — the one refusal that survives a restart, and the one you clear
yourself. And when the disk itself goes wrong, obsync finds out by working
rather than by scanning: it never runs `git fsck`, because every run already
reads exactly the objects it depends on, and a corrupt object announces itself
the moment something needs it. git's exit status cannot tell a rotted object
from a locked index, so time decides instead — five sync runs in a row whose
local half failed, and obsync discards the one piece of repository state that
holds no history, `.git/index`, and builds it again from HEAD. If the run after
that fails too it stops, tells you the command that failed, git's own words, and
how much room is left on the disk when there is almost none, and then retries
one read-only `git status` a tick until the repository reads again. It repairs
nothing else and never re-clones. And when a push does not land, obsync reads
what the remote actually did with it out of git's own machine field rather than
guessing: a push that lost a race to another device is retried on the next run
and nobody is told, a remote that was unreachable is waited out, and a push the
remote *rejected* — a hook, a branch protection, a quota, a pack over a limit —
stops the network half at once and tells you, because no amount of waiting
repairs a verdict. It relays the remote's own words verbatim and never guesses
at which file or which rule is the problem; your vault keeps being committed
meanwhile, the whole network half is retried once an hour so other people's
changes still arrive, and the commit the remote refused is never rewound and
never capped. The **declared surface** — everything a version number will make a
promise about — is written down ahead of the code that implements it.

And you find out about all of that without going looking. obsync answers exactly
one question about itself — *does this need a human?* — and answers it in the
three places you already have: `docker ps`, through a healthcheck that reads a
private record obsync rewrites at the end of every wake-up; `docker exec obsync
status`, which prints what it has been doing and what it is waiting on, and
always exits 0; and `docker logs`, which is **empty when nothing is wrong**. A
freeze, a remote that has rejected a push, a push that has never once succeeded,
a remote that has been unreachable for a day, and a loop that has stopped
turning are the whole of what needs you — everything else, including a remote
that is merely down, an aborted run and any amount of backoff, is obsync working
as designed. Whatever does need you is repeated once an hour and never once a
tick, so `docker logs --since 1h` is empty exactly when nothing is wrong and
never empty when something is.

And it ships as one thing. The container image is a digest-pinned Alpine base
carrying git and one static binary, and it runs as whatever UID and GID Docker's
own `user:` line names — no root entrypoint, no `PUID`/`PGID` knob, no init
process, and no `/etc/passwd` entry to need, because obsync's identity comes
from its own private git config. Docker's `HEALTHCHECK` is baked into it, so
`docker ps` answers the one question obsync answers about itself with nothing
added to your compose file. And the compose file is here:
[`compose.yaml`](compose.yaml) is the reference stack — ignis and obsync beside
each other — and it is normative rather than exemplary, which is why CI runs it
instead of reviewing it. Copy it, point `OBSYNC_REPO` at your own repository and
put your token in a file, and the decisions you did not know you had to make
have been made for you: the UID the two containers share, the vault mount,
ignis's write coalescing pinned to zero so obsync never reads a note that is
still in another process's memory, and a stop grace period long enough for
obsync to finish the run it is in rather than be killed halfway through it.

Not yet: the attention note, and the operator documentation. obsync is not
something to point at a vault yet.

## What obsync will never do

The list is at the front door on purpose: it is what decides whether obsync is
something to hand a vault to, and every entry is a line the design declined to
cross rather than a default it happens to ship. **obsync never:**

- **force-pushes** — not `--force`, not `--force-with-lease`, and there is no
  flag to turn it on. Every write to the remote is a fast-forward or it does not
  happen.
- **rebases** — a rebase walks a live vault through one checkout per replayed
  commit while Obsidian has your notes open.
- **runs `git checkout` after bootstrap** — checking a branch out rewrites the
  working tree under someone who is typing into it.
- **writes your repo's `.git/config`**, and never runs `git remote set-url`.
  Your identity and your remote stay yours; obsync only ever reads them.
- **re-clones or self-repairs a damaged repo** — a re-clone discards exactly the
  commits obsync exists to have made. A written recovery recipe replaces it.
- **discards history.** It may delete derived state such as `.git/index`; it
  never touches a commit, a blob or a file you own. Following a rewritten remote
  by **hard-resetting** onto it is the mirror image of force-pushing, and is
  refused for the same reason.
- **stashes** — a stash reverts your working tree to HEAD, so your most recent
  edits would vanish out of your open vault for the duration.
- **runs `git fsck`**, at startup or at any cadence — damage is found by
  working, never by scanning.
- **rewinds a commit the remote refused**, and imposes no cap on how far the
  local branch runs ahead of the remote.
- **diagnoses a remote rejection** — it relays the remote's own words verbatim,
  labelled as the remote's, and never guesses at a cause.
- **writes your vault's `.gitignore`** — that file is content, and it is yours.
  It is also what outranks obsync's own ignore floor, so it is how you overrule
  a default you disagree with; obsync's floor goes in the repo's exclude file,
  which is never committed.
- **deletes a file from your vault of its own accord** — the one time obsync
  stops tracking files — the workspace churn and OS cruft its ignore floor
  covers, in a vault whose history already carries them — it takes them out of
  the index and leaves every byte on disk.
- **configures git-LFS on your behalf** — if you have set it up, obsync inherits
  it for free by running git; it will never turn it on for you.
- **overwrites a conflict copy** — that is the one way this design could
  actually lose bytes.
- **exits on a sync failure.** It parks alive and keeps saying why, because a
  crash-looping container buries the one message that matters.

The entries a grep can decide are decided by one: `neverlist_test.go` reads
obsync's own source and fails the build if a forbidden argv appears in it, which
is what makes softening one of these a visible amendment rather than an edit.

## Reference deployment

The primary target is [ignis](https://github.com/Nystik-gh/ignis), which runs
Obsidian in a browser with the vault on the server. obsync runs as a sidecar
sharing the same vault volume. This is the deployment ignis itself currently
recommends for git sync ([ignis#14](https://github.com/Nystik-gh/ignis/issues/14)).

obsync is not coupled to ignis, though — it assumes an Obsidian vault on a
mounted volume, and ignis is the documented reference stack.

## Sizing

What obsync costs is a fact about the vault rather than something to configure,
so it is stated here rather than left to be found out. The method and the
numbers are in [`docs/research/sizing.md`](docs/research/sizing.md), and the
benchmarks that produced them are in the repository, so they can be re-run on
your own hardware.

**CPU is milliseconds a run.** Every sync run asks git what changed, and that is
the one cost that grows with the vault: about 5ms at a thousand notes, 12ms at
ten thousand and 43ms at fifty thousand. obsync ticks every 60 seconds, so a
vault far larger than most spends well under a tenth of a percent of one core on
it, and there is no vault size in that range at which it is worth thinking
about. A merge — computed outside the working tree every time the vault and the
remote have both moved — costs about 68ms at a thousand notes and under a
quarter of a second at fifty thousand.

**Plan disk headroom on the vault volume, and plan it for the attachments.**
When both sides changed the same file, obsync keeps both: your version stays
where it is and the other side's lands beside it as a conflict copy. The remote
pays nothing for that copy — it is a blob it already holds, at a second path —
but the vault volume carries the file **twice**, until you resolve the conflict
and delete the copy. For notes that is kilobytes. For a 90MB video edited in two
places it is 90MB, once for every such file in the merge. obsync does not check
free space before it writes, and there is no setting for a disk threshold — it
reads free space only once a local git has already failed, and then only to tell
you how much is left.

## Documentation

- [`compose.yaml`](compose.yaml) — the reference stack, ignis plus obsync. It is
  normative rather than exemplary: it is the one document whose correctness you
  inherit by copying it rather than by reading it, so it is exercised in CI.
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

And one measured against the code rather than before it:

- [`docs/research/sizing.md`](docs/research/sizing.md) — what a sync run and a
  merge cost as the vault grows, and what keeping both sides of a conflicted
  attachment costs the vault volume.

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
