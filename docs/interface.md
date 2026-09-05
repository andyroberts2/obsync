# The declared surface

Everything obsync's version number makes a promise about: the **config
surface**, the subcommands, the health contract, and what obsync writes into the
vault. A daemon with no library API still has a public interface, and this page
is it.

## Versioning

obsync is versioned with SemVer **over this page**. A release that moves
anything stated here is a **surface change**, and its release notes say so in a
"Surface changes" section. That section is present in every release, and empty
only when nothing moved.

**obsync is 0.x until it has run a real vault sustained, and then it reaches
1.0.0.** The surface below is the intended contract from the first 0.x release,
and it is **not yet warranted**. First contact moves something, and 0.x is where
that is allowed.

The version identifies the build. `obsync status` reports it, and it is stamped
at link time rather than worked out at runtime.

### What a release publishes

One pushed **annotated tag**, `vMAJOR.MINOR.PATCH`, cuts a release. There is no
schedule and no button. The tag's message carries the "Surface changes" section,
and a release whose tag moved this page with that section empty is refused
rather than published.

The image is published to GHCR and to no other registry:

```
ghcr.io/andyroberts2/obsync
```

One build is pushed under four names. For `v1.4.2` those are `1.4.2`, `1.4`, `1`
and `latest`. They are the same image: one digest, with one attestation and one
SBOM beside it.

**Pin the floating major.** It is the name that follows a base image patch, and
an unattended sidecar on a pin that never moves ends up on a stale base. Before
1.0 there is no floating major, so [`compose.yaml`](../compose.yaml) pins `0.4`
today and changes to `1` at 1.0. It moves with every release: the suite asserts
the reference stack pins the newest release's floating name, and fails until it
does. `latest` is published because people expect it to exist. Nothing here
points at it, and nothing you run unattended needs to.

**A floating name only ever moves forward.** A release publishes the floating
names it is the newest under, and no others. A backport cut from an older line
takes `1.3.5` and `1.3`, and leaves `1` and `latest` on the newer build. What
you pinned never becomes older code.

**A base image bump is a `patch` release.** The base and the builder are pinned
by digest rather than by tag, because Alpine moves a release tag on every patch
and git moves with it. A CVE fix in the base therefore arrives as a version of
obsync, with notes, like everything else on this page.

Every release carries a build-provenance attestation and an SBOM:

```bash
gh attestation verify oci://ghcr.io/andyroberts2/obsync:0.4 --owner andyroberts2
docker buildx imagetools inspect ghcr.io/andyroberts2/obsync:0.4
```

---

## 1. The config surface

**Nine environment variables, one required.** Configuration is environment only,
`OBSYNC_` prefixed, one vault per container. There are no flags and no config
file. A tenth variable is a change to this page.

| Variable | Required | Default | Accepted form |
|---|---|---|---|
| `OBSYNC_REPO` | **yes** | — | One of the repo forms below |
| `OBSYNC_VAULT_PATH` | no | `/vault` | Absolute path to the vault directory |
| `OBSYNC_BRANCH` | no | resolved at startup | A branch name |
| `OBSYNC_TOKEN_FILE` | iff `http(s)://` | — | Path to a readable regular file |
| `OBSYNC_USERNAME` | no | `obsync` | A username |
| `OBSYNC_SIZE_CEILING` | no | `95MB` | A whole number and a unit |
| `OBSYNC_AUTHOR_NAME` | no | `obsync` | A git author name |
| `OBSYNC_AUTHOR_EMAIL` | no | `obsync@obsync.invalid` | A git author address |
| `OBSYNC_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |

**An empty value means unset.** `OBSYNC_USERNAME=` in a compose file resolves to
the default rather than to the empty string.

### The repo, and the remote

`OBSYNC_REPO` is the only value nothing can infer, and the only variable obsync
will not start without. The accepted forms are:

```
https://host/owner/vault.git
http://host/owner/vault.git
ssh://git@host/owner/vault.git
git@host:owner/vault.git          # scp-style
file:///srv/vaults/vault.git
```

Anything else is a config error. A URL must name a host and a repository path.
`file://` needs no host.

- **Plain `http://` gets a startup WARN and is never refused.** The credential
  and the vault cross the network in the clear, and obsync syncs it anyway.
- **`file://` is supported rather than merely tolerated.** obsync's own tests
  reach a local bare remote over it.
- **The remote name is not a knob.** It is `origin`, always. `OBSYNC_REMOTE`
  gets the ordinary unknown-variable WARN.
- **One repo, one branch, one remote per container.** Two vaults means two
  compose services.

On every sync run obsync compares `OBSYNC_REPO` against the vault's own
`origin`, normalised to a **host and path** pair. The scheme, embedded
credentials, a default port, host case, a `.git` suffix and a trailing slash are
all discarded, and `git@host:owner/vault` equals `ssh://git@host/owner/vault`.
So you can swap a PAT for a deploy key without obsync noticing. A differing host
or path is a **full freeze**: obsync stops touching the repository, stays alive,
and keeps re-checking. **obsync never runs `git remote set-url`** and never
adopts a remote it was not pointed at.

### The credential

`OBSYNC_TOKEN_FILE` is required if and only if the repo URL is `http://` or
`https://`. `ssh://` and scp-style take an ordinary mounted key and
`known_hosts`, and obsync stores and manages neither. `file://` needs nothing.

An SSH remote needs those files where ssh looks for them, which is **`$HOME` in
the image, `/home/obsync`**. It also needs one more ordinary mount, because ssh
expands `~` out of the UID's `/etc/passwd` entry rather than out of `HOME`, and
the image bakes no entry for any UID. [`credentials.md`](credentials.md) carries
the whole instruction.

**A file is the only supported form for the secret.** obsync re-reads it every
time git asks for a credential, so a rotated token recovers with no restart.
`_FILE` is token-only rather than a general suffix convention.

The token never appears in a URL, in an argv, in a log line, or in
`git remote -v`. `OBSYNC_USERNAME` is what the remote wants beside it: GitHub
takes any non-empty username with a PAT, GitLab needs `oauth2`, and Gitea needs
the real one. Minimum scopes per remote are in
[`credentials.md`](credentials.md).

### The identity obsync runs as

**obsync runs as whatever UID and GID Docker's own `user:` line names**, and it
must be the pair that owns the vault. There is no `PUID`, no `PGID` and no root
entrypoint. obsync's git identity comes from its own private config rather than
from a passwd file, so it needs no `/etc/passwd` entry, and the image bakes
none.

**The image's default is `USER 1000:1000`** — not root, so a compose file with
no `user:` line does not run a container holding a write-scoped credential as
UID 0. **`HOME` is `/home/obsync`**, and obsync writes nothing there. It is
where an SSH key and `known_hosts` are mounted, and nothing else.

A wrong UID is not a silent corruption. A vault obsync's UID cannot write in is
a **full freeze** with a named cause, and obsync parks alive saying so.

### Sizes

`OBSYNC_SIZE_CEILING` takes a suffix — `B`, `KB`, `MB`, `GB` — and never raw
bytes. `104857600` is refused and `100MB` is not. The multipliers are powers of
1024, which is what git's own config sizes mean, so a number copied out of a git
setting means the same here.

The ceiling is the largest single file obsync will commit, and the largest blob
a merge may invent. It is **prevention rather than a guarantee**: obsync can
never discover a remote's real limit, so lowering the ceiling reduces how often
a remote rejects a push and never stops one.

**Conflict copies are exempt from the ceiling at any size.** Their bytes are the
losing version of a path, which the remote already holds — or which obsync
already committed.

### Names obsync does not recognise, and names it used to

- **An unknown `OBSYNC_*` variable WARNs and never exits.** obsync says which
  name it did not recognise and runs on the defaults it resolved, so a newer
  compose file never brings down an older image.
- **A retired name is recognised rather than ignored.** A variable this page
  once carried produces a startup ERROR naming its replacement. obsync checks
  for retired names *before* the unknown-name sweep, so an upgrade past a rename
  cannot silently revert a setting to its default. Retired names stay recognised
  for good.

obsync has retired nothing so far. The first retirement is a surface change, and
it appears here.

### Where obsync exits, and where it parks

> **If the check needs a syscall on the vault or a network round trip, it is a
> gate and obsync parks alive. If it can be decided from the environment block
> alone, it is a config error and obsync exits 1.**

The config errors are a closed list of six:

1. `OBSYNC_REPO` missing.
2. A repo URL that is not a URL, or names no host or no repository path.
3. A repo URL in a scheme obsync does not accept.
4. A size that carries no unit, or is not a whole number of one.
5. A log level that is not one of the four.
6. `OBSYNC_TOKEN_FILE` set to something obsync cannot read at startup, or to
   something that is not a regular file.

Everything else is a gate or a runtime failure: a vault that is not there, a
remote that is not reachable, a credential the remote refuses. obsync parks
alive and keeps telling you why. **A credential file turning unreadable later is
not a config error.** That is a token being rotated, and obsync recovers on its
own.

obsync reports every problem in an environment block rather than the first, one
logged line each. An operator who finds them one at a time pays a redeploy for
each.

### The startup line

obsync logs one INFO line naming every knob it resolved, set or defaulted, with
the **token absent entirely**. Not redacted, and not its first four characters:
absent, so there is no prefix to leak. A credential embedded in `OBSYNC_REPO`
gets the same treatment.

The keys are the variable names, lowercased and without the `OBSYNC_` prefix,
plus one:

| Key | Carries |
|---|---|
| `repo` | `OBSYNC_REPO`, as it was set, with any embedded credential dropped |
| `remote` | The normalised host and path obsync compares `origin` against |
| `vault_path` | `OBSYNC_VAULT_PATH` |
| `branch` | `OBSYNC_BRANCH`, empty when the tracked branch is resolved at startup |
| `token_file` | `OBSYNC_TOKEN_FILE` — the path, never the secret |
| `username` | `OBSYNC_USERNAME` |
| `size_ceiling` | `OBSYNC_SIZE_CEILING`, in canonical form |
| `author_name` | `OBSYNC_AUTHOR_NAME` |
| `author_email` | `OBSYNC_AUTHOR_EMAIL` |
| `log_level` | `OBSYNC_LOG_LEVEL` |

The keys are named after the variables rather than after this project's
glossary, because the line exists to be diffed against a compose file.

---

## 2. The subcommands

A closed list of four. There are no flags on any of them.

| Subcommand | Contract |
|---|---|
| _(default)_ | Run the sync loop until SIGTERM |
| `healthcheck` | Silent; exit 0 healthy, 1 unhealthy. What the `HEALTHCHECK` calls |
| `status` | Human-readable report to stdout; exit 0 always; includes the build version |
| `credential-helper` | git's credential-helper protocol. Not for human use, but on the surface because git's invocation of it is |

**The default subcommand exits for two reasons only**: non-zero on a config
error, and 0 on SIGTERM. **It never exits on a sync failure.** A failed run is a
diagnosable stuck state, and exiting turns it into a crash loop that discards
its own backoff.

On SIGTERM obsync refuses to start a new run, finishes the current one, and
exits, with a deadline of about 30 seconds. The reference
[`compose.yaml`](../compose.yaml) therefore sets a `stop_grace_period` longer
than Docker's 10s default.

**stdout belongs to the subcommands.** obsync's log is logfmt on stderr, one
format, no knob.

---

## 3. The health contract

A Docker `HEALTHCHECK` is baked into the image and calls `obsync healthcheck`,
which reads a private status file under `.git/obsync/`. **There is no HTTP
server, no port and no health knob.** The subcommand is the whole mechanism, and
the single static binary means the image needs no `curl` for it.

```dockerfile
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=2 \
  CMD ["obsync", "healthcheck"]
```

**Health answers exactly one question: *does this need a human?*** It does not
answer "is everything working".

| Verdict | States |
|---|---|
| **Unhealthy** | Any full freeze · any network freeze, including a remote rejection, immediately · a push attempted but never once succeeded · a merely-unreachable remote past the backoff ceiling of **24h** · a status file staler than 5 ticks (300s) |
| **Healthy** | Everything else — including an unreachable remote inside 24h, an aborted run, and any amount of backoff |

A **full freeze** stops obsync touching the repository at all. A **network
freeze** stops the network half while the vault keeps being committed locally.
An **aborted run** is a pass that gave up and will be retried.
[`operations.md`](operations.md) says what each one means and what clears it.

Both lists are closed. Two entries in them are opposites, and both are
deliberate: **a remote that is merely unreachable is healthy for a day, and a
remote that has *rejected* a push is unhealthy at once.** Waiting fixes the
first and cannot fix the second.

**The 24h backoff ceiling is not a retry limit.** obsync keeps backing off and
retrying past it, and only the health verdict changes.

**A conflict is never unhealthy.** Under obsync's keep-both rule a conflict is
normal operation.

### Never pushed is three states, not two

The status file carries the last successful push as a timestamp or as never, and
the three cases are distinct:

| State | Meaning | Reported as |
|---|---|---|
| **Never attempted** | Nothing has changed yet | Not a failure, and never reported as one |
| **Attempted, never succeeded** | Nobody has ever seen this deployment work | ERROR immediately, unhealthy at once |
| **Succeeded, now failing** | It worked, and has stopped | Ordinary backoff, repeated hourly |

The middle row is how a wrong-scoped token is found. Such a token fetches
happily and fails only at the first real push.

**The status file itself is private.** The subcommands are the interface, its
layout is not, and nothing else must parse it. What a runtime does with the
health status is in [`operations.md`](operations.md).

---

## 4. What obsync writes into the vault

Everything obsync writes lives in a namespace declared here.

| Path | Promise |
|---|---|
| `obsync-attention.md` (vault root) | Derived each run, four sections in a fixed order, deleted when empty, never tracked |
| Conflict copies | `Name (obsync conflict YYYY-MM-DD HHMM).ext`, beside the canonical path, byte-identical, committed, never overwritten |
| `.git/info/exclude` | Rewritten wholesale under a marker comment at every startup; the ignore floor's contents are part of the promise |
| `.git/obsync/` | The private status file, and `tmp/`, the staging directory every obsync write is renamed out of |
| `.git/obsync.lock` | The `flock` that keeps a second obsync off the vault |
| `refs/obsync/failed-apply` | Written before a write-verify freeze; never pushed; deleted by a human to clear the gate it holds |
| The refused-path list | What obsync declines to commit whatever its state |

**The attention note's four sections, in this order**: live freezes ·
outstanding conflict copies · refused paths · paths that have stayed unsettled
long enough to stop looking transient. Every section is derived from live state
rather than accumulated, so the note cannot drift from what it describes for
longer than one run. obsync **deletes** the note rather than emptying it when
there is nothing left to say, so its presence alone is the signal.

**Conflict copy names** are UTC at minute precision, and avoid every character
Obsidian forbids. A collision appends a counter. **An existing copy is never
overwritten** — that is the one way this design could lose bytes.

### The ignore floor

Written to the repository's `.git/info/exclude` at every startup, wholesale,
under a marker comment. It is never committed, so it cannot conflict and cannot
be clobbered by an external push.

```
.obsidian/workspace.json
.obsidian/workspace-mobile.json
.obsidian/workspaces.json
.obsidian/plugins/*/data.json
.trash/
.DS_Store
Thumbs.db
.vscode/
.idea/
.obsidian-git-data
obsync-attention.md
```

**The floor is a default rather than a rule.** git reads `.gitignore` before
`info/exclude`, so a vault `.gitignore` holding `!.obsidian/workspace.json`
overrides the floor and the file is tracked again. **obsync never writes the
vault's `.gitignore`** — that file is content, and it is yours.

**One exception cannot be overridden.** `.obsidian/plugins/*/data.json` is also
excluded by pathspec on the `git add` itself, which no `.gitignore` can negate.
Plugin settings are where community plugins keep API keys.

Everything else under `.obsidian/` is tracked — `app.json`, `appearance.json`,
`core-plugins.json`, `community-plugins.json`, and each plugin's `main.js`,
`manifest.json` and `styles.css` — so a fresh clone arrives as the same vault
rather than as a folder of markdown. `.trash/` is excluded, and the consequence
is stated rather than hidden: **git history is the undelete mechanism.**

A vault whose history *already* holds the churn the floor excludes would churn
forever. obsync untracks that subset once — `workspace*.json`, `workspaces.json`,
`.trash/` and the cruft entries — in a single loudly-messaged commit, and leaves
the files on disk. That commit deletes those paths from every other clone on its
next pull, which for `workspace.json` is the point.

**Already-tracked `data.json` is left alone.** Untracking it would delete
deliberately-synced plugin settings from every other clone, and would not unleak
a key the remote's history already holds. obsync logs loudly at startup when it
finds one.

### Refused paths

A closed list of filenames that never enter a commit, whatever the vault's
state:

```
.env, .env.*
id_rsa, id_dsa, id_ecdsa, id_ed25519
*.pem, *.key, *.p12, *.pfx
.netrc, .npmrc, .pypirc
credentials
```

plus **any single file over the size ceiling**.

**Name-matching only. There is no content scanning, ever**, and there is no way
to switch a refusal off. The list stays short instead: someone who genuinely
wants a `.pem` in their vault renames the file.

**Nothing else switches one off either.** Something else may put a refused path
in the index: a plugin that runs `git add`, or your own muscle memory. obsync
takes it back out before it commits, because `git commit` records the index
rather than what obsync staged. That is index-only, and your file's bytes are
never touched.

**A refusal skips the path and never stops the loop.** Everything else in the
vault keeps syncing. While a tracked path is refused, the remote holds the last
version that passed and the vault holds a newer one. That state is stale,
consistent, and named in the attention note.

> **This page treats the ignore floor's contents and the refused-path list as
> part of the surface**, because changing either silently changes what your
> repository holds. Adding an entry to either list is a surface change, and so
> is removing one.

### Bootstrap, and the one repo obsync creates

obsync decides once, at startup, what to do with the directory it was pointed
at:

| The directory | What obsync does |
|---|---|
| Already a git repo | Attaches to it, on the branch it is already on |
| Empty | Clones the remote into it, on the branch the remote calls default |
| Anything else | Refuses it, and keeps re-checking without exiting |

An empty directory pointed at a remote with **no branch to clone** is refused
too. That means a remote holding no refs at all, or one whose `HEAD` names a
branch it does not hold. obsync keeps re-checking, and either pushing a vault to
the remote or naming an existing branch with `OBSYNC_BRANCH` releases it with no
restart. Cloning an empty remote would leave a repository whose `HEAD` names no
commit, which obsync refuses on every later run.

The clone is the one time obsync creates a repository, and the only time
`.git/config` comes into existence under obsync rather than under you. git
writes it as part of creating the repo, with **one remote, named `origin`, and a
fetch refspec naming one branch and no tags**. obsync never writes that file
afterwards. A directory holding nothing but ignore-floor entries is not refused,
but git will not clone into a directory that holds anything at all, so obsync
names the entry in the way instead.

`OBSYNC_BRANCH` overrides the branch in the first two rows. On a vault that is
already a repo it may only agree with the branch the vault is on, because
**obsync never runs `git checkout` after bootstrap**. An override naming another
branch is refused, and the remedy is your own checkout.

**obsync creates the tracked branch on the remote only when the remote has no
refs at all.** A remote that has refs but not the tracked branch is a full
freeze. The branch name comes from local HEAD, so a stray branch or a typo would
otherwise create a remote branch, sync an entire vault into it, and succeed. When
a dedicated branch is what you want, the cost is one deliberate
`git push -u origin <branch>`.

### The counterpart claim

**obsync never writes a file you own.**

- `.git/config` holds your identity and your remote. obsync only ever *reads*
  it, and that read is the check that `origin` is still where obsync was
  pointed. The one exception is the repository obsync creates by cloning into an
  empty directory, where there was no file yet.
- The vault's `.gitignore` is yours alone, and outranks obsync's floor.
- Your notes are yours.

The one place obsync writes outside its owned paths is the one you asked for:
**applying the remote's commits to tracked files**, which is the sync itself. It
never invents content there, because the tree comes from git. Where both sides
changed a path, the vault's version keeps the path and the remote's becomes a
conflict copy beside it. If the vault is being written at a path the incoming
change touches, obsync abandons the run and recomputes rather than forcing it.

Removing obsync is a deletion rather than an untangling. Everything it leaves
behind is in the namespaces named above.

The full list of things obsync will never do is the README's never-list.

---

## Not on the surface

Stated so that nothing here is mistaken for a promise:

- **The status file's name and layout.** `.git/obsync/` is an owned path above,
  so where obsync writes is declared. What it writes inside is private, and
  `obsync status` and `obsync healthcheck` are its interface.
- **The log's individual messages.** The format — logfmt on stderr — has no knob
  and the startup line's keys are above. The wording of everything else is not a
  promise.
- **obsync's timing constants**, except the ones stated above. The quiet window,
  the max-wait cap, the tick, the push floor, the backoff floor, the
  conflict-storm ceiling and the settle interval are constants rather than knobs,
  and moving one is not a surface change. On the surface are the `HEALTHCHECK`
  directive's four parameters, the health contract's 300s staleness window and
  24h backoff ceiling, and the roughly 30s deadline on a SIGTERM.
- **A bare binary.** The container image is the supported artifact. A binary is
  a non-goal rather than a prohibition.
