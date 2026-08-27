# obsync

Two-way git sync for a self-hosted Obsidian vault, as a Docker sidecar.

This app has been built to address a very specific need: When using ignis in
order to serve obsidian as a web app, the standard obsidian git plugin cannot
be used in-browser, so this is not an option to keep changes in sync with a
remote git repo. This sidecar is a workaround to achieve a similar outcome.

obsync watches a mounted Obsidian vault (the same folder that ignis is using).
It commits and pushes what you write, and it pulls down what other people push,
so the vault and a remote git repository stay in step.

It takes prior art as inspiration, and builds on these learnings:
- [`kubernetes/git-sync`](https://github.com/kubernetes/git-sync) pulls but doesn't push.
- [`simonthum/git-sync`](https://github.com/simonthum/git-sync) does also push,
but doesn't appear to be maintained, has no daemon, no debounce and no credential handling.

## Does obsync fit your use case?

**Yes, if:**

- Your vault lives on a server, and you write into it through a browser.
  [ignis](https://github.com/Nystik-gh/ignis) is the reference stack, and obsync
  is not tied to it.
- One writer works in the vault continuously, and your other devices push now
  and then.
- You have a git remote you control, and a credential that can write to one
  repository on it.
- You can run a sidecar container that shares the vault volume and the UID that
  owns it.

**No, if any of these is true:**

- **You run Obsidian's own Sync.** Read the warning below.
- You want three devices each running their own sync daemon against one branch.
  That needs per-device branches and real merge machinery. obsync is built for
  one continuous writer.
- You want one container to sync several vaults, remotes or branches. Two vaults
  means two obsync services, and each needs its own repository, branch and
  credential.
- You want a metrics endpoint to scrape, or git-LFS configured for you. Neither
  exists, and neither is planned.
- What you have is a directory rather than an Obsidian vault. obsync is
  Obsidian-specific by choice: it looks for `.obsidian/` to prove the vault is
  really there, and it knows what belongs in the repository because it knows
  what Obsidian puts in one.

> **Important** ([#16](../../issues/16)).
> **obsync cannot detect Obsidian's own Headless Sync, and cannot coordinate
> with it.** If you run Headless Sync, decide against obsync here rather than
> debugging it later.

## Quickstart

Four steps, and the third one is the whole configuration.

1. **Make a repository** on your remote, and a credential that can write to it.
   Give obsync the least scope that works. The minimum for GitHub, GitLab, Gitea
   and an SSH deploy key is in [`docs/credentials.md`](docs/credentials.md).
2. **Copy [`compose.yaml`](compose.yaml)** out of this repository. If using
   ignis, then you can add it to your ignis `docker-compose.yml` file.
3. **Change three things in it.** Point `OBSYNC_REPO` at your repository, put
   your token in `./secrets/obsync-token` (just the raw token - nothing else),
   and change `./vaults/notes` to point to your own vault folder.
   `OBSYNC_REPO` is the only variable obsync will not start without.
4. **Run `docker compose up -d`.**

What obsync does at startup depends on the directory you point it at:

| The directory | What obsync does |
|---|---|
| Empty | Clones the remote into it |
| Already a git repository | Attaches to it, on the branch that vault is already on |
| Anything else | Refuses it, rather than adopting a folder it cannot reason about |

Then check it, once:

```bash
docker compose exec obsync obsync status   # what it has done, and what it is waiting on
docker ps                                  # the image carries its own HEALTHCHECK
```

Write a note in the vault, wait a minute, and look at your remote. obsync
commits ten seconds after you stop typing, and every five minutes while you keep
going.

After that there is nothing to do. A healthy obsync writes nothing to its log
for hours. When it needs you, it writes
[`obsync-attention.md`](docs/operations.md#start-here) at the root of your
vault, and `docker ps` turns unhealthy.
[`docs/operations.md`](docs/operations.md) is what to do then.

## What obsync does

**It syncs both ways, on its own clock.**

- Every 60 seconds obsync asks git what changed in the vault. It commits the
  change as one commit whose message says what changed, then pushes that commit
  to the tracked branch.
- An inotify watch on every directory in the vault wakes obsync early, so a
  commit lands ten seconds after the vault goes quiet. The watch only wakes the
  loop and never says what changed, so a kernel with no watches left costs
  latency and nothing else. obsync names the sysctl to raise and falls back to
  its tick.
- Every run also fetches. When someone else pushed, obsync fast-forwards the
  vault. It merges and never rebases.
- A file that is still being written is left out of *this* commit rather than
  committed in half. obsync samples each changed path twice, a second apart, and
  leaves out anything that moved. A 40MB attachment still copying arrives whole
  on the next run.
- An unreachable remote is waited out, from 60 seconds up to 15 minutes, while
  obsync carries on committing locally.

**When both sides changed, both survive.**

- Your version stays exactly where it is. The other side's version lands beside
  it as an ordinary note, named `Note (obsync conflict 2026-08-24 1403).md`,
  byte for byte, committed in the same commit.
- The merge is computed entirely outside the vault, so a conflicted state never
  exists in it and conflict markers never reach a note.
- Resolve a conflict by editing the two notes together and deleting the copy.
  The ordinary loop commits that like any other edit.
- A conflict obsync's rules have no answer for stops the half that would publish
  a guess. obsync applies nothing at all and says what to look at, while the
  vault goes on being committed locally.

**It knows what belongs in the repository.**

- An ignore floor, written into the repository's own exclude file, keeps
  workspace churn, the trash and OS cruft out of every commit. The rest of
  `.obsidian/` is tracked, so a fresh clone is the same vault.
- Your own `.gitignore` outranks that floor. The one exception is plugin
  settings, which are where API keys live, and obsync refuses those on the
  `git add` itself.
- A short closed list of credential-shaped filenames, and any file over the size
  ceiling, are never committed. obsync says so once and keeps syncing everything
  else.

**It stops rather than guess.**

- obsync re-checks nine gates and one sentinel at the top of every run. It will
  not touch a vault whose mount has dropped, whose `.git` has gone, or whose
  HEAD is on no branch. It also stops for an `origin` that is not the remote it
  was given, a branch that names no commit, and a vault a second obsync already
  holds.
- Each one stops obsync, states the fact and the remedy, and clears on its own
  within a tick once you repair the cause. No restart is needed, and obsync
  never exits.
- The sentinel that matters most is `.obsidian/`. Its absence means the vault is
  not there. Any amount of note deletion with it intact is you editing your
  vault, which obsync syncs without comment.
- After obsync writes an incoming change, it checks that the vault holds the
  tree it meant to write. A tree obsync cannot account for is anchored at a ref
  for you to look at, and obsync stops rather than pushing it.
- obsync never runs `git fsck`. It finds damage by working, because every run
  already reads exactly the objects it depends on. Five sync runs in a row whose
  local half failed, and obsync rebuilds `.git/index` from HEAD. If the next run
  fails too, obsync stops and tells you the failing command, git's own words, and
  how much room is left on the disk.
- When a push does not land, obsync reads what the remote did with it out of
  git's own machine-readable field rather than guessing. A push that lost a race
  is retried quietly. A push the remote *rejected* stops the network half at
  once, and obsync relays the remote's own words verbatim.

**It answers one question about itself: does this need a human?**

- `docker ps` — the image carries its own HEALTHCHECK, which reads a private
  record obsync rewrites at the end of every wake-up.
- `docker compose exec obsync obsync status` — what obsync has been doing and
  what it is waiting on. It always exits 0.
- `docker logs` — **empty when nothing is wrong.** Anything that needs you is
  repeated once an hour and never once a tick, so `docker logs --since 1h` is
  empty exactly when nothing is wrong and never empty when something is.
- `obsync-attention.md` at the root of your vault. obsync writes it when it
  needs a human and **deletes** it when it does not, so the note being there at
  all is the signal. It carries live freezes with the fact and the remedy,
  outstanding conflict copies wikilinked beside the notes they copy, refused
  paths, and paths something is rewriting faster than obsync can see them
  settle. Every section is worked out again from scratch on every run.

**It ships as one thing.**

- A digest-pinned Alpine base carrying git and one static binary.
- It runs as whatever UID and GID Docker's own `user:` line names. There is no
  root entrypoint, no `PUID`/`PGID` setting, no init process, and no
  `/etc/passwd` entry to need.
- One pushed annotated tag builds amd64 and arm64, pushes the same image under
  four names, and attaches a build-provenance attestation and an SBOM.

## What obsync will never do

> **See** ([#16](../../issues/16)).
> **This list exists so that obsync does not.** Each entry is a line the design
> deliberately declined to cross, and there is no flag for any of them.
>
> It is at the front door because trust is the adoption barrier here: you are
> handing a personal vault to a daemon that holds a write-scoped credential and
> runs unattended at 3am. Every entry names the ticket that decided it, so
> softening one is visibly an amendment rather than an edit. Never cut an entry
> for brevity.

**obsync never:**

- **force-pushes** — not `--force`, not `--force-with-lease`, and there is no
  flag to turn it on. Every write to the remote is a fast-forward, or it does
  not happen. ([#6](../../issues/6))
- **rebases** — a rebase walks a live vault through one checkout per replayed
  commit while Obsidian has your notes open. ([#6](../../issues/6))
- **runs `git checkout` after bootstrap** — checking a branch out rewrites the
  working tree under someone who is typing into it. ([#6](../../issues/6))
- **writes your repo's `.git/config`**, and **never runs `git remote set-url`**.
  Your identity and your remote stay yours, and obsync only ever reads them.
  ([#12](../../issues/12))
- **re-clones or self-repairs a damaged repo** — a re-clone discards exactly the
  commits obsync exists to have made. A written recovery recipe replaces it.
  ([#15](../../issues/15))
- **discards history.** It may delete derived state such as `.git/index`, and it
  never touches a commit, a blob or a file you own. ([#15](../../issues/15))
  Following a rewritten remote by **hard-resetting** onto it is the mirror image
  of force-pushing, and is refused for the same reason. ([#6](../../issues/6))
- **stashes** — a stash reverts your working tree to HEAD, so your most recent
  edits would vanish out of your open vault for the duration.
  ([#6](../../issues/6))
- **runs `git fsck`**, at startup or at any cadence. Damage is found by working,
  never by scanning. ([#15](../../issues/15))
- **rewinds a commit the remote refused**, and imposes no cap on how far the
  local branch runs ahead of the remote. ([#18](../../issues/18))
- **diagnoses a remote rejection** — it relays the remote's own words verbatim,
  labelled as the remote's, and never guesses at a cause.
  ([#18](../../issues/18))
- **writes your vault's `.gitignore`** — that file is content, and it is yours.
  It is also what outranks obsync's own ignore floor, so it is how you overrule
  a default you disagree with. obsync's floor goes in the repository's exclude
  file, which is never committed. ([#8](../../issues/8))
- **deletes a file from your vault of its own accord** — obsync stops tracking
  files exactly once, for the workspace churn and OS cruft its ignore floor
  covers. It takes them out of the index and leaves every byte on disk.
  ([#8](../../issues/8))
- **configures git-LFS on your behalf** — if you have set it up, obsync
  inherits it for free by running git. It will never turn it on for you.
  ([#8](../../issues/8))
- **overwrites a conflict copy** — that is the one way this design could
  actually lose bytes. ([#7](../../issues/7))
- **exits on a sync failure.** It parks alive and keeps saying why, because a
  crash-looping container buries the one message that matters.
  ([#5](../../issues/5))

The entries a grep can decide are decided by one. `neverlist_test.go` reads
obsync's own source and fails the build if a forbidden argv appears in it.

## Documentation

- [`compose.yaml`](compose.yaml) — the reference stack, ignis plus obsync. Copy
  it rather than read it. CI runs it.
- [`docs/interface.md`](docs/interface.md) — the declared surface: the nine
  environment variables, the four subcommands, the health contract, and what
  obsync writes into your vault. This is what SemVer is measured over.
- [`docs/credentials.md`](docs/credentials.md) — the minimum credential scope
  for GitHub, GitLab, Gitea and an SSH deploy key, how you find out you got it
  wrong, and what obsync does with the secret. Read once, when you deploy.
- [`docs/operations.md`](docs/operations.md) — the three tiers, what clears a
  freeze, why restarting is the wrong reflex, and the recipes for the two things
  obsync will not do for you. Read at 3am.
- [`SECURITY.md`](SECURITY.md) — how to report something privately.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — the transcription rule, and nothing
  else.

There is no documentation site and no wiki. A rendered page describing whichever
image you happen to be running drifts from that image the moment it is
published. The README at a tag is already the versioned document.

## Reference deployment

The primary target is [ignis](https://github.com/Nystik-gh/ignis), which runs
Obsidian in a browser with the vault on the server. obsync runs as a sidecar
sharing the same vault volume. This is the deployment ignis itself recommends
for git sync ([ignis#14](https://github.com/Nystik-gh/ignis/issues/14)).

obsync is not tied to ignis. It needs an Obsidian vault on a mounted volume, and
ignis is the documented reference stack.

## Sizing

What obsync costs is a fact about your vault rather than something to configure.
The method and the numbers are in
[`docs/research/sizing.md`](docs/research/sizing.md), and the benchmarks are in
the repository, so you can re-run them on your own hardware.

**CPU is milliseconds a run.** Every sync run asks git what changed, and that
is the one cost that grows with the vault. It takes about 5ms at a thousand
notes, 12ms at ten thousand, and 43ms at fifty thousand. obsync ticks every 60
seconds, so even a very large vault spends well under a tenth of a percent of
one core. A merge costs about 68ms at a thousand notes, and under a quarter of a
second at fifty thousand.

**Plan disk headroom on the vault volume, and plan it for the attachments.**
When both sides changed the same file, obsync keeps both. The remote pays
nothing for the copy, because it is a blob the remote already holds at a second
path. The vault volume carries the file **twice**, until you resolve the
conflict and delete the copy. For notes that is kilobytes. For a 90MB video
edited in two places it is 90MB, once for every such file in the merge. obsync
does not check free space before it writes, and there is no setting for a disk
threshold.

## Research

Prior art and constraints, gathered before any design decisions were made:

- [`docs/research/kubernetes-git-sync.md`](docs/research/kubernetes-git-sync.md)
  — architecture, the worktree and symlink model, auth, and why it cannot be
  forked or reused for a live vault.
- [`docs/research/simonthum-git-sync.md`](docs/research/simonthum-git-sync.md)
  — the sync algorithm, and its safety gates, which are the valuable part.
- [`docs/research/ignis-and-obsidian-vaults.md`](docs/research/ignis-and-obsidian-vaults.md)
  — how ignis touches the filesystem, and Obsidian vault git conventions.

And one measured against the code rather than before it:

- [`docs/research/sizing.md`](docs/research/sizing.md) — what a sync run and a
  merge cost as the vault grows, and what keeping both sides of a conflicted
  attachment costs the vault volume.

## Licence

[Apache-2.0](LICENSE), for the whole repository. It is the licence of the
`kubernetes/git-sync` credential-isolation code that obsync transcribes verbatim
into one quarantined file, and it carries a patent grant.

## Contributing

obsync is **stdlib plus exactly two direct dependencies**: a filesystem
notification library, and `golang.org/x/sys` for `flock` and `statfs`. A third
is not forbidden, but it has to be argued for, and the commit that adds it says
what stdlib could not do. The rule lives in `go.mod`, and the test suite fails
if the count moves.
