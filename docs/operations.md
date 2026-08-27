# Operating obsync

obsync answers exactly one question about itself — **does this need a human?**
This page is what to do when the answer is yes.

It does not list what obsync can be frozen on. The running system enumerates
that itself, more accurately than a page can: the attention note's first section
is derived from live state on every run and names the freeze, the conclusive
fact behind it, and the remedy. What is here is the shape — the three tiers,
what clears each of them, and the four things obsync cannot tell you itself.

---

## Start here

In this order. The first two are seconds and answer most of it.

1. **Read `obsync-attention.md` at the root of the vault.** obsync writes it
   when it needs a human and **deletes** it when it does not, so the file being
   there at all is the signal. Four sections, in this order: live freezes, with
   the fact and the remedy for each; outstanding conflict copies, each
   wikilinked beside the note it is a copy of; refused paths; and paths
   something is rewriting faster than obsync can ever see them still. A section
   with nothing in it is absent rather than empty.
2. **`docker compose exec obsync obsync status`** — the service, then the
   binary, because `docker exec` runs a command rather than the image's
   entrypoint. The same state, from the process rather than from the vault, plus
   the build version. It always exits 0: it is a report, not a verdict.
3. **`docker compose logs --since 1h obsync`.** Empty is the designed state: a healthy
   obsync writes nothing for hours, and anything needing a human is repeated
   once an hour, so an hour of log that is empty means an hour with nothing
   wrong. `OBSYNC_LOG_LEVEL=debug` adds every git invocation with its full argv,
   exit status and duration, and is safe to turn on — the credential is never in
   an argv, a URL or a log line.
4. **`docker ps`.** The image carries its own `HEALTHCHECK`, so the health
   column is obsync's answer to the one question. What each verdict covers is
   [`interface.md`](interface.md#3-the-health-contract); what your runtime does
   with it is [below](#health-and-your-runtime).

---

## Do not restart it

> **Load-bearing documentation** (§11, [#16](../../issues/16)).
> **"This clears on its own once fixed; no restart needed."**
>
> obsync writes that sentence at the end of every remedy it prints, and it is
> true of every freeze in the design: gates are re-checked at the top of every
> run, and a frozen obsync keeps ticking, re-evaluating, and doing nothing else.
> Repair the cause and obsync starts syncing again within a tick, with no
> restart and nothing to clear by hand. Never cut this line.

**A restart destroys the diagnosis and repairs nothing.** obsync parks alive
rather than exiting precisely so that the diagnosis survives, and what it holds
is process-lifetime:

- **The remote's own words**, on a rejected push, live in the running process
  until the next hourly retry. A restarted obsync says nothing about the
  rejection until it has tried again and been refused again.
- **The network backoff and the backoff ceiling's clock** start over, so a
  remote that has been failing for a day reads as one that has just started.
- **The local failure streak** starts over, so a repository that was one run
  from being called damaged is five again.
- **"Attempted, never succeeded"** starts over: a restarted obsync has genuinely
  not attempted a push yet, which is not a failure and is not reported as one.

The one freeze a restart *cannot* clear is the write-verify freeze
[below](#a-tree-obsync-could-not-verify), and it is keyed on a ref rather than
on the process for exactly this reason.

---

## The three tiers

Every failure obsync produces is one of three things. The list is closed at
three: a condition added later gets a cadence and a health input, never a fourth
category.

| Tier | What stops | What you are told | Health | What clears it |
|---|---|---|---|---|
| **Aborted run** | This pass, and nothing else | Nothing above debug | Healthy | The next wake-up, ~60s later |
| **Network freeze** | The network half. The vault goes on being committed locally | The freeze, its fact and its remedy — once on entry, then once an hour | Unhealthy at once | The fact ceasing to be true |
| **Full freeze** | Everything — obsync stops touching the repository at all | The same | Unhealthy at once | The fact ceasing to be true |

**An aborted run is not news.** obsync lost a race — someone else held the index
lock, a file moved while it was being staged, an incoming change touched a path
the vault was writing — and the next run starts fresh. It is silent by design,
because making a transient loss news is how the signal becomes noise. What it is
not is unrecorded: consecutive aborted runs in the local half are what the local
failure streak counts.

**A network freeze means the vault is sound and its relationship to the remote
is not.** obsync keeps committing the vault locally throughout, so nothing you
write is at risk while one stands, and there is no cap on how far the local
branch runs ahead of the remote. Nothing leaves or arrives until it clears.

**A full freeze means committing would itself be wrong**, so obsync stops
entirely and stays alive. It writes the attention note anyway — that touches the
vault rather than the repository — except where it has nothing to write it
through: a vault it cannot write in at all, a non-empty directory it refused to
adopt rather than writing a file into, and the cases where there is no
repository yet, since every file obsync writes is renamed into place out of a
staging directory inside `.git`. In those, the log is the only channel.

**Where two are live at once, the full freeze wins.**

### What a frozen obsync is doing

Ticking. Every wake-up re-asks the whole question and does nothing else with the
answer, so the freeze that started when you were asleep clears the minute after
you fix it. On top of that:

- **Anything needing a human is repeated once an hour**, never once a tick —
  which is what makes `docker logs --since 1h` empty exactly when nothing is
  wrong and never empty when something is.
- **A push the remote rejected is retried once an hour**, and the retry is a
  whole network half, so other people's changes still arrive while it stands.
- **A remote that is merely unreachable** is retried on the ordinary backoff,
  60s doubling to a ceiling of 15 minutes, and obsync keeps retrying past the
  point where it stops calling itself healthy.
- **A damaged repository** is retried differently, and that is the one exception
  worth knowing: see [below](#a-damaged-repository).

---

## The four things obsync cannot tell you itself

Everything else is in the note. These four are not derivable from live state, so
they are here.

### A tree obsync could not verify

After obsync applies an incoming change to the vault it checks that the vault
holds the tree it computed. If it does not, obsync has stopped being able to
trust its own view of the vault, so it stops — and before it does, it writes the
tree it *meant* to apply to `refs/obsync/failed-apply`, because that commit is a
real object that nothing else points at and a later `gc` would prune the one
artifact that explains the mess.

**This is the one freeze a restart cannot clear, and the one you clear
yourself.** The ref is itself the gate: while it exists obsync stops touching
the repository, and it keeps checking, so deleting the ref is what releases
it.

```bash
# What obsync meant the vault to hold, against what it holds.
git -C /path/to/vault diff refs/obsync/failed-apply

# Recover whatever you need from it, by path.
git -C /path/to/vault show refs/obsync/failed-apply:'Daily/2026-08-24.md'

# Then, deliberately, once the tree is what you want it to be:
git -C /path/to/vault update-ref -d refs/obsync/failed-apply
```

obsync attempts no repair of its own here and never will: a tool that has just
proved it cannot apply a tree correctly is the last thing that should try again
unsupervised. The ref is never pushed.

### A damaged repository

A repository obsync can no longer read — a corrupt or empty object, a truncated
index, a ref pointing at nothing — is not something a gate can catch, because it
is never a cheap conclusive fact: it is something a command runs into. obsync
finds it by working. Every sync run already reads exactly the objects obsync
depends on, and no scan of the whole repository improves on that, which is why
obsync never runs `git fsck`.

git's exit status cannot tell a rotted object from a locked index, so **time is
the classifier**: five consecutive sync runs whose local half failed. At five,
obsync deletes `.git/index` and tries once more — the index is derived state,
reconstructible from the working tree, and **obsync may discard derived state
and never discards history**. The cost is stated rather than hidden: a human's
staged-but-uncommitted work is dropped. The files are untouched, and the next
run would have committed them anyway.

If that run fails too, obsync freezes and tells you the failing git command,
git's own first line of stderr, the streak count, and how much room is left on
the disk when there is almost none.

**This freeze is the one that self-clears by retrying rather than by
re-checking.** While it stands obsync runs one read-only `git status` a tick and
nothing else; the tick that succeeds releases it.

> **Load-bearing documentation** (§7, [#15](../../issues/15)).
> **The recovery recipe.** obsync never re-clones and never repairs a repository
> by replacing it, because a re-clone discards exactly the commits obsync exists
> to have made — and obsync cannot tell whether a damaged object is one the
> remote already holds or one only this disk ever had. This recipe is what
> replaces that code. Never cut it.
>
> ```bash
> # 1. Stop obsync, so that nothing is turning while you move things. The
> #    diagnosis is already in the note; read it before you start.
> docker compose stop obsync
>
> # 2. Keep the old .git. The commits obsync had not pushed yet are in it,
> #    and they may be the only copy that has ever existed.
> mv /path/to/vault/.git /path/to/vault/../vault-git-damaged
>
> # 3. Clone the remote beside the vault, and move its .git into place. Do
> #    this as the UID in the compose file's `user:` line, or chown it after.
> git clone --branch <the tracked branch> https://host/owner/vault.git /tmp/vault-fresh
> mv /tmp/vault-fresh/.git /path/to/vault/.git
> rm -rf /tmp/vault-fresh
>
> # 4. Restore what the vault never received. The remote may have moved on
> #    while obsync was frozen, and nothing brought those files down — so
> #    git now reports them as deleted, and obsync's first run would commit
> #    that deletion and push it. Read the list before you act on it.
> git -C /path/to/vault ls-files --deleted
> git -C /path/to/vault ls-files -z --deleted | xargs -0 -r git -C /path/to/vault restore --
>
> # 5. Look at what the old repository still has, before you throw it away.
> #    -o keeps the patches out of the vault; without it they land in the
> #    working directory, and obsync would commit and push them.
> git --git-dir=/path/to/vault-git-damaged log --oneline <branch>
> git --git-dir=/path/to/vault-git-damaged format-patch -o /path/to/vault-git-damaged-patches \
>     origin/<branch>..<branch>
>
> # 6. Start obsync. It un-freezes on its own once `git status` succeeds —
> #    no restart is needed for the freeze, and this one is a restart only
> #    because you stopped it in step 1.
> docker compose start obsync
> ```
>
> **Step 4 is the one that is easy to skip and expensive to skip.** The vault is
> the side obsync treats as true, and the repository you have just attached says
> the remote holds files the vault does not — so without it obsync does exactly
> what it is built to do and publishes the deletion of every note another device
> pushed while this one was frozen. Restoring is the safe direction and deleting
> again is cheap: the one thing step 4 can get wrong is resurrecting a note you
> deliberately deleted while obsync was down, and you delete it again in
> Obsidian.
>
> Three things about the state you are looking at, all easy to misread:
>
> - **Before step 3, obsync has already deleted `.git/index`**, and it builds one
>   back from HEAD before it commits anything. A `git status` reporting every
>   file in the vault as deleted is a missing index, not a lost vault.
> - **After step 3 that is no longer the reading.** The repository you attached
>   has an index of its own, so a file reported deleted there is a real
>   difference between the remote and the vault, and step 4 is what answers it.
> - **Your notes were never touched.** Everything in the working tree is
>   yours and is exactly as you left it. What is at risk is only what had been
>   committed and not yet pushed, which is what step 2 keeps.
>
> The repository you reattached is at the remote's tip and the vault holds your
> files, so obsync's first run after step 6 commits the difference between them
> and pushes it. That is the ordinary loop, not a repair — which is exactly why
> the difference has to be one you meant.

### A push the remote rejected

The remote received the push, evaluated it, and said no — a pre-receive hook, a
branch protection rule, a storage quota, a pack over a limit. It is a verdict
rather than a failure, so obsync stops the network half on the first occurrence
rather than waiting to see whether it repeats, and reports it immediately.

> **Load-bearing documentation** (§7, [#18](../../issues/18)).
> **The recipe is entirely yours, and obsync relays, never diagnoses.**
> obsync prints the remote's own words verbatim, in a fenced block, labelled as
> the remote's rather than obsync's, and adds nothing to them. It never guesses
> at which file or which rule is the problem, because git ships the remote's
> reason as *"a human-readable explanation"* and nothing else — there is no
> machine field naming a path, and a guess printed beside a fact reads as a
> diagnosis. Never cut this line, and never replace the relayed text with an
> interpretation of it.
>
> 1. **Look at the remote, not at the vault.** There is nothing wrong here to
>    find. obsync's local half is still committing and the vault is intact.
> 2. **Read the remote's words in the attention note** — the fenced block under
>    the freeze. They are what the remote said, exactly as it arrived.
> 3. **Change whatever the remote objects to, on the remote.** Relax the hook,
>    adjust the branch protection rule, raise the quota, widen the credential's
>    scope.
> 4. **Wait.** obsync retries the whole network half once an hour and clears the
>    freeze when a push lands. Nothing else is needed.
>
> **obsync offers no way past the stuck commit**, and imposes no cap on how far
> the local branch runs ahead meanwhile. It will not rewind the commit the
> remote refused: a subcommand that drops a commit is the self-repair this
> design does not do, and the rejected commit may be the only copy of the work
> in it. If the offending content genuinely has to go, that is your own `git`
> in the vault, and obsync syncs the result like any other edit.

The pack obsync uploads grows while this stands, because the hourly retries keep
merging and committing. That is the fail-open-locally rule working rather than a
defect.

### Health and your runtime

**Docker Swarm acts on health status. Plain Compose ignores it.** The same image
and the same `HEALTHCHECK` therefore behave differently depending on what you
run them under:

- **Under plain `docker compose`**, an unhealthy obsync shows as `unhealthy` in
  `docker ps` and nothing else happens. Nothing restarts it, which is what
  obsync's design expects: a parked obsync holding a diagnosis is the point.
- **Under Swarm**, an unhealthy task is replaced. A container restarted for
  being unhealthy loses everything in the list [above](#do-not-restart-it) —
  and because a freeze is reported as unhealthy, an unattended restart can turn
  a diagnosable stuck state into a loop that keeps destroying its own diagnosis.
  The write-verify freeze is the one that survives it, deliberately.

If you run obsync under Swarm, decide deliberately what you want an unhealthy
task to do before you deploy it. A `restart_policy` of `condition: none` stops
the *replacement*; it does not stop Swarm acting on the health status in the
first place, so do not plan on the parked process still being there to ask.

Plan on the ref instead. Everything in the list [above](#do-not-restart-it) is
process state and is genuinely gone, and the attention note is derived from
process state — so a note holding a rejection's relayed words is rewritten
without them until obsync has been refused again. The one thing keyed on
something a restart cannot reach is `refs/obsync/failed-apply`, which is why the
freeze that matters most is the freeze that is keyed on a ref.

---

## When the disk fills up

obsync never checks free space before it writes, and there is no threshold to
set. It reads free space only once a local git has already failed, and then only
to tell you how much is left.

- **A full disk does not corrupt a repository.** git writes objects, packs and
  the index through a temp file and a rename, so running out of room *aborts
  commands* rather than leaving half an object behind.
- **What it does instead is fail runs**, and consecutive failed runs are exactly
  what the streak counts — so a disk that stays full eventually reads as a
  damaged repository, and says so with the free space in the same line. Free
  room, and the freeze clears on the tick after `git status` succeeds. There is
  nothing else to do and nothing to repair.
- **Plan headroom for the attachments, not for the notes.** When both sides
  changed one file, obsync keeps both: the remote pays nothing, because the
  copy's bytes are a blob it already holds, and the **vault volume carries the
  file twice** until you resolve the pair and delete the copy. For notes that is
  kilobytes; for a 90MB video edited in two places it is 90MB, once per such
  file in the merge. The measurements are in
  [`research/sizing.md`](research/sizing.md), and the README's **Sizing**
  section states what they mean for a deployment.

---

## When something else is writing the vault

obsync is built for a vault that has another writer in it — that is the ordinary
case, and it is why every run asks git what changed rather than trusting a file
watcher, and why a file that is still being written is left out of *this* commit
rather than committed in half.

What a **third writer** obsync cannot see looks like, if one appears:

- **Conflict copies with no explanation.** Both sides of a note changed, and one
  of the sides was not you.
- **Runs that abort and retry**, visible only at debug: an incoming change
  touched a path something else was writing, so obsync recomputed rather than
  applying over it.
- **A path in the note's last section** — something is rewriting it faster than
  obsync can ever see it settle, so it is never committed at all.

The commonest cause is **Obsidian's own Headless Sync running beside obsync**.
obsync cannot detect it and cannot coordinate with it; see the README's fit
section, which is where the decision belongs, because it decides whether obsync
is for you at all rather than what to do at 3am.

---

## Removing obsync

Stop the container and delete it. Everything obsync wrote lives in namespaces it
declared — `obsync-attention.md`, conflict copies, `.git/info/exclude`,
`.git/obsync/`, `.git/obsync.lock` and `refs/obsync/failed-apply` — so removal
is a deletion rather than an untangling. The vault is a git repository with your
notes in it and your remote configured, which is what it was before. The full
list is [`interface.md`](interface.md#4-what-obsync-writes-into-the-vault).
