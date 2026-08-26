# The declared surface

Everything obsync's version number makes a promise about: the **config
surface**, the subcommands, the health contract, and what obsync writes into
the vault. It is strictly larger than the config surface, because a daemon with
no library API still has a public interface — just not one a compiler can see.

**This page is the canonical statement of that surface.** It was written from
the design spec's §10 ([#21](../../issues/21), comment 1), which is superseded
by it. There is one statement rather than two, and where the two disagree this
page is the later word.

## Versioning

obsync is versioned with SemVer **over this page**. A release that moves
anything stated here is a **surface change**, and its release notes say so in a
"Surface changes" section that is present and empty when nothing moved.

**obsync is 0.x through implementation, and reaches 1.0.0 once it has run a
real vault sustained.** The surface below is offered as the intended contract
from the first 0.x release and is **explicitly not yet warranted**: first
contact moves something, and 0.x is where it is allowed to.

The version identifies the build. `obsync status` reports it, and it is stamped
at link time rather than derived at runtime.

---

## 1. The config surface

**Nine environment variables, one required.** Configuration is environment only,
`OBSYNC_` prefixed, one vault per container. There are no flags and no config
file. A tenth variable is a change to this page rather than an implementation
detail.

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
the default, not to the empty string.

### The repo, and the remote

`OBSYNC_REPO` is the only value nothing can infer, and it is the only variable
obsync will not start without. The accepted forms are:

```
https://host/owner/vault.git
http://host/owner/vault.git
ssh://git@host/owner/vault.git
git@host:owner/vault.git          # scp-style
file:///srv/vaults/vault.git
```

Anything else is a config error. A URL must name a host — `file://` excepted —
and a repository path.

- **Plain `http://` earns a startup WARN and is never refused.** The credential
  and the vault cross the network in the clear; obsync syncs it anyway.
- **`file://` is supported, not merely tolerated.** obsync's own tests reach a
  local bare remote over it, and a form obsync tests is a form obsync supports.
- **The remote name is not a knob.** It is `origin`, always. `OBSYNC_REMOTE`
  gets the ordinary unknown-variable WARN.
- **One repo, one branch, one remote per container.** Two vaults is two compose
  services.

obsync compares `OBSYNC_REPO` against the vault's own `origin` on every sync
run, normalised to a **host and path** pair: scheme, embedded credentials, a
default port, host case, a `.git` suffix and a trailing slash are discarded, and
scp-style `git@host:owner/vault` equals `ssh://git@host/owner/vault`. So an
operator may swap a PAT for a deploy key without obsync noticing, and a
differing host or path is a **full freeze** — obsync stops touching the repo,
stays alive, and keeps re-checking. **obsync never runs `git remote set-url`**
and never adopts a remote it was not pointed at.

### The credential

`OBSYNC_TOKEN_FILE` is required if and only if the repo URL is `http://` or
`https://`. `ssh://` and scp-style take an ordinary mounted key and `known_hosts`
— **SSH needs no knobs**, and obsync stores and manages neither. `file://` needs
nothing.

**A file is the only supported form for the secret.** It is re-read every time
git asks for a credential, which is what makes a rotated token recover with no
restart. `_FILE` is token-only and not a general suffix convention.

The token never appears in a URL, in an argv, in a log line, or in
`git remote -v`. `OBSYNC_USERNAME` is what the remote wants beside it: GitHub
takes any non-empty username with a PAT, GitLab needs `oauth2`, Gitea the real
one. Minimum scopes per remote are in `docs/credentials.md`.

### Sizes

`OBSYNC_SIZE_CEILING` takes a human suffix — `B`, `KB`, `MB`, `GB` — and never
raw bytes: `104857600` is refused, `100MB` is not. The multipliers are powers of
1024, which is what git's own config sizes mean, so a number copied out of a git
setting means the same here.

The ceiling is the largest single file obsync will commit, and the largest blob
a merge may invent. It is **prevention, not a guarantee**: obsync can never
discover a remote's real limit, so lowering it reduces how often a remote
rejects a push and never stops one.

**Conflict copies are exempt from the ceiling at any size.** Their bytes are the
remote's losing version of a path, which the remote already holds.

### Names obsync does not recognise, and names it used to

- **An unknown `OBSYNC_*` variable WARNs and never exits.** obsync says which
  name it did not recognise and runs on the defaults it resolved, so a newer
  compose file never brings down an older image.
- **A retired name is recognised, not ignored.** A variable this page once
  carried produces a startup ERROR naming its replacement, checked *before* the
  unknown-name sweep, so an upgrade past a rename can never silently revert a
  setting to its default. Retired names stay recognised for good.

obsync has retired nothing. The first retirement is a surface change, and it
appears here.

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

Everything else — a vault that is not there, a remote that is not reachable, a
credential the remote refuses — is a gate or a runtime failure. obsync parks
alive and keeps telling you why. **A credential file turning unreadable *later*
is not a config error**, it is the self-healing bad-credential path.

All the problems in one environment block are reported, not the first, one
logged line each: an operator who finds them one at a time pays a redeploy for
each.

### The startup line

obsync logs one INFO line naming every knob it resolved — defaulted or set —
with the **token absent entirely**. Not redacted, not its first four
characters: absent, so there is no prefix to leak and no "is that the real
value?" question. A credential an operator embedded in `OBSYNC_REPO` gets the
same treatment.

The keys are the variable names, lowercased and without the `OBSYNC_` prefix,
plus one:

| Key | Carries |
|---|---|
| `repo` | `OBSYNC_REPO`, as it was set, with any embedded credential dropped |
| `remote` | The normalised host and path obsync will compare `origin` against |
| `vault_path` | `OBSYNC_VAULT_PATH` |
| `branch` | `OBSYNC_BRANCH`, empty when the tracked branch is resolved at startup |
| `token_file` | `OBSYNC_TOKEN_FILE` — the path, never the secret |
| `username` | `OBSYNC_USERNAME` |
| `size_ceiling` | `OBSYNC_SIZE_CEILING`, in canonical form |
| `author_name` | `OBSYNC_AUTHOR_NAME` |
| `author_email` | `OBSYNC_AUTHOR_EMAIL` |
| `log_level` | `OBSYNC_LOG_LEVEL` |

The keys are on the surface, and they are named after the variables rather than
after this project's glossary — `token_file`, not *credential file* — because
the line exists to be diffed against a compose file. It is this page's runtime
counterpart: what obsync says it is running, against what this page says exists.

---

## 2. The subcommands

A closed list of four. There are no flags on any of them.

| Subcommand | Contract |
|---|---|
| _(default)_ | Run the sync loop until SIGTERM |
| `healthcheck` | Silent; exit 0 healthy, 1 unhealthy. What the `HEALTHCHECK` calls |
| `status` | Human-readable report to stdout; exit 0 always; includes the build version |
| `credential-helper` | git's credential-helper protocol. Not documented for human use, but on the surface because git's invocation of it is |

**The default subcommand exits for two reasons only**: non-zero on a config
error, and 0 on SIGTERM. **It never exits on a sync failure** — a failed run is a diagnosable
stuck state, and exiting would turn it into a crash loop that discards its own
backoff.

On SIGTERM obsync refuses to start a new run, finishes the current one, and
exits, with a hard deadline of about 30 seconds. The reference `compose.yaml`
therefore sets a `stop_grace_period` longer than Docker's 10s default.

**stdout belongs to the subcommands.** obsync's log is logfmt on stderr, one
format, no knob.

---

## 3. The health contract

A Docker `HEALTHCHECK` is baked into the image and calls `obsync healthcheck`,
which reads a private status file under `.git/obsync/`. **There is no HTTP
server, no port and no health knob**: the subcommand is the whole mechanism, and
the single static binary means the image needs no `curl` for it.

```dockerfile
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=2 \
  CMD ["obsync", "healthcheck"]
```

**Health answers exactly one question: *does this need a human?*** — not "is
everything working".

| Verdict | States |
|---|---|
| **Unhealthy** | Any full freeze · any network freeze, including a remote rejection, immediately · a push attempted but never once succeeded · a merely-unreachable remote past the backoff ceiling of **24h** · a status file staler than 5 ticks (300s) |
| **Healthy** | Everything else — including an unreachable remote inside 24h, an aborted run, and any amount of backoff |

A **full freeze** stops obsync touching the repo at all; a **network freeze**
stops the network half while the vault keeps being committed locally; an
**aborted run** is a pass that gave up and will be retried. What each one means
and what clears it is `docs/operations.md`.

Both lists are closed. Two entries in them are opposites, and both are
deliberate: **a remote that is merely unreachable is healthy for a day, and a
remote that has *rejected* a push is unhealthy at once.** Waiting fixes the
first and cannot fix the second.

**The 24h backoff ceiling is not a retry limit.** obsync keeps backing off and
retrying past it; only the health verdict changes. Nothing about the failure
escalates it, because only elapsed time separates a remote that will come back
from one that will not.

**A conflict is never unhealthy.** Under obsync's keep-both rule a conflict is
normal operation.

### Never pushed is three states, not two

The status file carries the last successful push as a timestamp-or-never, and
the three cases are distinct:

| State | Meaning | Reported as |
|---|---|---|
| **Never attempted** | Nothing has changed yet | Not a failure, and never reported as one |
| **Attempted, never succeeded** | Nobody has ever seen this deployment work | ERROR immediately, unhealthy at once |
| **Succeeded, now failing** | It worked, and has stopped | Ordinary backoff, repeated hourly |

The middle row is how a wrong-scoped token is found: it fetches happily and
fails only at the first real push.

**The status file itself is private.** The subcommands are the interface; its
layout is not, and nothing should parse it. What a runtime does with the health
status — Swarm acts on it, plain Compose ignores it — is in
`docs/operations.md`.

---

## 4. What obsync writes into the vault

**obsync's owned paths.** Everything obsync writes lives in a namespace it
declared here.

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
longer than one run, and it is **deleted rather than emptied** when there is
nothing left to say. Its presence alone is the signal.

**Conflict copy names** are UTC at minute precision, and avoid every character
Obsidian forbids. A collision appends a counter. **An existing copy is never
overwritten** — that is the one way this design could lose bytes.

### The ignore floor

Written to the repo's `.git/info/exclude` at every startup, wholesale, under a
marker comment. Never committed, so it cannot conflict and cannot be clobbered
by an external push.

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

**The floor is a default, not a rule.** git reads `.gitignore` before
`info/exclude`, so a vault `.gitignore` containing `!.obsidian/workspace.json`
overrides the floor and the file is tracked again. **obsync never writes the
vault's `.gitignore`** — that file is content, and it is the user's.

**One exception, and it cannot be overridden**:
`.obsidian/plugins/*/data.json` is also excluded by pathspec on the `git add`
itself, which no `.gitignore` can negate. Plugin settings are where community
plugins keep API keys.

Everything else under `.obsidian/` is tracked — `app.json`, `appearance.json`,
`core-plugins.json`, `community-plugins.json`, and each plugin's `main.js`,
`manifest.json` and `styles.css` — so a fresh clone arrives as the same vault
rather than a folder of markdown. `.trash/` is excluded, with the consequence
stated rather than hidden: **git history is the undelete mechanism.**

A vault whose history *already* contains the churn the floor excludes would
churn forever, so obsync untracks that subset once — `workspace*.json`,
`workspaces.json`, `.trash/` and the cruft entries — in a single loudly-messaged
commit, leaving the files on disk. That commit deletes those paths from every
other clone on its next pull, which for `workspace.json` is the point, and is
why the floor stays narrow. **Already-tracked `data.json` is left alone**:
untracking it would delete deliberately-synced plugin settings from every other
clone and would not unleak a key the remote's history already holds. obsync
logs loudly at startup when it finds one.

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

**Name-matching only — there is no content scanning, ever**, and there is no
way to switch a refusal off. The list stays short instead: someone who
genuinely wants a `.pem` in their vault renames the file.

**A refusal skips the path and never stops the loop.** Everything else in the
vault keeps syncing. The consequence is stated plainly: while a tracked path is
refused, the remote holds the last version that passed and the vault holds a
newer one — stale, consistent, and named in the attention note.

> **This page treats the ignore floor's contents and the refused-path list as
> part of the surface**, because changing either silently changes what a user's
> repo contains. Adding an entry to the floor is a surface change; removing one
> is a surface change; so is either edit to the refused list.

### The counterpart claim

**obsync never writes a file the human owns.**

- `.git/config` holds their identity and their remote. obsync only ever *reads*
  it — that read is the check that `origin` is still where obsync was pointed.
- The vault's `.gitignore` is theirs alone, and outranks obsync's floor.
- Their notes are theirs.

The one place obsync writes outside its owned paths is the one an operator asked
for: **applying the remote's commits to tracked files**, which is the sync
itself. It never invents content there — the tree comes from git — and where
both sides changed a path, the vault's version keeps the path and the remote's
becomes a conflict copy beside it. If the vault is being written at a path the
incoming change touches, the run is abandoned and recomputed rather than forced.

**"Leaves no trace" would be false, and this is stronger, because it is
checkable**: obsync leaves plenty, all of it in namespaces named above.
Removing obsync is a deletion, not an untangling.

The full list of things obsync will never do is the README's never-list.

---

## Not on the surface

Stated so that nothing here is mistaken for a promise:

- **The status file's name and layout.** `.git/obsync/` is an owned path above,
  so where obsync writes is declared; what it writes inside it is private, and
  `obsync status` and `obsync healthcheck` are its interface.
- **The log's individual messages.** The format — logfmt on stderr — has no knob
  and the startup line's keys are above; the wording of everything else is not
  a promise.
- **obsync's timing constants**, except the ones stated above. The quiet
  window, the max-wait cap, the tick, the push floor, the backoff floor, the
  conflict-storm ceiling and the settle interval are constants rather than
  knobs, and moving one is not a surface change. The ones stated above are on
  the surface: the `HEALTHCHECK` directive's four parameters, the health
  contract's 300s staleness window and 24h backoff ceiling, and the ~30s
  deadline on a SIGTERM, which is what the reference `compose.yaml`'s
  `stop_grace_period` is set against.
- **A bare binary.** The container image is the supported artifact. A binary is
  a non-goal rather than a prohibition: nothing will make one impossible, and
  nothing is promised about one.
