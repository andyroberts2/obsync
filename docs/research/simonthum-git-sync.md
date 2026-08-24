# Research: `simonthum/git-sync` as prior art for obsync's PUSH half

- Repo: https://github.com/simonthum/git-sync
- Cloned commit: `b991840c5533c010adda58015de44ba36d6bc1d0` (2025-09-05)
- Local clone: `/tmp/claude-1000/-home-andy-code-obsync/3b655fa3-cf38-4da8-9581-1d14c7cc0c42/scratchpad/git-sync`
- Main script: `git-sync` (439 lines, pure bash)
- Licence: **CC0 1.0** (public domain dedication) — no attribution required, fully safe to lift logic or even code verbatim.
- Activity: first commit 2012, most recent commit **2025-09-05**. Actively maintained by the original author (simonthum), low-traffic but responsive (all 16 issues ever filed are closed; no open issues at time of research). This is a mature, stable, "done" utility rather than a fast-moving project.

All line numbers below refer to the `git-sync` file at the commit above.

---

## Lessons for obsync (read this first)

### Steal these rules directly

1. **Sync is opt-in per branch, with a global fallback.** `branch.<name>.sync` overrides `git-sync.syncEnabled` (lines 110–126). obsync should have an explicit "sync enabled" flag rather than syncing any repo it finds — prevents accidental sync of a vault that isn't ready (e.g. mid-initial-import).
2. **New/untracked files are a *hard stop* by default**, not something silently committed (lines 92–108, 204–217). This is the single most important safety rule in the script: a script that auto-adds everything it finds risks committing secrets, OS cruft, or half-written files. obsync should default to "only commit modified+deleted paths that are already tracked" and require an explicit `syncNewFiles`-equivalent opt-in to `git add -A` new files. For an Obsidian vault this matters a lot — new note creation is the common case, so obsync likely *does* want `syncNewFiles=true` by default, but the flag should exist and be visible/toggleable.
3. **Refuse to run in any non-clean repo state** — mid-rebase, mid-merge, mid-cherry-pick, mid-bisect, detached HEAD, no configured remote (lines 157–201, 274–331). Never try to "fix" this automatically; just stop and report. obsync's daemon loop should treat "repo not in a clean, sync-ready state" as a distinct health condition it surfaces (e.g. to a status file or the UI) rather than retrying blindly.
4. **Fast-forward when possible; only rebase when actually diverged; never force-push.** Decision is made from `git rev-list --count --left-right` ahead/behind counts (lines 229–255) — cheap, purely local after fetch, four-way branch (equal/ahead/behind/diverged). obsync should reuse exactly this ahead/behind classification instead of always doing a merge or always doing a rebase.
5. **On rebase failure, stop with a non-zero exit and an actionable message — do NOT attempt to resolve, and do NOT abort the rebase for the user** (lines 426–436). It leaves the repo in the conflicted `REBASE-m`/`REBASE-i` state and tells the human what to do. This is the most important non-obvious safety property for an *unattended* daemon: it means every subsequent run will hit the "unsafe repo state" gate (line 275/281) and refuse to touch anything further, rather than compounding the problem. obsync must replicate this "detect conflict → freeze → alert" behaviour; for a headless sidecar with no human watching a terminal, obsync additionally needs to surface this state somewhere a human *will* see it (health endpoint, log alert, ntfy/webhook) since git-sync's model assumes an interactive user will eventually run `git status` and notice.
6. **Re-verify sync state after every mutating step**, don't just trust the return code (lines 382–386 post-commit re-check; `exit_assuming_sync` at 258–267 post-push/merge/rebase re-check). Cheap insurance against partial failures.
7. **Distinguish "check" from "sync"** (lines 28–34, 334–358): a read-only precondition probe with its own exit code, separate from the mutating run. Very useful for a daemon: obsync can poll "is it safe to sync" cheaply/frequently and only invoke the mutating path when it actually needs to, and can expose "check" as a health probe.
8. **Distinct, meaningful exit codes** (0 sync'd/nothing to do, 1 needs manual intervention e.g. new files/conflict, 2 unsafe repo state/config problem, 3 likely network/transient, 100 bad mode, 128 not a git repo). obsync's own process (or the container's healthcheck) should key off similarly distinct codes/states rather than a single boolean success/fail, so a systemd/Docker restart policy can tell "retry me" (network) apart from "a human must intervene" (conflict) apart from "misconfigured" (no remote).
9. **Commit message records provenance**: default `"changes from $(uname -n) on $(date)"` (line 37), overridable via `syncCommitMsg` (line 372). For obsync's Docker sidecar, stamp the container/host name and maybe the vault path so commits are traceable to "which client wrote this" when multiple devices sync the same repo.
10. **Hooks are honoured by default, with an explicit `--no-verify` opt-out** (`syncSkipHooks`, lines 139–154). obsync should let pre-commit hooks run unless a config explicitly disables them — useful if a user wants e.g. a markdown linter or secret-scanner hook to gate auto-commits.
11. **`autocommitscript` escape hatch** (line 363): let power users override the entire commit step with an arbitrary command. Cheap to add, high flexibility, and it's how a user could plug in something like `git add -A -- ':!.obsidian/workspace.json'` without obsync needing to hardcode Obsidian exclusion logic itself.

### Gaps in git-sync obsync must fill itself (this is prior art for PUSH only)

- **No file watching, debouncing, or scheduling in the core script at all.** All of that lives in optional `contrib/` wrapper scripts (`git-sync-on-inotify`, `git-sync-on-fswatch`, a `modd.conf` snippet) that just call `git-sync` in a loop on a timer or on fs events, with **no debounce** — a rapid burst of saves (e.g. Obsidian's autosave-on-every-keystroke behaviour, or its atomic-rename writes) can trigger one `git-sync` invocation per file event or mid-write (contrib scripts, and confirmed as a known, *unfixed* limitation in issue #23 — "git-sync-on-inotify can let certain changes slip through," closed by punting to `modd`/documentation rather than a real fix). obsync needs real debounce (batch fs events over e.g. 1–5s of quiet) — this is the single biggest thing to build that git-sync doesn't provide.
- **No locking against concurrent invocations.** Nothing in the script takes a lockfile/flock; if the wrapper's timer and an fs-event trigger overlap, or if two `git-sync` processes run concurrently, behaviour is whatever plain concurrent `git` operations do (races, "already exists" lock errors from git's own `.git/index.lock`, or worse). obsync's daemon must serialize sync runs itself (single worker loop / mutex / flock).
- **No retry/backoff for network failures.** A `git fetch`/`git push` failure just exits 3 with "likely a network problem, try again" (lines 393–396, 412–414) — the *next* scheduled invocation is the retry mechanism, with no backoff. Fine for a cron-ish wrapper, insufficient for an always-on daemon that should back off (and stop hammering) on sustained outage, and should re-check network/auth status distinctly.
- **No auth handling whatsoever.** Assumes `git fetch`/`git push` just work — i.e. SSH agent or a credential helper is already configured in the environment. obsync (Docker sidecar) must own credential provisioning explicitly: SSH key or PAT mounted/injected, `GIT_SSH_COMMAND`/credential helper configured, and clear error surfacing when auth fails (git-sync would just report a generic "likely network problem").
- **No Git LFS awareness.** Not mentioned anywhere in script or README. Irrelevant for an all-markdown vault, but if obsync's vault ever contains large binary attachments a user has enabled LFS for, obsync needs its own handling — git-sync has none to borrow.
- **No `.gitignore` authoring/management** — it relies entirely on whatever `.gitignore` already exists in the repo; it only *reads* ignore status indirectly via `git status --porcelain` semantics. obsync will need to ship or manage a sensible default `.gitignore` for Obsidian vaults itself (see hazards section below) since git-sync provides no help here beyond "respect whatever the repo's `.gitignore` says."
- **No conflict *resolution*, only conflict *detection and freeze*.** By design (see gate 5 above) — worth restating as a gap: if obsync wants any smarter-than-"stop and alert a human" behaviour for markdown-specific merges (e.g. line-based auto-resolution heuristics, or accepting "ours"/"theirs" for `.obsidian/workspace.json`), that logic must be added; git-sync intentionally does not attempt it.
- **No Obsidian-specific awareness at all** — it's a generic tracking-repo tool with zero knowledge of Obsidian's file layout, `.trash/`, plugin config churn, or atomic-save patterns (see section 6 below). All of that is obsync's job.

---

## 1. The sync algorithm — control flow, in order

Overall flow as stated in the README "How does it work?" section, then verified line-by-line against the script:

1. **Sanity checks** — repo must exist and be in a state considered safe (not mid-rebase/merge/cherry-pick/bisect, not detached HEAD).
2. **Determine branch and remote**, and check the branch is enlisted for sync (`.sync`/`.syncEnabled`).
3. **Check initial file state** — bail if there are new (untracked) files, unless `syncNewFiles` is set. If mode is `check`, exit here.
4. **Auto-commit** any local (tracked, modified/deleted) changes, using either a custom `autocommitscript` or the built-in `git add -u`/`git add -A` + `git commit`.
5. **Re-check repo cleanliness** post-commit — bail if the auto-commit didn't leave a clean tree.
6. **Fetch** from the remote for the branch.
7. **Classify sync state** (`equal`/`ahead`/`behind`/`diverged`) via `git rev-list --count --left-right`, then act:
   - `equal` → nothing to do.
   - `ahead` → `git push`.
   - `behind` → `git merge --ff --ff-only` (fast-forward only, never a merge commit).
   - `diverged` → `git rebase`, and if that succeeds cleanly, `git push`.
8. **Final assertion** — re-derive sync state and exit 0 only if it is now `equal`.

Key blocks, quoted:

**Entry sanity gate** (lines 273–283):
```
273  # first some sanity checks
274  rstate="$(git_repo_state)"
275  if [[ -z "$rstate" || "|DIRTY" = "$rstate" ]]; then
276      __log_msg "Preparing. Repo in $(__gitdir)"
277  elif [[ "NOGIT" = "$rstate" ]] ; then
278      __log_msg "No git repository detected. Exiting."
279      exit 128 # matches git's error code
280  else
281      __log_msg "Git repo state considered unsafe for sync: $(git_repo_state)"
282      exit 2
283  fi
```

**Branch/remote resolution and the sync-enabled gate** (lines 285–331) — determines `branch_name` from `git symbolic-ref -q HEAD`, resolves `remote_name` from `pushRemote` → `remote.pushDefault` → `branch.<name>.remote` in that priority order, then exits 2 with instructions if no remote is configured, or exit 1 with instructions if the branch isn't enlisted via `.sync`.

**New-file gate + check-mode early exit** (lines 347–358):
```
347  # check for intentionally unhandled file states
348  if [ ! -z "$(check_initial_file_state)" ] ; then
349      __log_msg "There are changed files you should probably handle manually."
350      git status
351      exit 1
352  fi
353
354  # if in check mode, this is all we need to know
355  if [ $mode == "check" ] ; then
356      __log_msg "check OK; sync may start."
357      exit 0
358  fi
```

**Auto-commit block** (lines 360–387) — builds and `eval`s the commit command, then re-verifies the tree is clean afterward (see full quote in section 4 below).

**Fetch + four-way dispatch** (lines 389–439) — the heart of the algorithm; quoted in full in section 3 below.

---

## 2. Preconditions & safety gates

Everything the script refuses to do, and why, in the order it checks them:

| Gate | Location | Condition to proceed | Failure behaviour |
|---|---|---|---|
| Repo state sane | lines 157–201 (`git_repo_state`), 274–283 | Not mid `REBASE-i`/`REBASE-m`/`AM/REBASE`/`MERGING`/`CHERRY-PICKING`/`BISECTING`; not bare/inside-git-dir | `NOGIT` → exit 128 (no repo). Any unsafe state → exit 2 with the specific state name printed |
| On a branch | lines 285–293 | `git symbolic-ref -q HEAD` resolves (not detached) | exit 2, prints `git status`, "Syncing is only possible on a branch." |
| Remote configured | lines 295–314 | One of `branch.<name>.pushRemote`, `remote.pushDefault`, `branch.<name>.remote` is set | exit 2 with copy-pasteable `git branch --set-upstream-to=...` hint |
| **Branch enlisted for sync** | lines 110–126, 317–331 | `branch.<name>.sync` (overrides) or `git-sync.syncEnabled` is `true`, or `-s` flag passed | exit 1 with copy-pasteable `git config --bool branch.<name>.sync true` hint. **Deliberate opt-in per branch** — nothing syncs by default |
| Mode valid | lines 333–341 | `$1` is empty, `sync`, or `check` | exit 100, "Mode $1 not recognized" |
| **No untracked/new files (unless allowed)** | lines 92–108, 204–217, 347–352 | `syncNewFiles` false (default): status must contain *only* modified (`M`) entries — anything else (untracked `??`, added `A`, etc.) blocks. `syncNewFiles` true: only truly *unmatched* patterns block | exit 1, prints `git status`, "There are changed files you should probably handle manually." **This is the main deliberate "refuse to guess" gate** — new files are ambiguous (could be secrets, scratch files, partial saves) so the script won't auto-add them without explicit opt-in |
| Auto-commit leaves clean tree | lines 380–387 | After running the commit command, `git_repo_state` must be empty again | exit 1, "Auto-commit left uncommitted changes. Please add or remove them as desired and retry." (guards against a custom `autocommitscript` or hook that partially fails) |
| Fetch succeeds | lines 391–396 | `git fetch` exit code 0 | exit 3, "likely a network problem" |
| Fast-forward-only merge succeeds | lines 416–424 | `git merge --ff --ff-only` exit code 0 (this can only fail if the "behind" classification was somehow wrong, since a true fast-forward can't conflict) | exit 2 |
| Rebase succeeds cleanly | lines 426–436 | `git rebase` exit 0 **and** repo state empty **and** post-rebase state is `ahead` | exit 1, "Rebasing failed, likely there are conflicting changes. Resolve them and finish the rebase before repeating git-sync." — **leaves the conflicted rebase in place**, does not abort it |
| Push succeeds | lines 407–414, 431 | `git push` exit 0 | exit 3, "likely a connection failure" |
| Final state is `equal` | lines 258–267 (`exit_assuming_sync`), called at 404, 410, 420, 432 | ahead/behind count between local and remote is `0 0` | exit 3, "Synchronization FAILED! You should definitely check your repository carefully!" |

**Config knobs, precisely:**

- `branch.<name>.sync` / `git-sync.syncEnabled` (bool) — master per-branch/global opt-in gate (lines 110–126). Branch-specific wins over global. CLI `-s` forces true regardless.
- `branch.<name>.syncNewFiles` / `git-sync.syncNewFiles` (bool) — whether untracked files are swept into the commit (`git add -A`) vs. ignored entirely (`git add -u`) and treated as a blocking condition (lines 92–108, 129–136). Branch-specific wins. CLI `-n` forces true.
- `branch.<name>.syncSkipHooks` / `git-sync.syncSkipHooks` (bool) — whether the commit step passes `--no-verify` (lines 139–154). Default false, i.e. **hooks run by default**.
- `branch.<name>.syncCommitMsg` (string) — replaces `DEFAULT_AUTOCOMMIT_MSG` (line 372–375).
- `branch.<name>.autocommitscript` (string) — full override for the add+commit step, `eval`'d verbatim (line 363–370). If set, none of the built-in add/commit logic runs at all — the user's script is fully responsible for leaving a clean tree.

**`.gitignore` handling**: git-sync does not manage `.gitignore` itself — it only relies on `git status --porcelain`'s classification, which already excludes ignored files. In other words, anything the repo's own `.gitignore` excludes is invisible to git-sync's new-file gate and to auto-commit; git-sync assumes a correctly maintained `.gitignore` is the user's job.

---

## 3. Divergence handling

Decision is made purely from a local, already-fetched ahead/behind count — no network calls after fetch (lines 229–255):

```
228  # determine sync state of repository, i.e. how the remote relates to our HEAD
229  sync_state()
230  {
231      local count="$(git rev-list --count --left-right $remote_name/$branch_name...HEAD)"
232
233      case "$count" in
234          "") # no upstream
235              echo "noUpstream"
236              false
237              ;;
238          "0	0")
239              echo "equal"
240              true
241              ;;
242          "0	"*)
243              echo "ahead"
244              true
245              ;;
246          *"	0")
247              echo "behind"
248              true
249              ;;
250          *)
251              echo "diverged"
252              true
253              ;;
254      esac
255  }
```

Dispatch, after `git fetch` (lines 398–439, quoted in full since it's the crux of the design):

```
398  case "$(sync_state)" in
399  "noUpstream")
400          __log_msg "Strange state, you're on your own. Good luck."
401          exit 2
402          ;;
403  "equal")
404          exit_assuming_sync
405          ;;
406  "ahead")
407          __log_msg "Pushing changes..."
408          git push $remote_name $branch_name:$branch_name
409          if [ $? == 0 ]; then
410              exit_assuming_sync
411          else
412              __log_msg "git push returned non-zero. Likely a connection failure."
413              exit 3
414          fi
415          ;;
416  "behind")
417          __log_msg "We are behind, fast-forwarding..."
418          git merge --ff --ff-only $remote_name/$branch_name
419          if [ $? == 0 ]; then
420              exit_assuming_sync
421          else
422              __log_msg "git merge --ff --ff-only returned non-zero ($?). Exiting."
423              exit 2
424          fi
425          ;;
426  "diverged")
427          __log_msg "We have diverged. Trying to rebase..."
428          git rebase $remote_name/$branch_name
429          if [[ $? == 0 && -z "$(git_repo_state)" && "ahead" == "$(sync_state)" ]] ; then
430              __log_msg "Rebasing went fine, pushing..."
431              git push $remote_name $branch_name:$branch_name
432              exit_assuming_sync
433          else
434              __log_msg "Rebasing failed, likely there are conflicting changes. Resolve them and finish the rebase before repeating git-sync."
435              exit 1
436          fi
437          # TODO: save master, if rebasing fails, make a branch of old master
438          ;;
439  esac
```

**Answers:**

- **Merge vs. rebase vs. bail**: never a "real" (non-fast-forward) merge. Only three outcomes: pure fast-forward merge (`behind`), rebase-then-push (`diverged`), or plain push (`ahead`). It picks purely off the ahead/behind counts — no heuristics, no "try merge first."
- **On rebase conflict**: `git rebase` fails (or the working tree ends up not clean, or the resulting state isn't `ahead`), and the script **exits 1 without touching the rebase further** — no `git rebase --abort`. The repo is deliberately left mid-rebase (in `REBASE-m`/`REBASE-i` state) for a human to finish (`git rebase --continue`/`--abort` themselves). The only cleanup gesture is a `# TODO` comment at line 437 noting the author intended, but never implemented, saving a branch pointer to the pre-rebase `master` for extra safety.
- **This means the very next invocation will hit gate 1** (`git_repo_state` returns `REBASE-m`/`REBASE-i`, lines 161–164, 274–283) and refuse to run at all — so a stuck rebase self-latches into "sync frozen until a human fixes it," rather than the daemon retrying and making things worse. This is a deliberate and valuable safety property for an unattended sync loop, but it also means (for obsync) that a headless container has no path to interactively finish a rebase — obsync needs a way to surface "sync is frozen, SSH in / open a UI and resolve" that git-sync doesn't need because a human is normally sitting at the terminal that ran it.
- **`noUpstream`** (i.e. `git rev-list ... --left-right` returned nothing, e.g. the remote branch doesn't exist yet) is *not* handled by trying an initial push — it just gives up with "Strange state, you're on your own." (exit 2). GitHub issue #24 flags this as a possible gap (first sync to a brand-new empty remote branch isn't bootstrapped automatically) — worth deciding explicitly for obsync (first-run bootstrap onto an empty repo is a very plausible scenario for a fresh vault).

---

## 4. Commit strategy

**What gets committed and when** — only entered if `local_changes()` (lines 219–226, tracked modifications *or* untracked files depending on flag) is non-empty:

```
360  # check if we have to commit local changes, if yes, do so
361  if [ ! -z "$(local_changes)" ]; then
362      autocommit_cmd=""
363      config_autocommit_cmd="$(git config --get branch.$branch_name.autocommitscript)"
364
365      # discern the three ways to auto-commit
366      if [ ! -z "$config_autocommit_cmd" ]; then
367          autocommit_cmd="$config_autocommit_cmd"
368      else
369          autocommit_cmd="$(__gitadd); $(__gitcommit);"
370      fi
371
372      commit_msg="$(git config --get branch.$branch_name.syncCommitMsg)"
373      if [ "" == "$commit_msg" ]; then
374        commit_msg=${DEFAULT_AUTOCOMMIT_MSG}
375      fi
376      autocommit_cmd=$(echo "$autocommit_cmd" | sed "s/%message/$commit_msg/")
377
378      __log_msg "Committing local changes using ${autocommit_cmd}"
379      eval $autocommit_cmd
380
381      # after autocommit, we should be clean
382      rstate="$(git_repo_state)"
383      if [[ ! -z "$rstate" ]]; then
384          __log_msg "Auto-commit left uncommitted changes. Please add or remove them as desired and retry."
385          exit 1
386      fi
387  fi
```

- **Batching**: yes, implicitly — it's one commit per sync run covering *all* pending changes at that moment (`git add -u` or `-A` stages everything matching, then a single `git commit`). It does not commit per-file or debounce within a run; batching granularity is entirely a function of how often the outer loop/wrapper invokes `git-sync`.
- **Add scope**: `git add -u` (tracked modified/deleted only) by default (line 129–136, `__gitadd`), or `git add -A` (tracked + new) if `syncNewFiles` is true.
- **Commit message format**: `"changes from $(uname -n) on $(date)"` by default (line 37), fully overridable per-branch via `syncCommitMsg` (line 372–375). No structured/templated format beyond that single string — no per-file listing, no diff stat.
- **Hooks**: run normally unless `syncSkipHooks` adds `--no-verify` (lines 139–154).
- **Deletions**: handled naturally — `git add -u` stages deletions of tracked files (it's the standard git semantics of `-u`), so deleted vault notes get committed as deletions with no special-casing in the script.
- **Renames**: no special handling — a rename is just an add-then-delete under the hood; `git add -u`/`-A` stage both halves and git's own commit-time rename detection kicks in when diffing, but the script does nothing to keep a rename atomic across two watched fs events (relevant to the "no debounce" gap above — a rename that a watcher reports as separate delete+create events could, without debouncing, split across two separate `git-sync` invocations/commits).
- **Custom escape hatch**: `autocommitscript` bypasses all of the above — the operator's arbitrary command must itself leave a clean tree (checked at line 382–386) or the whole run fails safe.

---

## 5. What it does NOT handle (gaps relevant to an always-on daemon)

- **No file watching / event source** in the core script — that's 100% delegated to the optional `contrib/` wrappers (`git-sync-on-inotify`, `git-sync-on-fswatch`, `modd.conf`), which are thin loops that just call `git-sync` on a timer and/or on fs events.
- **No debounce.** The inotify/fswatch wrappers fire (or at best coalesce via a single blocking `--one-event`/`-t` wait) essentially per fs event, with a fallback poll interval (`GIT_SYNC_INTERVAL`, default 500s for inotify / 60s for fswatch). A burst of rapid saves can trigger back-to-back `git-sync` runs. GitHub issue #23 ("git-sync-on-inotify can let certain changes slip through") documents this as a known, unresolved limitation — the maintainer's answer was to document it and suggest switching to `modd` rather than fix it in git-sync itself.
- **No scheduling logic** beyond "loop with a timeout" in the wrappers; no cron/systemd-timer integration shipped (issue #12, "Perpetual mode?", was closed by the maintainer directing users to wire up their own systemd unit/loop rather than adding a daemon mode to git-sync).
- **No retry/backoff.** A failed fetch/push just exits 3; the next external invocation (whenever that is) is the only retry mechanism. No exponential backoff, no distinguishing "just retried and failed again" from a fresh attempt.
- **No locking against concurrent runs.** Nothing takes a lockfile or `flock`; concurrent invocations (e.g. a timer tick overlapping an fs-event trigger) are not guarded against anywhere in the script or the wrappers.
- **No Git LFS support/awareness** anywhere in script, README, or contrib.
- **No auth/credential handling.** Entirely delegates to the ambient git credential helper / SSH agent; failures surface only as a generic "likely a network problem" (line 394/412), not distinguished from an actual auth failure.
- **No conflict resolution**, by design — see section 3. It detects and freezes, never resolves.
- **No `.gitignore` scaffolding or management.**
- **No handling of a brand-new/empty remote branch** (`noUpstream` just gives up, see section 3 and issue #24).
- **Single-branch only** — operates strictly on the current branch's relationship to its one configured remote; no multi-remote, no multi-branch fan-out.
- **No structured logging/telemetry** beyond `echo`-based `__log_msg` lines to stdout; no machine-readable status output for something like a health check (though the distinct exit codes partially substitute for this).

---

## 6. Obsidian/vault-specific hazards — coverage

**None of these are addressed by git-sync at all** — it is a fully generic "any tracking repo of text files" tool with zero Obsidian awareness. Specifically:

- **`.obsidian/` config churn** (workspace layout, plugin toggles, recently-used lists) — not filtered, not ignored, not treated specially. If tracked, every UI interaction that touches these files becomes a commit-worthy change, indistinguishable from real note edits.
- **`workspace.json` thrash** (rewritten on almost every pane focus/resize/tab switch in Obsidian) — same as above; git-sync has no concept of "noisy, low-value file" to exclude or de-prioritize. This alone could make git-sync auto-commit constantly if `.obsidian/workspace.json` is tracked and not gitignored.
- **`.trash/`** (Obsidian's local trash for deleted notes, when "system trash" isn't used) — not recognized; treated as ordinary tracked/untracked files like anything else. Whether trashed notes get committed (as adds) or block sync (as untracked, if `syncNewFiles` is off) depends purely on generic status classification, not any deletion-aware logic.
- **Plugin data files** (`.obsidian/plugins/*/data.json`, caches, `.obsidian/workspace-mobile.json`, etc.) — no special casing.
- **Atomic-rename saves** (many editors, possibly Obsidian's sync engine internals, write to a temp file then rename over the target to avoid partial writes) — git-sync has no awareness of this pattern. Combined with the "no debounce" gap, a watcher-triggered wrapper could in principle catch a repo mid-write if the temp-file-then-rename isn't atomic at the filesystem level the watcher observes, though `git status`/`git diff` operate on the resulting file content at commit time so a *fully completed* atomic rename before the commit step runs is safe — the real risk is committing at a moment between an old file's deletion event and its replacement's creation event.
- **Partial writes** — same caveat: git-sync's own commit step only ever sees whatever is on disk at the moment `git add`/`git commit` execute; it has no fsync/quiescence check, so a commit is only as safe as "was the app done writing when this ran" — which is exactly why real debounce (waiting for a quiet period) matters and is absent here.

**Conclusion**: all Obsidian-specific handling — recommended `.gitignore` entries for `.obsidian/workspace*.json` and `.trash/` (or a deliberate decision to track them), debounce/quiescence before committing, and any special deletion/rename semantics for vault notes — is 100% obsync's responsibility to design. git-sync's contribution is entirely the *generic git safety envelope* (sections 1–5 above), not vault semantics.
