# kubernetes/git-sync — Research Report for obsync

Source cloned at commit `cf98d8389384662e1b0d20389a6cf88246d303fe` (2026-07-28) to
`/tmp/claude-1000/-home-andy-code-obsync/3b655fa3-cf38-4da8-9581-1d14c7cc0c42/scratchpad/repos/git-sync`.
All line numbers below are file:line citations into that checkout (paths given relative to the repo root).

---

## Verdict

**Do not fork or vendor git-sync. Do not use it as-is as the PULL sidecar for a live Obsidian vault either, in its default consumption mode.** Its core design goal — "publish an atomic, immutable snapshot via a symlink flip, for consumers that always re-resolve the symlink" — is fundamentally in tension with a directory that a running application (Obsidian/ignis) has open, watches with inotify, or bind-mounts by resolved path. The maintainer has also explicitly and repeatedly rejected adding push/bidirectional support as out of scope, calling local mutation + push-back "a race" and "not what git-sync is for" (main.go's own design doc says the same thing structurally).

Recommended path: **(d) reimplement the pull half ourselves**, as a small script/binary that does a plain `git pull`/`git fetch && reset --hard` **in-place** into the exact directory Obsidian has mounted (no worktree swap, no symlink), and write our own push logic as the other half of the same daemon so pull and push can be coordinated (mutex/lock) as one bidirectional-sync loop. This is a few hundred lines, not a multi-thousand-line vendor job.

Reasoning, in order of weight:
1. **The worktree+symlink model is not just an implementation detail we could route around — it's the entire mechanism by which git-sync gets atomicity**, and it's not optional (no flag disables it; see §2). Any wrapper that wants "sync in place" has to bypass git-sync's publish step entirely, at which point git-sync's main.go loop is providing us almost nothing (its value-add is precisely worktree+symlink+hooks).
2. **No push support, anywhere, ever** — confirmed by full source grep (§7) and by the maintainer's own words on two closed issues going back to 2017 and 2020 (§7). This is core, deliberate scope, not a missing feature we could bolt on cleanly without fighting the existing single-writer, single-direction data model (repo root is reset with `--soft` and worktrees are force-recreated every sync — see main.go:1925, main.go:1652-1663 — code that assumes it is the only writer to `.git`).
3. **License is Apache-2.0** (permissive, forking is legally fine), but **the sync logic is not an importable library** — it lives entirely in `package main` (main.go, credential.go, env.go) as unexported methods on an unexported `repoSync` struct. Only `pkg/cmd`, `pkg/hook`, `pkg/logging`, `pkg/pid1`, `pkg/version` are separate importable packages, and none of them contain the git orchestration logic. "Vendor it as a library" is not really on the table — you'd be vendoring (or copy-pasting) `main.go` and then needing to substantially rewrite its publish step anyway.
4. On the credential/auth side (§3), the PAT handling is simple and worth stealing conceptually (git's own `credential approve` + a private `GIT_CONFIG_GLOBAL` file, main.go:786-795, main.go:2056-2067) — but it's ~15 lines of git plumbing, not something that justifies adopting the whole binary.
5. The one thing genuinely worth reusing conceptually is the **exechook pattern** (§7) — an idempotent, retried, hash-triggered command after publish — as a design template for triggering our own push after a pull-driven local change, even though we won't use git-sync's implementation.

If, despite the above, minimizing engineering effort matters more than getting in-place sync right, option **(a)** (run git-sync unmodified as a second sidecar purely for PULL, with `--root` pointed at a *scratch* volume, and have a **separate small script poll the published symlink and rsync its target into the real, stable vault path Obsidian has open**) is a viable fallback — but it adds an extra rsync hop, an extra polling loop, and does not remove the core problem of merging that rsync against local edits your push daemon is about to commit. It is a worse architecture than just writing the ~150-line pull loop ourselves.

---

## 1. Architecture — what happens on each loop iteration

Entry point `main()` (main.go:144) runs as an infinite `for {}` loop (main.go:1052-1146):

1. Start a `context.WithTimeout(ctx, *flSyncTimeout)` (main.go:1054) — this is the **per-sync deadline** (`--sync-timeout`, default 120s).
2. Call `git.SyncRepo(ctx, syncHooks)` (main.go:1056), which:
   - Refreshes credentials (`refreshCreds`, main.go:978-1019 — re-reads `--password-file`, re-calls `--askpass-url`, or refreshes the GitHub App JWT-derived token if near expiry).
   - `initRepo` (main.go:1355-1418): ensures `--root` exists and is a sane git repo (`git init -b git-sync` if not), and that `origin` points at `--repo`.
   - Reads the **current** worktree by resolving the `--link` symlink (`currentWorktree`, main.go:1843-1856).
   - Runs `git fetch <repo> <ref> --verbose --no-progress --prune --no-auto-gc [--depth N | --unshallow] [--filter F]` (main.go:2002-2029) into `FETCH_HEAD`.
   - Resolves `FETCH_HEAD^{}` to get `remoteHash` (main.go:1893-1897).
   - If `remoteHash == currentHash` and the current worktree passes a sanity check, it's a no-op (`changed=false`).
   - Otherwise: `git reset --soft remoteHash` on the **root** repo (not the worktree — no files touched, main.go:1925), then `git worktree add --force --detach <root>/.worktrees/<hash> <hash> --no-checkout` (main.go:1657, called from `createWorktree` main.go:1644-1663), then `configureWorktree` (main.go:1667-1750) which rewrites the worktree's `.git` file to use a relative gitdir reference, applies sparse-checkout if configured, does `git reset --hard <hash> --` to actually populate files (main.go:1729), and runs `git submodule update --init [--recursive] [--depth N]` if submodules aren't off (main.go:1735-1747).
   - Calls the **pre-publish exechook** (`beforePublish`, main.go:1955), then **atomically swaps the `--link` symlink** to point at the new worktree directory via a temp-symlink-then-`rename()` (`publishSymlink`, main.go:1592-1620), then calls the **post-publish hooks** (exechook + webhook, `afterPublish`, main.go:1973).
   - The **old** worktree is left in place and its mtime is touched (main.go:1966) to start a "staleness" clock.
3. Back in `main()`: on error, increments `failCount`; if `failCount >= --max-failures` (or `--init-max-failures` during the initial-sync phase, main.go:1046-1051), the process exits 1 (main.go:1059-1062). Otherwise it logs and loops.
4. On success: runs `git.cleanup(ctx)` (main.go:1090, defined at main.go:1752-1799) which deletes worktrees older than `--stale-worktree-timeout` (default 0 = immediately, main.go:1420-1441), prunes worktree metadata, expires unreachable reflog entries, and runs `git gc` per `--git-gc` mode.
5. If `--one-time`, waits for any in-flight hooks and exits (main.go:1095-1119, exit code reflects hook success).
6. If the resolved hash equals the literal `--ref` (i.e. ref was already a commit hash — nothing left to poll for), it sleeps forever (main.go:1121-1125, `sleepForever` at main.go:1291-1296).
7. Otherwise sleeps `waitTime` (`--period`, or `--init-period` before the first success) via `time.NewTimer`, interruptible by `--sync-on-signal` (main.go:1139-1145).

### Exact directory layout under `--root` (e.g. `/git`)

```
/git/                              <-- --root (a real git repo: `git init -b git-sync`)
  .git/                            <-- normal git dir; "origin" remote = --repo
    worktrees/
      <hash>/                      <-- git's own per-worktree metadata (HEAD, index, etc.)
      <hash2>/                     <-- (one per worktree, old ones pruned by `worktree prune`)
  .worktrees/
    <hash>/                        <-- ACTUAL CHECKED-OUT FILES for commit <hash>
      .git                         <-- a *file* (not dir): "gitdir: ../../.git/worktrees/<hash>"
      <your repo's files...>
    <hash2>/                       <-- previous worktree, kept until --stale-worktree-timeout elapses
  <link>                           <-- symlink, e.g. /git/myrepo -> .worktrees/<hash>  (relative target)
```

`worktreeFor(hash)` = `git.root.Join(".worktrees", hash)` (main.go:1838-1840). The symlink target is written as a **relative path** specifically so the whole `--root` can be bind/volume-mounted at a different path in another container and still resolve (main.go:1601-1607, comment: *"linkDir is absolute, so we need to change it to a relative path... so it can be volume-mounted at another path and the symlink still works"*).

---

## 2. Critical for us — does the published path change on every sync?

**Confirmed: yes, precisely as suspected, and there is no flag to avoid it.**

- The `--link` **symlink's own path** is stable (e.g. `/git/myrepo` never moves — `absLink` is computed once, main.go:717).
- But **what the symlink points to changes on every commit change**: a brand-new directory `.worktrees/<new-hash>` is created (main.go:1644-1663), fully populated, and only then is the symlink atomically re-pointed at it (main.go:1592-1620). The **old** target directory is *not* deleted immediately by default — it's kept until `--stale-worktree-timeout` (default `0`, meaning "next cleanup pass deletes it right away", main.go:1420-1441, doc at main.go:2868-2873) — but it **will** be `os.RemoveAll`'d out from under anything still referencing it.
- This is git-sync's whole reason for existing in this shape: the man page is explicit that this design exists *because* `git checkout` isn't atomic, and the symlink is "the contract" (main.go:2536-2550, man text).

**Why this is a hard blocker for a live-mounted Obsidian vault**, concretely:
1. **Anything that resolves the symlink once and keeps using the resolved (real) path** — e.g. a container that bind-mounts `readlink(/git/myrepo)` rather than `/git/myrepo` itself, or a filesystem watcher (inotify/fanotify-based, which is how Electron's chokidar and most "watch this folder" libraries work) that calls `realpath()` before establishing a watch — will silently stop seeing updates the moment git-sync swaps the symlink, because inotify watches are bound to the **inode**, not the path. The watch keeps firing on the *old, soon-to-be-deleted* directory, or just goes silent.
2. **Any file descriptor opened through the old path** stays valid (Unix semantics: unlink doesn't invalidate an open fd) but now points at a file that is about to be `os.RemoveAll`'d from disk (deleted-but-open) rather than the live, current file. Obsidian holding a vault file open for editing while a sync happens would silently diverge from what's on disk / lose the file when the worktree is GC'd.
3. There is **no flag** for "sync in place, mutate the same directory every time." `--link` always implies the worktree-swap publish model; the man page frames this as a deliberate, non-optional feature (main.go:2544-2550: *"Why the symlink? ... git-sync 'publishes' updates via the symlink to present an atomic interface to consumers"*).

**Net:** you would have to consume git-sync by always re-`readlink()`-ing `--link` immediately before every file access and never caching the real path or holding a long-lived watch/fd on the resolved directory — which is exactly what Obsidian (an Electron app with its own file-watching and open-file-handle model) does not do and cannot easily be made to do. This alone rules out feeding Obsidian directly from `--link`.

---

## 3. Auth — exactly how a PAT is passed and consumed

All credential paths funnel into `git credential approve` via stdin (`StoreCredentials`, main.go:2055-2067):
```go
creds := fmt.Sprintf("url=%v\nusername=%v\npassword=%v\n", url, username, password)
git.RunWithStdin(ctx, "", creds, "credential", "approve")
```
This relies on `credential.helper = cache --timeout 3600` being set globally (main.go:2288-2291, `SetupDefaultGitConfigs`), and `core.askPass = true` is also forced globally (main.go:2292-2294) so an interactive prompt can never hang the process. Git config is isolated from the host: a private, temp `GIT_CONFIG_GLOBAL` file is created per-process and `GIT_CONFIG_NOSYSTEM=true` is set (main.go:786-795) — no `~/.gitconfig` or `~/.git-credentials` pollution, no `~/.netrc` is used or read.

Concrete PAT paths, exact flag/env names:
- **Username + password/PAT**: `--username` / `$GITSYNC_USERNAME`, plus **either** `$GITSYNC_PASSWORD` (env-only, no flag — deliberately, to avoid `ps`-visible secrets, main.go:270-272) **or** `--password-file` / `$GITSYNC_PASSWORD_FILE` (re-read fresh before every sync, so token rotation works automatically — main.go:2803-2809). For GitHub, "For a private repo, use a PAT as the password with any non-empty username" is the documented pattern (implied by main.go:2957-2966, and standard GitHub HTTPS auth). A GitHub PAT needs **`repo` scope** (classic) or, for a fine-grained PAT, **Contents: Read** (or **Read and write**, if you also intend `git push` from the same credential) on that repository — git-sync itself only ever does read operations (`fetch`), so **Contents: Read-only is sufficient for git-sync's own use**.
- **URL-embedded creds**: `https://user:pat@github.com/...` in `--repo` is also auto-extracted into `--username`/password if `--username` isn't separately set (main.go:721-737), but mixing this with `--username` is a fatal config error (main.go:601-605).
- **`--askpass-url` / `$GITSYNC_ASKPASS_URL`**: GETs a URL every sync, expects a `200` with body lines `username=...` / `password=...` (main.go:2133-2185, doc main.go:2587-2591) — designed for e.g. a cloud metadata endpoint or a sidecar that mints short-lived tokens.
- **`--credential` / `$GITSYNC_CREDENTIAL`**: JSON object/array `{"url":...,"username":...,"password"|"password-file":...}` for per-URL creds (e.g. submodules on a different host) — schema at credential.go:27-32, doc main.go:2596-2615.
- **GitHub App auth**: `--github-app-private-key-file` (or `$GITSYNC_GITHUB_APP_PRIVATE_KEY`), `--github-app-client-id` or `--github-app-application-id`, `--github-app-installation-id`, optional `--github-base-url`. git-sync signs a JWT (RS256) itself and calls `POST {base}/app/installations/{id}/access_tokens` to mint a short-lived installation token, refreshed automatically when within 30s of expiry (main.go:1008-1016, `RefreshGitHubAppToken` main.go:2187-2274). This is the most "modern" option and avoids a long-lived PAT entirely, at the cost of managing a GitHub App instead.
- **SSH**: `--ssh-key-file` (default `/etc/git-secret/ssh`, repeatable) and `--ssh-known-hosts[-file]` (defaults `/etc/git-secret/known_hosts`), consumed by setting `$GIT_SSH_COMMAND` (main.go:2069-2106) — not via `~/.ssh/config`.
- **Cookie file**: `--cookie-file` reads a fixed path `/etc/git-secret/cookie_file` and sets `http.cookiefile` (main.go:2108-2123).
- No `.netrc` support anywhere in the source.

All logging is credential-redacted: `--password`, embedded URL passwords, and `--credential` password fields are all replaced with `"REDACTED"` before being logged (main.go:1201-1266, `redactURL`, `logSafeFlags`).

---

## 4. Loop/timing flags

- `--period` / `$GITSYNC_PERIOD` (default `10s`, min `10ms`): sleep between sync attempts once past initial sync (main.go:201-203, 505-507).
- `--init-period` / `$GITSYNC_INIT_PERIOD`: separate (usually faster) retry interval used **only** until the first successful sync (defaults to `--period` if unset, main.go:198-200, 508-512). This is the "initial sync phase" (main.go:2559-2568).
- `--sync-timeout` / `$GITSYNC_SYNC_TIMEOUT` (default `120s`, min `10ms`): hard deadline for one complete sync attempt, enforced via `context.WithTimeout` (main.go:204-206, 1054). If exceeded, the git subprocess's context is cancelled and the run returns a `DeadlineExceeded`-wrapped error (pkg/cmd/cmd.go:85-87) — git itself is killed, not gracefully stopped.
- `--max-failures` / `$GITSYNC_MAX_FAILURES` (default `0`): consecutive failures allowed before the **process exits(1)**; negative = retry forever (main.go:213-215, 1059-1062).
- `--init-max-failures` / `$GITSYNC_INIT_MAX_FAILURES`: separate failure budget for the initial-sync phase only, falls back to `--max-failures` if unset (main.go:216-218, 1043-1051).
- `--one-time` / `$GITSYNC_ONE_TIME`: exits after the first sync, exit code reflects hook success/failure (main.go:207-209, 1095-1119).
- `--sync-on-signal` / `$GITSYNC_SYNC_ON_SIGNAL`: an OS signal (name or number) that wakes the sleep early and triggers an immediate resync (main.go:210-212, 518-534, 1137-1145).
- **No backoff, no jitter, anywhere in the retry path.** On failure the loop just logs, increments `failCount`, and sleeps the same fixed `waitTime` (`--period`/`--init-period`) before trying again (main.go:1063, 1134-1146). `pkg/cmd/cmd.go` (the subprocess runner) does zero retries itself — one `exec.CommandContext` call, one attempt, error bubbles straight up (pkg/cmd/cmd.go:63-94). The only fixed (non-exponential) backoffs in the codebase are for the **hooks**, not the main sync: `--exechook-backoff` (default 3s), `--pre-publish-exechook-backoff` (default 3s), `--webhook-backoff` (default 3s) — each hook is retried on a flat backoff by `HookRunner.Run` until it succeeds (pkg/hook/hook.go:127-156).
- On transient network failure: `git fetch` fails → `SyncRepo` returns an error → `failCount++` → logged as "error syncing repo, will retry" (main.go:1063) → same fixed `--period` sleep → retry. No special-casing of network vs. other errors.

---

## 5. Safety & correctness

- **Dirty worktree**: not really possible by construction — worktrees are ephemeral, force-recreated (`git worktree add --force --detach ... --no-checkout` then `reset --hard`, main.go:1657, 1729), and nothing in git-sync's own operation ever writes into a worktree afterward. (This *is* precisely the model that breaks if **we** point a live-editing Obsidian at that same directory — see §2.)
- **Local divergence**: impossible to have any, because git-sync never commits locally; the root repo is `reset --soft` to the fetched remote hash every sync (main.go:1925) — it is a pure mirror, one-directional by construction.
- **Force-push upstream**: handled transparently — `git fetch` + `reset --soft <remoteHash>` (not `merge`/`rebase`) means a force-push on the remote is simply followed; there's no "diverged branches" concept to reconcile since the local root never has independent commits.
- **Shallow clones (`--depth`)**: default `1` (a single commit); `0` means full history (main.go:176-178). If `--depth` is later removed/changed and the local repo is shallow, `fetch` auto-detects via `git rev-parse --is-shallow-repository` and adds `--unshallow` (main.go:2011-2019, `isShallow` main.go:2031-2044).
- **Submodules**: `--submodules` = `recursive` (default), `shallow`, or `off` (main.go:98-104, 182-184). Applied via `git submodule update --init [--recursive] [--depth N]` per worktree (main.go:1735-1747). Submodule auth is expected to go through `--credential` for the submodule's own URL.
- **LFS**: **not supported at all** — zero mentions of `lfs` anywhere in the Go source, Dockerfile, or docs (grep across the whole tree came back empty). Confirmed by GitHub issue #654 ("Add git-lfs to image", closed) — a user demonstrated a custom-image workaround (`apt-get install git-lfs` on top of the git-sync image) but the maintainer never merged first-class support, citing lack of an e2e test. Note the shipped image is `FROM scratch` (see §6) so that specific apt-based workaround doesn't even apply to current releases — you'd have to build your own base image.
- **Repo corruption/recovery**: on every sync, `sanityCheckRepo` (main.go:1457-1499) checks the root isn't empty, is actually a repo root (`rev-parse --show-toplevel`), passes `git fsck --connectivity-only`, and has no stale `.git/shallow.lock` (main.go:1443-1455, `hasGitLockFile`). If any check fails, git-sync **wipes the entire root directory's contents** (`removeDirContents`, main.go:1381-1384, keeping the mount point itself since it may be a mounted volume) and re-`git init`s from scratch — i.e. self-healing by nuking and re-cloning, no partial recovery attempted. Similarly `sanityCheckWorktree` (main.go:1501-1536) checks a worktree's `HEAD` matches its directory name and passes `fsck`; a failing worktree is deleted and a fresh one is built (main.go:1902-1909).

---

## 6. Container ergonomics

- **Base image**: multi-stage build, final stage is **`FROM scratch`** (Dockerfile.in:145-166) — an intermediate `debian:trixie`-based stage installs `git`, `openssh-client`, `ca-certificates`, `curl`, `socat`, `bash`, `coreutils`, `tar`, `gzip`, etc. via `stage_binaries.sh`, then only the staged files are copied into the scratch final image (Dockerfile.in:44-146). No shell of any kind survives except what's explicitly staged (`bash`, symlinked to `sh`, Dockerfile.in:75).
- **Non-root/UID**: runs as **`USER 65533:65533`** by default (`nobody`-style UID, Dockerfile.in:149), `/git` is created `chown 65533:65533`, mode `02775` (setgid + group-write, Dockerfile.in:126). `--add-user` / `$GITSYNC_ADD_USER` appends an `/etc/passwd` entry for whatever UID the container actually runs as (main.go:1317-1339, needed for SSH, which does UID lookups) — `/etc/passwd` is shipped world-writable (`chmod 0666`, Dockerfile.in:118) specifically to support this. `--group-write` / `$GITSYNC_GROUP_WRITE` sets `umask 0002` instead of the default `0022` so all synced files/dirs end up group-writable (main.go:690-694), useful when the consumer container runs as a different UID in the same GID.
- **File ownership**: whatever UID/GID the process runs as (root dir, worktrees, and the symlink are all created by git-sync's own process, no chown step for arbitrary consumers beyond the group-write umask trick above).
- **Health/liveness**: `--http-bind` (`$GITSYNC_HTTP_BIND`) opens an HTTP listener; `/` returns `200` once `setRepoReady()` has been called after the first successful sync, else `503` (main.go:867-873, `getRepoReady`/`setRepoReady` main.go:1273-1287, 1073) — a simple, genuinely-meaningful liveness/readiness probe endpoint (not a dumb "process is up" check). `--http-metrics` exposes Prometheus metrics at `/metrics` (`git_sync_duration_seconds`, `git_sync_count_total`, `git_fetch_count_total`, `git_sync_askpass_calls`, `git_sync_refresh_github_app_token_count`, `git_sync_hook_run_count_total` — main.go:57-90, pkg/hook/hook.go:31-40). `--http-pprof` exposes Go pprof endpoints. Exit codes: `1` on fatal config errors or exceeding `--max-failures`; on `--one-time`, `0`/`1` reflects hook success (main.go:1095-1119); no other documented exit-code contract.
- **Logging**: structured, via `go-logr`'s `funcr` (main.go:44, `pkg/logging`), JSON-ish key/value format (seen directly in issue #928's pasted logs: `{"logger":"","ts":"...","caller":{"file":"main.go","line":...},"level":0,"msg":"..."}`). `-v/--verbose` controls verbosity 0-9 with a documented meaning per level (main.go:2902-2913). `--error-file` writes the last fatal error to a file for other tooling to poll (main.go:195-197).
- **pid 1 handling**: if git-sync detects it's running as PID 1 (`os.Getpid() == 1`, main.go:146), it re-execs itself as a child and becomes a minimal init that reaps zombies and forwards signals (`pkg/pid1/pid1.go`) — a nice detail if run as a container's sole process without an init system, irrelevant when run as a sidecar under a real entrypoint/supervisor.

---

## 7. Extension points — hooks, and the push question

**Exechook** (`--exechook-command` / `$GITSYNC_EXECHOOK_COMMAND`, main.go:231-239): runs an arbitrary command, **with cwd set to the newly-published worktree path** (`getWorktree` closure at main.go:927-929 → `git.worktreeFor(hash).Path()`), env includes `GITSYNC_HASH=<hash>` (pkg/hook/exechook.go:71-72), fires **after** the symlink swap (main.go:1029-1037, `afterPublish`), retried on `--exechook-backoff` until it exits 0 (pkg/hook/hook.go:143-155). There's also `--pre-publish-exechook-command`, which fires **before** the symlink swap, with `GITSYNC_HASH` set to the *previous* hash (main.go:1021-1028, doc main.go:2822-2828) — useful for a "drain/prepare" step, but not for pushing (the new content isn't published yet).

**Webhook** (`--webhook-url`, main.go:251-266): HTTP call after publish, header `Gitsync-Hash: <hash>`, method/success-status/timeout/backoff all configurable, fire-and-forget if `--webhook-success-status 0`.

Both hook types are **explicitly documented as "must be idempotent"** and are "not guaranteed to fire exactly once per hash" (main.go:3002-3018) — by design for a fetch-and-publish tool, not a transactional trigger. This makes exechook usable as a "kick off a push attempt" trigger conceptually (worth stealing as a pattern for our own daemon), but not reliable enough to *be* the push mechanism itself without our own idempotency/locking on top — and since git-sync's worktree is throwaway and gets deleted/replaced on the very next pull, anything the hook does in that directory (like committing local edits) can race with the next fetch cycle.

**Is there any push support at all — even experimental? No.** A full-text grep for `push` across the entire Go source (excluding `vendor/`) returns zero hits related to git push functionality (only unrelated matches inside vendored Prometheus/protobuf/syscall code, e.g. `PushOptions`, `ackonpush`).

GitHub issue search (`gh search issues --repo kubernetes/git-sync`) confirms this is deliberate, not just unimplemented:
- **Issue #309, "git push back to git repo"** (closed, 2020) — maintainer @thockin: *"This is really not what git-sync is for. It exists to sync itself to some 'upstream'. Making local changes and pushing them is a race and (even if the git repo were set up how you want) likely to spuriously fail... If you think the local tree is mutable, why not just 'git clone' it yourself and manage it that way? Git-sync is not offering much value to you, I think."*
- **Issue #40, "scope for using ssh to push back to a repo; folder hierarchies shift"** (closed, 2017) — same conclusion; also independently confirms the worktree-relocation problem from §2 (user found their consuming container's mount point kept getting new `rev-<hash>` subfolders that git didn't recognize as the repo root).
- No open or closed issue mentions "bidirectional"; a "two-way" search surfaces nothing on-topic either.

**Conclusion: push is not just missing, it's an explicitly rejected use case by upstream**, on both correctness grounds (race between local edits and the next scheduled fetch) and scope grounds (git-sync's contract is "read-only mirror, atomic publish"). Adding push support well would mean rewriting most of `SyncRepo`'s assumptions (root is `reset --soft` every cycle assuming no local commits exist, main.go:1925; worktrees are unconditionally force-recreated, main.go:1652-1657) — this is not a small patch on top of upstream, it's a different program that happens to share some git-plumbing helpers.

---

## Surprises / traps to flag

1. **The symlink-swap model is not opt-out** — there is no "in-place" mode. If you want git-sync's polling/retry/credential ergonomics without the worktree churn, you cannot get it from this binary; you'd have to intercept between the symlink and your real consumer (extra moving part) or not use git-sync's publish step at all.
2. **Old worktrees are deleted out from under any long-lived reference by default** (`--stale-worktree-timeout` default = `0`, i.e. "delete on the very next cleanup pass" — main.go:2868-2873). This is more aggressive than it sounds; the very next successful sync after a hash change can reap the previous worktree.
3. **`FROM scratch`** means there is no package manager, shell utilities beyond what's staged, and no easy way to `docker exec` in and add tools (e.g. LFS, ripgrep, etc.) without rebuilding the whole image yourself from `Dockerfile.in`.
4. **No `~/.netrc`, no long-lived `.git-credentials` file** — everything goes through `git credential approve` backed by `credential.helper = cache --timeout 3600` (an **in-memory, per-process git-credential-cache daemon** spun up by git itself, not a file on disk). This means credentials are **not persisted across process restarts** — every container restart re-runs the full credential-setup flow, which is fine for our use case but worth knowing if you were hoping to inspect `.git-credentials` for debugging.
5. **No jitter/exponential backoff on the main sync loop at all** — every replica in a fleet configured with the same `--period` will drum-beat in lockstep. Irrelevant at our single-sidecar scale, but shows the retry model is deliberately simple, not something to lean on for resilience against flaky networks beyond "just wait a fixed time and try again."
6. **`--depth` default is `1`** (shallow by default) — fine for a pull-only mirror, but if we want to later support `git log`/history features in the vault UI, or need `git blame`, we'll want `--depth 0` equivalent (full clone) in our own reimplementation, not git-sync's default.
7. **PAT scope**: only `Contents: Read` (fine-grained) / `repo` (classic) is needed for git-sync's own read-only operation; since **we** need push too, our token needs **Contents: Read and write** — this is a difference from what git-sync's own docs assume.
