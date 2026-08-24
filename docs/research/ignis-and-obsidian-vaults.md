# ignis + Obsidian vault filesystem research, for obsync's commit strategy

Repo cloned for this research: `https://github.com/Nystik-gh/ignis`, commit as of 2026-08-24 (default branch `main`), into a scratch dir — all `path:line` citations below are relative to the repo root.

---

## Constraints this places on obsync

1. **Never touch anything ignis is actively syncing except via plain file writes/renames/deletes inside `VAULT_ROOT`'s per-vault subfolders.** ignis's own writes are plain `fs.promises.writeFile` (truncate-and-write, not write-then-rename) — see Part A.2. obsync must match this: don't try to be "more atomic" than ignis itself, since a torn write from `git checkout`/`merge` is no worse than what ignis already risks on crash. But obsync *should* use its own write-then-rename where it can, since ignis does not.
2. **Run obsync as the same UID:GID as ignis (`PUID`/`PGID`, default `1000:1000`), and mount the same `./vaults` host path at the same container path the way ignis expects (`/vaults`, one subfolder per vault).** Files obsync writes must be owned/group-writable by that same UID so ignis (`gosu $RUN_USER`, entrypoint.sh:135) can still write them. See Part A.1.
3. **Exclude `.git/` from anything ignis watches or serves.** ignis's chokidar watcher already hardcodes `ignored: [/(^|[/\\])\.git([/\\]|$)/]` (packages/server-core/src/watcher.js:44), so a `.git` directory sitting inside a vault folder is already invisible to the live-update pipeline — good, this is free. obsync should keep the repo's `.git` there (one per vault) rather than out-of-tree, since ignis already excludes it.
4. **External changes ARE noticed and pushed live** — ignis is not a static snapshot server. It runs a persistent chokidar watcher per open vault (`awaitWriteFinish: {stabilityThreshold: 300ms, pollInterval: 100ms}`, watcher.js:40-42) and pushes `created`/`modified`/`deleted` events to every connected browser tab over WebSocket within about a second, invalidating that tab's in-memory metadata/content caches (packages/shim/src/fs/watcher-client.js). **This means a `git pull`/`merge`/`reset --hard` obsync performs directly on the vault files will be picked up and reflected live in any open Obsidian tab** — this is a major favorable constraint: obsync does not need to signal ignis or trigger a reload itself. But it also means:
   - A `git checkout`/`merge` that touches many files at once will fire a *burst* of watcher events and WebSocket messages, one per file. obsync should not assume this is "atomic" from Obsidian's point of view — Obsidian will see files change one-by-one, so a half-applied merge is visible mid-flight to an open tab, same as it already is for ignis's own coalesced writes.
   - The `awaitWriteFinish` (300ms stability, 100ms poll) means a very fast write-then-immediately-overwrite (e.g., checking out a file then a merge amending it) can be coalesced by chokidar into a single event, but there is no guarantee obsync's multi-file operation is seen as a single unit — assume no atomicity across files.
5. **Do not write while a coalesced write from ignis might be in flight for the same path**, or you can lose ignis's write. Server-side write coalescing (`WRITE_COALESCE_MS`, default `0`/disabled — apps/ignis-server/server/write path in `packages/server-core/src/write-coalescer.js`) buffers a browser's write in memory and only flushes to disk after a debounce window elapses (up to 60000ms if an operator raises `WRITE_COALESCE_MS`). While a write sits in that buffer, `fs.promises.readFile`/`stat` from obsync's side of the filesystem would see **stale (pre-write) content**, because the buffered bytes only exist in the Node process's memory, not on disk, until the timer fires (packages/server-core/src/write-coalescer.js:152-186). obsync reading the vault to build a commit could therefore momentarily miss the very latest keystroke-triggered save if an operator has turned coalescing on. Default is off (`WRITE_COALESCE_MS=0`), so by default every write lands on disk immediately — but obsync's design should not assume that stays true for every deployment.
6. **A third writer can exist: the `ob` (obsidian-headless) CLI subprocess, if the operator has enabled ignis's built-in "Headless Sync" plugin for Obsidian's own paid Sync service.** That plugin `spawn`s `ob sync --continuous` as a long-lived child process with `cwd: state.vaultPath` (apps/ignis-server/server/plugins/headless-sync/sync-manager.js:133-141), and that process writes to vault files **directly**, completely bypassing ignis's HTTP write-coalescer/echo-guard/watcher-suppression machinery. If a user has this enabled, obsync is not the only external actor mutating the vault outside ignis's own write path — obsync's commit logic (and its assumptions about "quiet periods") needs to tolerate a second concurrent writer it has no visibility into or coordination with.
7. **Any bulk write obsync performs (`git checkout`, `merge`, `reset`) should be treated by ignis's `awaitWriteFinish` watcher as ordinary filesystem noise — no vault "swap" or `.obsidian`-only bulk update path exists that ignis special-cases.** The one thing that *is* special-cased is vault add/rename/remove, which stops and restarts the watcher (apps/ignis-server/server/vault-lifecycle.js) — obsync should never rename/move the vault's own folder itself, only mutate file contents inside it, or it risks racing ignis's `withWatcherStopped()` vault-mutation logic.
8. **`.obsidian/workspace.json` (and, in multi-tab setups, `workspace.<name>.json`) will churn on essentially every keystroke/pane focus and must be excluded from commits**, exactly as the wider Obsidian-git community already concluded (Part B.7) — ignis's own transform layer (packages/shim/src/fs/transforms.js, described in docs/ARCHITECTURE.md "Transforms") treats `.obsidian/workspace.json` as inherently per-tab, ephemeral UI state, redirecting it per open browser tab. Committing this file adds pure noise and false "activity."
9. **Follow, don't fight, the existing prior art:** there is an open, maintainer-acknowledged feature request for exactly this ("Add support for git sync", issue #14) where the maintainer's own stated plan is a server-side plugin, and in the meantime the community's accepted workaround is precisely a **sidecar container** running `kubernetes/git-sync` against the same `/vaults` mount — i.e., obsync's whole approach is validated as the currently-recommended pattern, not a novel or discouraged one (see Part A.5).
10. **Expect and handle git conflict markers landing inside real `.md` note bodies.** This is the standing failure mode of every git-based Obsidian sync tool in the ecosystem (obsidian-git's own default `syncMethod: "merge"` can leave `<<<<<<<`/`=======`/`>>>>>>>` inside a note file and logs unresolved files to `conflict-files-obsidian-git.md`, Part B.8/B.9) — obsync's design must have an explicit conflict strategy (e.g., always squash to a merge commit only when there is no textual conflict, otherwise leave the working tree conflicted and surface it rather than silently resolving to one side) or vault content will get corrupted with raw diff3 markers that Obsidian will happily save right back over on the next keystroke.

---

## Part A — ignis

### A.1 Deployment shape, volumes, UID/GID

Yes — ignis ships an official image (`nobbe/ignis:latest`) and both a repo-root `docker-compose.yml` (apps/ignis-server/docker-compose.yml, builds from source) and a documented compose file for the published image (apps/docs/src/content/docs/server/deploy.md).

Container paths (from the Dockerfile and docs, exact):

| Host (example) | Container path | Purpose |
|---|---|---|
| `./vaults` | `/vaults` | Vault root — one subfolder per vault. `VAULT_ROOT` env var, default `/vaults` (apps/ignis-server/Dockerfile:63; apps/docs/src/content/docs/server/environment.md). Declared as a `VOLUME` (Dockerfile:71). |
| `./data` | `/app/data` | Ignis server state: server-plugin config, sync state/tokens (`DATA_ROOT`, default `/app/data`; environment.md). Not declared as an explicit `VOLUME` in the Dockerfile but documented as persistent. |
| named volume `obsidian-app` | `/app/obsidian-app` | Extracted Obsidian client bundle, downloaded on first run (`OBSIDIAN_ASSETS_PATH`, Dockerfile:64). Declared `VOLUME` (Dockerfile:72). |

UID/GID: the image runs as root at container start, then `entrypoint.sh` creates (or reuses) a user/group for `PUID`/`PGID` (default `1000`/`1000`, Dockerfile:66-67, entrypoint.sh:5-6), `chown -R`s `/app/obsidian-app`, `/app/data`, and `/vaults` to that UID:GID (entrypoint.sh:30, best-effort — a read-only/NFS `root_squash` mount just logs a warning and continues), and finally `exec gosu "$RUN_USER" node .../server/index.js` (entrypoint.sh:135) — so the actual Node process, and therefore every file it writes into the vault, runs as `PUID:PGID`, not root. **obsync must run as the same PUID:PGID** (or at least a UID/GID that has read/write on the mounted volume) or file ownership will fight ignis's chown and each side's writes will end up unreadable/unwritable by the other.

The deploy docs explicitly document mounting vaults from other host locations/NAS mounts directly under `/vaults/<name>`, and symlinks inside `/vaults` are followed only if the link target is also mounted into the container at the same host path (deploy.md, "Vaults on other mounts"). Relevant for obsync if a vault folder is itself a symlink.

### A.2 Vault write mechanics: atomicity

Not atomic. There is no write-to-temp-file-then-rename anywhere in the write path. The actual disk write is a single `fs.promises.writeFile(absPath, data, encoding)` call:

- Server-side "coalescer" (used for all `/api/fs/writeFile` requests, including from Obsidian's own note-save path): `packages/server-core/src/write-coalescer.js:64-65` — `writeToDisk()` calls `fs.promises.writeFile` directly against the real path. `writeFile()` internally does open-with-truncate + write + close; there is a window during the write where the file is truncated/partial on disk if the process is killed mid-write.
- The coalescing (`WRITE_COALESCE_MS`, default `0` = disabled) only affects *when* the write happens (buffered in-process for up to the configured window before hitting disk), not *how* — the eventual disk write is still the same non-atomic `fs.promises.writeFile` (write-coalescer.js:152-186, `writeToDisk` at 64).
- The route handler (`apps/ignis-server/server/routes/fs.js`, `POST /api/fs/writeFile`) does `mkdir -p` on the parent dir then calls the same `writeCoalesced()` — no temp file or rename anywhere in this file.
- `rename`/`copyFile`/`unlink`/`rmdir`/`rm` routes exist and use plain `fs.promises.rename` / `copyFile` / `unlink` / `rmdir` / `rm` (fs.js) — again no atomic-swap pattern; a `rename` (used e.g. for note renames) is atomic at the OS level for a same-filesystem move, but `writeFile` for content changes is not.

So: **yes, there is a real window where a file on disk can be empty or partial** — during any `writeFile` call (a `truncate` then `write`), if the process/container is killed or the underlying filesystem is slow/unreliable (this is explicitly why `WRITE_COALESCE_MS` exists — for "slow filesystems (rclone, FUSE, NFS, SMB)", per `apps/docs/src/content/docs/server/environment.md`). This is a pre-existing, acknowledged risk in ignis itself, not something obsync introduces — but obsync reading vault content to build a commit could, in the worst case, read a torn/partial file mid-write. obsync should treat any file whose mtime is within its own `awaitWriteFinish`-equivalent settle window as "may still be writing" before committing it, mirroring what chokidar does on the ignis side (300ms stability threshold, watcher.js:41).

The client (browser) side additionally coalesces rapid writes within a 100ms "quiet" window / 2000ms max-wait cap before even sending the HTTP request (`packages/shim/src/fs/write-coalescer.js:8-9`, `QUIET_MS = 100`, `MAX_WAIT_MS = 2000`), and has its own retry-with-backoff durability layer for failed writes (`packages/shim/src/fs/write-durability.js`, retries up to 8 times with backoff up to 30s, flushed on `pagehide`/tab-hide). None of this changes the final on-disk write mechanism — it's still a single non-atomic `writeFile`.

### A.3 Open file handles, watchers, in-memory caching, and reload behavior

- **No long-lived open file handles are held on vault files.** Every read/write is a discrete `fs.promises.readFile`/`writeFile` call that opens, does the I/O, and closes (fs.js, write-coalescer.js). There's no persistent `fd` kept open across requests for vault content itself (the `packages/shim/src/fs/fd.js` module shims Obsidian's file-descriptor API on the *client* side, but it's backed by the same discrete HTTP read/write calls, not a real held-open server-side fd).
- **File watching:** yes, extensively. `packages/server-core/src/watcher.js` runs one `chokidar.watch(vaultPath, {...})` per open vault, with `ignoreInitial: true`, `awaitWriteFinish: {stabilityThreshold: 300, pollInterval: 100}` (watcher.js:38-43), ignoring `.git` directories (watcher.js:44-45). It starts when the first client opens a vault and auto-stops 10 minutes after the last listener disconnects (`IDLE_STOP_MS`, watcher.js:8). Every `add`/`change`/`unlink`/`addDir`/`unlinkDir` event is translated into `created`/`modified`/`deleted`/`folder-created` events and broadcast over WebSocket to every browser tab connected to that vault (`ws.js`, `wss.broadcastToVault`).
- **Vault content IS cached in memory, but bounded, and kept in sync by the watcher**, per docs/ARCHITECTURE.md ("Filesystem" section) and code: a client-side `MetadataCache` (all files' `{type,size,mtime,ctime}`, unbounded, populated from a single bootstrap call) and a `ContentCache` (LRU, default 50MB cap — `packages/shim/src/fs/content-cache.js:4`) that holds file bytes. Both live **in the browser tab's JS heap**, not server-side — the server itself does not keep a whole-vault cache; it re-reads from disk per request (modulo the small in-memory coalescing buffer described in A.2).
- **If obsync changes a file on disk underneath ignis, ignis will notice — via chokidar — and push a live update to every open tab within roughly a second** (300ms stability threshold + up to ~1s WebSocket resync debounce, `packages/shim/src/fs/watcher-client.js: RESYNC_DEBOUNCE_MS = 1000`). The client's `metadataCache`/`contentCache` get invalidated/updated for the changed path, and `fs.watch` listeners registered by Obsidian itself (e.g., its internal vault index) are dispatched via `fsWatch._dispatch(...)` (watcher-client.js). There is **no separate reload/refresh button needed** — it's a live push mechanism, not a manual-refresh-required one. There's also a full-tree reconciliation (`resync()`/`reconcile()`, watcher-client.js) fired on every WebSocket (re)connect, comparing the whole server tree against the cached metadata and replaying any missed events as synthetic created/modified/deleted — this is the safety net for changes that happened while a tab's socket was disconnected (e.g., obsync's writes happening while nobody has a tab open at all, or during a brief WS drop).
- **Echo suppression**: an `echo-guard` (`packages/shim/src/fs/echo-guard.js`) marks any path this same browser tab just wrote to, and suppresses watcher events for that path for 1500ms (`ECHO_SUPPRESS_MS`, echo-guard.js:5) so a tab doesn't reprocess its own write as an "external" change. This is scoped to the writing tab's own recent writes only — it has no effect on, and provides no protection against, obsync's writes, which will always be treated as genuine external changes and pushed to all tabs.
- One caveat found in issue tracking: issue #73 ("Watcher teardown race: `stopWatching()` deletes the dedup entry before `watcher.close()` resolves, so watchers stack on reconnect") was a real bug, fixed in ignis 0.8.10 — worth knowing that watcher lifecycle has had real races in the past; obsync should not assume watcher restarts are always clean on every ignis version.

### A.4 What ignis writes into the vault itself

ignis's server core does not write any config/state files of its own into the vault — `DATA_ROOT` (`/app/data`, default) is explicitly separate from `VAULT_ROOT` (`/vaults`) and is where "Ignis state: server plugin config, sync state, and tokens" lives (environment.md). No `.ignis` directory or similar was found anywhere in the vault-writing code paths.

Everything under `.obsidian/` in a given vault is written by Obsidian itself (running inside the browser, via the shimmed `fs`), exactly as it would on desktop — ignis doesn't special-case `.obsidian` content beyond the "Transforms" layer (docs/ARCHITECTURE.md "Transforms" section, `packages/shim/src/fs/transforms.js`):
- A **path resolver** redirects `.obsidian/workspace.json` reads/writes to `.obsidian/workspace.<name>.json` when a tab is opened with `?workspace=<name>`, so each tab/browser session can have an independent window layout file.
- A **read transform** masks Obsidian's built-in "Sync" core-plugin flag inside `core-plugins.json` while ignis's own Headless Sync (Obsidian-Sync-via-CLI) is active for that vault, and overrides the `active` field read out of `workspaces.json` per tab.
- A **write transform** keeps the canonical `active` field in `workspaces.json` stable on disk across tabs.

No dedicated server-side database, SQLite file, or similar was found written into a vault; `demo-template/.obsidian/` (apps/ignis-server/server/demo-template/.obsidian/) shows the minimal seed content ignis provisions for a brand-new demo vault: `app.json` (`{}`), `appearance.json` (theme/baseFontSize only), `core-plugins.json` (the standard Obsidian core-plugin toggle list) — nothing ignis-specific.

### A.5 Existing sync/git support, and open issues

**No git support exists in ignis**, and it is explicitly out of scope today. Open issue **#14, "Add support for git sync"** (github.com/Nystik-gh/ignis/issues/14) is directly on point:
- The maintainer (`Nystik-gh`) states the intended path is a first-party "server plugin" (like the existing Headless Sync plugin), plus shimming `child_process` so existing Obsidian git plugins can shell out to a real `git` binary — neither exists yet.
- A commenter (`l4rm4nd`) posted, and the maintainer endorsed as "a good solution... in the meantime," a docker-compose snippet running **`kubernetes/git-sync`** as a sidecar container against the same `/vaults` mount — i.e., the exact obsync pattern, already the community-recommended workaround.
- Another commenter (`wrein`) mentioned independently building an Obsidian plugin (client-side, running inside ignis) to sync a vault to GitLab/GitHub as git, which the maintainer welcomed as "always good with more options."

**What does exist and is easy to confuse with git sync**: the "Headless Sync" server plugin (`apps/ignis-server/server/plugins/headless-sync/`) wraps Obsidian's own official (paid) Sync product via the `obsidian-headless` CLI (`ob`), spawning `ob sync --continuous` (optionally `--pull-only`/`--mirror-remote`) as a per-vault child process (sync-manager.js:133-141). This is unrelated to git and is the third-writer hazard called out in Constraint 6 above.

Other issues worth noting for reliability expectations: **#66/#39/#48** — files intermittently showed 1970-01 modification times after Fast Note Sync / Obsidian Sync activity, fixed in 0.8.9 — evidence that mtime handling around external writers has had real bugs; obsync should not rely on mtime alone as a "did this change" signal without also checking content hash. **#73** — watcher-teardown race, fixed 0.8.10 (noted in A.3).

---

## Part B — Obsidian vault filesystem conventions

### B.1 What lives in a vault, and churn characteristics

Per Obsidian's own docs ([How Obsidian stores data](https://obsidian.md/help/Files+and+folders/How+Obsidian+stores+data)) and corroborating community sources: a vault is "a folder on your local file system, including any subfolders," and Obsidian creates a `.obsidian` configuration folder in the vault root "which contains preferences specific to that vault, such as hotkeys, themes, and community plugins." Notably, the official docs single out `.obsidian/workspace.json` and `.obsidian/workspaces.json` by name as files that "store the current workspace layout and **update whenever you open a new file**" — i.e., official confirmation this file churns on ordinary use, not just an inference from ignis internals.

Churn characteristics, corroborated across ignis's own architecture (Part A.4) and the wider community (obsidian-git discussion #709, Obsidian forum thread):

| Path | Churn | Notes |
|---|---|---|
| `.obsidian/workspace.json`, `.obsidian/workspace-mobile.json`, `.obsidian/workspaces.json` | **Very high** — rewritten on essentially every pane focus/file open, and per-device/per-tab (ignis even splits it per browser tab via a path-resolver transform, Part A.4) | The single biggest source of noisy diffs; universal advice is to `.gitignore` it. |
| `.obsidian/app.json` | Low-medium — hotkeys and misc app settings, changes only when the user actually changes a setting | Stable enough to sync if you want settings shared across devices. |
| `.obsidian/appearance.json` | Low — theme/CSS snippet toggles | Stable. |
| `.obsidian/core-plugins.json` | Low — which core plugins are enabled | Stable, but ignis's read-transform masks the "sync" flag conditionally (A.4), so a raw disk read of this file may not reflect what a given tab sees. |
| `.obsidian/community-plugins.json` | Low | List of enabled community plugin IDs; stable. |
| `.obsidian/plugins/<id>/data.json` | Variable, **can be high** for some plugins | Per-plugin settings/state; some plugins (indexers, sync caches) rewrite this frequently and it can be large — flagged by contributors as a source of oversized diffs (e.g., Smart Connections, Copilot plugin index files, per the trustedsec `.gitignore` and forum discussion, B.2). |
| `.obsidian/plugins/<id>/main.js`, `manifest.json`, `styles.css` | None after install, until plugin update | Effectively static; only changes on plugin version bump. |
| `.trash/` | High, if "system trash" isn't used | Obsidian's own soft-delete location when its "Deleted files" setting is "Move to Obsidian trash" rather than the OS trash; grows with every delete. Universally recommended to `.gitignore` — a git history doesn't need Obsidian's own trash mechanism duplicating version history. |

### B.2 Community-consensus `.gitignore` for a git-synced Obsidian vault

There is no single canonical file, but strong convergence across the Obsidian forum ([What should I gitignore for my vault's github repository?](https://forum.obsidian.md/t/what-should-i-gitignore-for-my-vaults-github-repository/101077)), the [trustedsec/Obsidian-Vault-Structure](https://github.com/trustedsec/Obsidian-Vault-Structure/blob/main/.gitignore) reference vault, and the [obsidian-git discussion #709](https://github.com/Vinzent03/obsidian-git/discussions/709) on the following recommended baseline, with reasoning per line:

```gitignore
# Per-device / per-session UI layout state. Rewritten on every pane focus
# and file open (confirmed by Obsidian's own docs). Committing this is
# pure noise and, worse, a genuine multi-device conflict source since two
# devices' "current open file" states will fight each other.
.obsidian/workspace.json
.obsidian/workspace-mobile.json
.obsidian/workspaces.json

# Obsidian's own soft-delete area. Deleting a note already creates a git
# history entry via the removal commit; duplicating that inside .trash/
# is redundant and can grow unbounded.
.trash/

# obsidian-git's own bookkeeping file (last auto-backup/pull timestamps).
# Meaningless outside the device that wrote it.
.obsidian-git-data

# OS/editor cruft that has nothing to do with the vault.
.DS_Store
Thumbs.db
.vscode/
.idea/
```

Beyond that baseline, opinion splits into two camps, both defensible:
1. **"Ignore all of `.obsidian/*`, sync nothing but notes."** Simplest, zero conflict risk on config files, but each device configures itself independently (no shared hotkeys/theme/plugin list). Recommended when devices intentionally have different plugin sets (e.g., mobile vs desktop).
2. **"Track the stable config files, ignore only the churny ones"** — i.e., commit `app.json`, `appearance.json`, `core-plugins.json`, `community-plugins.json`, and each plugin's `manifest.json`/`main.js`/`styles.css`/`data.json`, but still exclude `workspace*.json` and `.trash/`. Gives device parity for plugins/theme/hotkeys at the cost of occasional config conflicts if two devices both toggle a plugin before syncing.

Some contributors additionally exclude oversized or fast-churning **plugin-specific** state (e.g., search-index/embedding caches under specific plugin folders) on a case-by-case basis, since those regenerate locally and bloat the repo (forum thread, trustedsec `.gitignore`).

For obsync specifically: given it runs unattended (no user manually curating a `.gitignore` per repo), the safe default is the intersection everyone agrees on — `workspace*.json`, `.trash/`, OS cruft — and the choice of whether to include `.obsidian/plugins/*/data.json` and config files should be an explicit, documented obsync setting rather than a silent default, because both camps above are legitimate depending on whether the vault is single-device or multi-device.

### B.3 obsidian-git (Vinzent03) — actual defaults, as prior art

Source: `Vinzent03/obsidian-git`, `src/constants.ts` (`DEFAULT_SETTINGS`) and `src/automaticsManager.ts`, fetched directly from GitHub.

**Commit message template** (`src/constants.ts`):
```
commitMessage: "vault backup: {{date}}"
autoCommitMessage: "vault backup: {{date}}"
commitDateFormat: "YYYY-MM-DD HH:mm:ss"   // the {{date}} token format
```
`{{hostname}}` is also a supported template token (device identifier) though not used by default.

**Auto-commit/pull/push intervals — all disabled by default:**
```
autoSaveInterval: 0     // auto commit(-and-sync) interval, minutes; 0 = off
autoPushInterval: 0     // 0 = off
autoPullInterval: 0     // 0 = off
autoPullOnBoot: false
```
The plugin ships with automation **entirely opt-in** — a user must set a nonzero interval. Community guidance (per plugin discussions surfaced in search) commonly recommends 10–15 minutes once enabled.

**Debounce vs fixed-interval — a real either/or design choice** (`src/automaticsManager.ts:126-152`):
- If `autoBackupAfterFileChange` is `false` (the default), the timer is a plain periodic `setTimeout` that fires every `autoSaveInterval` minutes regardless of activity.
- If `autoBackupAfterFileChange` is `true`, the *same* `autoSaveInterval` value is instead used as the debounce window for an idle-triggered commit: the plugin listens for vault file-modify events and (re)starts a `debounce(doAutoCommitAndSync, time, true)` timer (Obsidian's own `debounce` utility) on every change, so the commit fires `autoSaveInterval` minutes after the *last* edit, not on a fixed clock. There is no separate/shorter debounce constant — the same user-configured interval doubles as either the fixed period or the idle-settle window depending on this one boolean.

**Pull-before-push and merge behavior:**
```
pullBeforePush: true          // always pulls before pushing, by default
syncMethod: "merge"           // vs "rebase" / "reset" — a real git merge, not fast-forward-only
mergeStrategy: "none"         // git merge -X strategy left unset; "ours"/"theirs" are opt-in, not default
squashCommitsBeforePush: false
disablePush: false
```
`pullBeforePush: true` by default is a strong prior-art signal: this plugin will not attempt a push without first pulling and merging remote changes, to avoid non-fast-forward push failures. `mergeStrategy: "none"` means the default behavior on an actual textual conflict is to leave real conflict markers in files, not silently prefer local or remote.

**Conflict handling**: on a merge with unresolved conflicts, the plugin records the list of conflicted files to a vault-root file named by the constant `CONFLICT_OUTPUT_FILE = "conflict-files-obsidian-git.md"` (`src/constants.ts`) and leaves standard git conflict markers (`<<<<<<<`/`=======`/`>>>>>>>`) inside the affected note files for the user to resolve by hand (or via the plugin's built-in diff/merge UI, `git-view`/`split-diff-view`). There's an open feature request (#803, "Conflict Handling to Help with Multi-Device Usage") acknowledging this UX is still rough for real multi-device concurrent editing.

Other defaults worth noting as prior art: `disablePopupsForNoChanges: false` (a Notice fires even for no-op commits unless silenced), `refreshSourceControlTimer: 7000` (7s poll for its own UI status, unrelated to commit cadence), `updateSubmodules: false`, `showStatusBar: true`/`showBranchStatusBar: true` for surfacing sync state to the user.

### B.4 Known corruption/conflict hazards syncing an Obsidian vault via git

- **Raw git conflict markers landing inside note bodies.** The single most-cited hazard (B.3 above) — a merge conflict on a `.md` file leaves `<<<<<<<`/`=======`/`>>>>>>>` text inside the file, which Obsidian will then happily display, edit, and re-save, potentially burying the markers deeper into note content on the next autosave before a human notices.
- **Never run two sync mechanisms against the same vault concurrently.** Multiple sources (Medium "Ditch Obsidian Sync," Stephan Miller's sync guide) converge on this as *the* rule: pointing e.g. both Obsidian's official Sync/iCloud/Syncthing *and* a git-based sync at the same vault folder is "the mistake that eats notes" — two independent writers on different schedules silently clobber each other's versions with no conflict detection between the two systems (since only one of them, if either, understands git). This is directly relevant to Constraint 6 (Headless Sync + obsync coexisting) — the two must be mutually exclusive per-vault, or at minimum the operator must be warned.
- **Config-file (not just note) conflicts** — `.obsidian/*.json` files edited/toggled differently on two devices before a sync produces the same class of conflict as note content but the user is less likely to notice or care about resolving it correctly, so guidance is to keep config files out of git version control on multi-device vaults unless the operator specifically wants shared config (Obsidian forum thread synthesis, B.2).
- **3+ device vaults are specifically called out as more failure-prone** than 2-device setups (search synthesis) — more concurrent writers increase the odds of a race between "device B pulls mid-way through device A's multi-file commit," landing device B in a half-updated working tree until its next pull.
- **Partial/torn reads if git operates on a vault while Obsidian (via ignis or otherwise) is mid-write** — this is the specific hazard this whole research task exists to flag for obsync: since ignis's own writes are non-atomic `fs.promises.writeFile` (Part A.2), a `git add`/`commit` that reads the working tree during that narrow truncate-then-write window can capture and commit a partial/empty file. obsync's read-before-commit pass should apply the same "settle" discipline ignis's own watcher does (300ms stability threshold) before trusting a file's on-disk state.

---

## Sources

**Part A (ignis, cloned repo, file:line):**
- apps/ignis-server/Dockerfile (VOLUME, ENV PUID/PGID/VAULT_ROOT, ENTRYPOINT)
- apps/ignis-server/scripts/entrypoint.sh (chown, gosu)
- apps/ignis-server/docker-compose.yml, apps/docs/src/content/docs/server/deploy.md, apps/docs/src/content/docs/server/environment.md
- apps/ignis-server/server/routes/fs.js
- packages/server-core/src/write-coalescer.js
- packages/shim/src/fs/write-coalescer.js, packages/shim/src/fs/write-durability.js, packages/shim/src/fs/echo-guard.js, packages/shim/src/fs/content-cache.js, packages/shim/src/fs/watcher-client.js
- packages/server-core/src/watcher.js, packages/server-core/src/ws.js
- apps/ignis-server/server/vault-lifecycle.js
- apps/ignis-server/server/plugins/headless-sync/index.js, sync-manager.js
- apps/ignis-server/server/demo-template/.obsidian/*
- docs/ARCHITECTURE.md
- apps/docs/src/content/docs/sync.md, using/limitations.md, roadmap.md
- https://github.com/Nystik-gh/ignis/issues/14 ("Add support for git sync")
- https://github.com/Nystik-gh/ignis/issues/73 (watcher teardown race)
- https://github.com/Nystik-gh/ignis/issues/66, /issues/48, /issues/39 (mtime bugs after sync)

**Part B (Obsidian, web):**
- https://obsidian.md/help/Files+and+folders/How+Obsidian+stores+data
- https://forum.obsidian.md/t/what-should-i-gitignore-for-my-vaults-github-repository/101077
- https://forum.obsidian.md/t/looking-for-an-example-gitignore-file/47351
- https://github.com/trustedsec/Obsidian-Vault-Structure/blob/main/.gitignore
- https://github.com/Vinzent03/obsidian-git/blob/master/src/constants.ts
- https://github.com/Vinzent03/obsidian-git/blob/master/src/automaticsManager.ts
- https://github.com/Vinzent03/obsidian-git/discussions/709 (workspace.json gitignore discussion)
- https://github.com/Vinzent03/obsidian-git/issues/803 (conflict handling feature request)
- https://github.com/Vinzent03/obsidian-git (README, general plugin behavior)
- https://themeansquare.medium.com/ditch-obsidian-sync-build-your-own-auto-sync-system-with-git-and-bash-3b26308a3f4e
- https://www.stephanmiller.com/sync-obsidian-vault-across-devices/
