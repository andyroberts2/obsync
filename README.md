# obsync

Bidirectional git sync for a self-hosted Obsidian vault, as a Docker sidecar.

obsync watches a mounted Obsidian vault, commits and pushes local edits on a
sensible cadence, and pulls upstream changes back down — keeping the vault and
a remote GitHub repo continuously in step.

It exists because the tools either side of this gap only do half the job:
[`kubernetes/git-sync`](https://github.com/kubernetes/git-sync) pulls but never
pushes, and [`simonthum/git-sync`](https://github.com/simonthum/git-sync) is a
one-shot script with no daemon, debounce, or credential handling.

## Does obsync fit?

**Yes, if this is your deployment:**

- Your vault lives on a server and you write into it through a browser —
  [ignis](https://github.com/Nystik-gh/ignis) is the reference stack, and obsync
  is not coupled to it.
- One continuous writer, plus pushes from your other devices now and then.
- You have a git remote you control, and a credential that can write to one
  repository on it.
- You can run a sidecar container that shares the vault volume and the UID that
  owns it.

**No, if any of these is true:**

- **You run Obsidian's own Sync** — see below.
- You want three devices each running their own sync daemon against one branch.
  That needs per-device branches and real merge machinery, and obsync settled on
  one continuous writer at the outset.
- You want one container to sync several vaults, several remotes or several
  branches. Two vaults is two obsync services; each needs its own repository,
  branch and credential anyway.
- You want a metrics endpoint to scrape, or git-LFS configured for you. Neither
  exists, and neither is coming.
- What you have is a directory rather than an Obsidian vault. obsync is
  Obsidian-specific by choice: it proves the vault is really there by looking
  for `.obsidian/`, and it knows what belongs in the repo because it knows what
  Obsidian puts in one.

> **Load-bearing documentation** (§11, [#16](../../issues/16)).
> **obsync cannot detect Obsidian's own Headless Sync, and cannot coordinate
> with it.** If you run it, decide against obsync here rather than debugging it
> later.
>
> The plugin is a second sync system writing your vault, and obsync has no way
> to see it from a sidecar: its state lives in ignis's unmounted data root, its
> process sits in ignis's PID namespace, and the one signal it leaves in the
> vault is deliberately masked while it runs. obsync treats it as an ordinary
> third writer — which is a thing this design tolerates rather than a thing it
> resolves. What you would get is conflict copies nobody wrote, runs that
> abandon themselves, and two systems arbitrating one vault with no rule
> between them. The failure signature is in
> [`docs/operations.md`](docs/operations.md#when-something-else-is-writing-the-vault);
> the decision is here, because it decides whether obsync is for you at all.
> Never cut this warning.

## Quickstart

Four steps, and the third one is the whole configuration.

1. **Make a repository** on your remote, and a credential that can write to it.
   The minimum scope for GitHub, GitLab, Gitea and an SSH deploy key is
   [`docs/credentials.md`](docs/credentials.md) — give obsync the least that
   works.
2. **Copy [`compose.yaml`](compose.yaml)** out of this repository. It is
   normative rather than exemplary: the decisions in it are decisions, and
   copying it is how you inherit the ones you did not know you had to make.
3. **Change three things in it** — `OBSYNC_REPO` to your repository, the token
   into `./secrets/obsync-token`, and `./vaults/notes` to your vault folder.
   `OBSYNC_REPO` is the only variable obsync will not start without. Nothing
   else in that file is a placeholder.
4. **`docker compose up -d`.**

Point obsync at an empty directory and it clones the remote into it. Point it at
a vault that is already a git repository and it attaches, on the branch that
vault is already on. Point it at a non-empty directory that is not a repository
and it refuses, rather than adopting a folder it cannot reason about.

Then check it, once:

```bash
docker compose exec obsync obsync status   # what it has done, and what it is waiting on
docker ps                                 # the image carries its own HEALTHCHECK
```

Write a note in the vault, wait a minute, and look at your remote. Ten seconds
after you stop typing, obsync commits — and every five minutes anyway if you
never stop.

After that there is nothing to do. A healthy obsync writes nothing to its log
for hours; when it needs you it says so in
[`obsync-attention.md`](docs/operations.md#start-here) at the root of your
vault, and `docker ps` turns unhealthy. What to do when it does is
[`docs/operations.md`](docs/operations.md).

> **Nothing is published to a registry yet.** `ghcr.io/andyroberts2/obsync:0.3`
> is what the reference compose pins and what the first release will put there
> ([#43](../../issues/43)). Until then, build it: `docker build -t obsync:dev .`
> and point the compose file's `image:` at `obsync:dev`.

## What obsync will never do

> **Load-bearing documentation** (§11, [#16](../../issues/16)).
> **This list is the one document load-bearing in the other direction.** Every
> other line of this kind exists so that you *do* something; this one exists so
> that obsync doesn't — each entry is a line the design deliberately declined to
> cross rather than a default it happens to ship, and there is no flag for any of
> them.
>
> It is at the front door because trust is the adoption barrier here: you are
> handing a personal vault to a daemon that holds a write-scoped credential and
> runs unattended at 3am. **Every entry names the ticket that decided it**, so
> softening one is visibly an amendment rather than an edit. Never cut an entry
> for brevity.

**obsync never:**

- **force-pushes** — not `--force`, not `--force-with-lease`, and there is no
  flag to turn it on. Every write to the remote is a fast-forward or it does not
  happen. (§3, [#6](../../issues/6))
- **rebases** — a rebase walks a live vault through one checkout per replayed
  commit while Obsidian has your notes open. (§3, [#6](../../issues/6))
- **runs `git checkout` after bootstrap** — checking a branch out rewrites the
  working tree under someone who is typing into it. (§3,
  [#6](../../issues/6))
- **writes your repo's `.git/config`**, and never runs `git remote set-url`.
  Your identity and your remote stay yours; obsync only ever reads them. (§8,
  [#12](../../issues/12))
- **re-clones or self-repairs a damaged repo** — a re-clone discards exactly the
  commits obsync exists to have made. A written recovery recipe replaces it.
  (§7, [#15](../../issues/15))
- **discards history.** It may delete derived state such as `.git/index`; it
  never touches a commit, a blob or a file you own. (§7,
  [#15](../../issues/15)) Following a rewritten remote by **hard-resetting**
  onto it is the mirror image of force-pushing, and is refused for the same
  reason. (§3, [#6](../../issues/6))
- **stashes** — a stash reverts your working tree to HEAD, so your most recent
  edits would vanish out of your open vault for the duration. (§3,
  [#6](../../issues/6))
- **runs `git fsck`**, at startup or at any cadence — damage is found by
  working, never by scanning. (§7, [#15](../../issues/15))
- **rewinds a commit the remote refused**, and imposes no cap on how far the
  local branch runs ahead of the remote. (§7, [#18](../../issues/18))
- **diagnoses a remote rejection** — it relays the remote's own words verbatim,
  labelled as the remote's, and never guesses at a cause. (§7,
  [#18](../../issues/18))
- **writes your vault's `.gitignore`** — that file is content, and it is yours.
  It is also what outranks obsync's own ignore floor, so it is how you overrule
  a default you disagree with; obsync's floor goes in the repo's exclude file,
  which is never committed. (§5, [#8](../../issues/8))
- **deletes a file from your vault of its own accord** — the one time obsync
  stops tracking files — the workspace churn and OS cruft its ignore floor
  covers, in a vault whose history already carries them — it takes them out of
  the index and leaves every byte on disk. (§5, [#8](../../issues/8))
- **configures git-LFS on your behalf** — if you have set it up, obsync inherits
  it for free by running git; it will never turn it on for you. (§5,
  [#8](../../issues/8))
- **overwrites a conflict copy** — that is the one way this design could
  actually lose bytes. (§4, [#7](../../issues/7))
- **exits on a sync failure.** It parks alive and keeps saying why, because a
  crash-looping container buries the one message that matters. (§2,
  [#5](../../issues/5))

The entries a grep can decide are decided by one: `neverlist_test.go` reads
obsync's own source and fails the build if a forbidden argv appears in it, which
is what makes softening one of these a visible amendment rather than an edit.

## Documentation

- [`compose.yaml`](compose.yaml) — the reference stack, ignis plus obsync. It is
  normative rather than exemplary: it is the one document whose correctness you
  inherit by copying it rather than by reading it, so it is exercised in CI.
- [`docs/interface.md`](docs/interface.md) — the declared surface: the nine
  environment variables, the four subcommands, the health contract, and what
  obsync writes into your vault. It is what SemVer is measured over, and what
  every release's "Surface changes" note is about.
- [`docs/credentials.md`](docs/credentials.md) — the minimum credential scope
  for GitHub, GitLab, Gitea and an SSH deploy key, how you find out you got it
  wrong, and what obsync does with the secret. Read once, when you deploy.
- [`docs/operations.md`](docs/operations.md) — the three tiers, what clears a
  freeze, why restarting is the wrong reflex, and the recipes for the two
  things obsync deliberately will not do for you. Read at 3am.
- [`SECURITY.md`](SECURITY.md) — how to report something privately.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — the transcription rule, and nothing
  else.

There is no documentation site and no wiki, deliberately: a rendered page
describing whichever image you happen to be running re-introduces on the prose
side exactly the failure digest-pinning removes on the image side. The README at
a tag is already the versioned document.

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
private record obsync rewrites at the end of every wake-up;
`docker compose exec obsync obsync status`, which prints what it has been doing
and what it is waiting on, and always exits 0; and `docker logs`, which is **empty when nothing is wrong**. A
freeze, a remote that has rejected a push, a push that has never once succeeded,
a remote that has been unreachable for a day, and a loop that has stopped
turning are the whole of what needs you — everything else, including a remote
that is merely down, an aborted run and any amount of backoff, is obsync working
as designed. Whatever does need you is repeated once an hour and never once a
tick, so `docker logs --since 1h` is empty exactly when nothing is wrong and
never empty when something is.

And there is a fourth place, which is the one you are actually looking at: your
vault. When obsync needs a human it writes `obsync-attention.md` at the vault
root, and **deletes it** when it does not — so the note being there at all is
the signal. It has four sections and it gains and loses them as reality
changes: whatever obsync has frozen, with the fact behind it and what to do,
closing on *"this clears on its own once fixed; no restart needed"*; every
conflict copy still standing, each wikilinked beside the note it is a copy of
so you resolve the pair inside Obsidian; every path obsync is refusing to
commit; and every path something is rewriting faster than obsync can ever see
it still. Every section is worked out again from scratch on every run — the
freezes from what the gates just found, the rest from your vault — so the note
is never in charge of what it describes and cannot disagree with it for longer
than one run. A push the remote rejected carries the remote's own words in a
fenced block, labelled as the remote's rather than obsync's. The note is in the
ignore floor, so it is never committed and reaches no other clone.

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

Not yet: the release. Nothing is published to a registry, so the image the
reference compose pins does not resolve and the version `obsync status` reports
is `dev` — [#43](../../issues/43) is where the tags, the provenance attestation
and the "Surface changes" check land. Until then obsync is something to build
and try rather than something to leave pointed at the only copy of a vault.

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
