# TASK

Review the code changes on branch {{BRANCH}} for issue #{{ISSUE_NUMBER}}:
{{ISSUE_TITLE}}

You are an expert code reviewer focused on enhancing code clarity, consistency,
and maintainability while preserving exact functionality — and, in this repo, on
one thing more: obsync is a daemon holding a write-scoped credential that drives
`git` against a directory a human has open in an editor. **The cost of being
wrong is asymmetric.** A missed backup is an inconvenience; a destroyed vault is
unrecoverable. Weight your attention accordingly.

# CONTEXT

Here are the last 10 commits:

<recent-commits>

!`git log -n 10 --format="%H%n%ad%n%B---" --date=short`

</recent-commits>

<issue>

!`gh issue view {{ISSUE_NUMBER}} --comments`

</issue>

<diff-to-main>

!`git diff main...HEAD`

</diff-to-main>

**Read `gh issue view 21 --comments` too, all three parts** — the body is
§1–§6, comment 1 is §7–§12, comment 2 is the testing decisions. It is the parent
spec of every ticket and the spec axis of this review is against it, not against
the ticket alone. `gh issue view 21` without `--comments` gives you a third of
the design and will not tell you so.

# REVIEW PROCESS

## 1. Run the two-axis review

Use the `mattpocock-skills:code-review` skill (`/mattpocock-skills:code-review`)
with:

- **Fixed point:** `main` (this repo's default branch — there is no `master`).
- **Spec source:** issue #{{ISSUE_NUMBER}}, already fetched above, plus the
  sections of #21 it names.
- **Standards sources:** `.sandcastle/CODING_STANDARDS.md` (note the
  `.sandcastle/` prefix — there is no root-level copy) and `CONTEXT.md`.

Skip anything `gofmt`, `go vet` or `golangci-lint` already enforces — the
tooling catches it in the checks below.

## 2. Check the invariants mechanically

These are grep-checkable on purpose. Run them over the diff and over the tree,
and treat a hit outside its one sanctioned home as a finding:

```bash
rg -n '"push".*--force|--force-with-lease|ForceWithLease' -- '*.go'   # must be zero
rg -n '"rebase"|"fsck"|"clone"' -- '*.go'      # clone only in bootstrap
rg -n '"checkout"' -- '*.go'                    # bootstrap only, never after
rg -n 'remote".*set-url|"set-url"' -- '*.go'    # must be zero
rg -n 'os\.Exit|panic\(' -- '*.go'              # never on the sync-loop path
rg -n 'time\.Sleep' -- '*.go'                   # tests must drive the fake clock
rg -n 'bufio\.Scanner|strings\.Split\(.*"\\n"' -- '*.go'  # line-splitting git output
```

Then read for the ones grep cannot see:

- **Human-output parsing.** Every git listing must be `-z`/NUL-separated and
  every status `--porcelain=v2 -z`. A vault path may contain spaces, unicode and
  legally a newline; splitting on `\n` is a bug even when the test passes.
- **Prose deciding behaviour.** git's words may *name* a failure (an auth
  string, a corrupt-object message) so the human's message is specific. Only
  *persistence* may escalate one. A branch taken on a matched string is a
  finding. The one sanctioned split is `push --porcelain`'s documented enum,
  where the parenthetical is relayed verbatim and never branched on.
- **Timeouts.** Network commands 120s; **local git never timed out**. A
  `context.WithTimeout` around a local command is a finding — killing a
  `reset --keep` halfway manufactures the one unrecoverable state in the design.
- **Process groups.** Every git spawned in its own process group, with the kill
  signalling the group, and always `Wait()`ed.
- **Tier assignment.** Every new failure path maps to exactly one of aborted
  run / network freeze / full freeze. Inconclusive degrades to network freeze;
  full freeze is only "even committing would be wrong". A fourth category is a
  spec change.
- **Self-clearing.** No latch may key on process lifetime. If a freeze cannot
  clear by re-checking a gate or by retrying a read-only probe, that is a
  finding.
- **Owned paths.** Anything obsync writes must be inside the declared set —
  `obsync-attention.md`, conflict copies, `.git/info/exclude`, `.git/obsync/`,
  `.git/obsync.lock`, `refs/obsync/failed-apply`. A write outside it is a change
  to the declared surface, not an implementation detail.
- **Write-then-rename** through `.git/obsync/tmp/` for every file obsync writes.
- **Dependencies.** `go.mod` must still list exactly two direct dependencies.
  A third — including a test-only one — needs an argument in the commit message;
  if there isn't one, that is the finding.
- **Knobs.** Nine environment variables, one required. A tenth, or a constant
  promoted to a variable, is a spec change.
- **Vocabulary.** `CONTEXT.md` is binding on identifiers. A synonym in a package,
  type, function, log key or test name is a finding, because the value of a
  glossary is that one word means one thing everywhere.

## 3. Attack the tests

The testing rules here are load-bearing, and a test that breaks them is worse
than no test because it reports confidence it hasn't earned:

- **Any faked git is a finding**, no exceptions. Tests drive real `git` against
  real repos with a `git init --bare` remote over `file://`.
- **Only the clock and the watcher may be injected.** A new interface introduced
  so something can be swapped in a test is a finding.
- **Assertions must be on the world, not the route.** Commits in the bare
  remote, bytes and files in the vault, the attention note's contents, the
  exclude file, the presence of a ref, a subcommand's stdout and exit code.
  Asserting which git commands ran, in what order, or how many times, is a
  finding.
- **`time.Sleep` waiting for obsync is a finding** — the fake clock exists so
  every timing rule is deterministic.
- **Closed tables need row-per-case coverage.** §4's conflict table and §7's
  push disposition table each have every row exercised, the latter driven by a
  real `pre-receive` hook and a real `receive.maxInputSize` on the bare remote.

## 4. Stress-test edge cases

Go beyond the happy path, and prefer this project's real hazards over generic
ones. For each changed path, ask what a **third writer** does to it:

- A file that changes between `status` and `add`, or vanishes entirely.
- A path whose mtime is skewed into the future (NFS/SMB clocks) — does anything
  read as unsettled forever?
- Paths with spaces, unicode, a leading dash, and a newline.
- An empty vault, a vault with `.obsidian/` missing, a `.git` that disappears
  mid-run, an `index.lock` held by someone else.
- A remote with no refs, a remote with refs but not our branch, a remote whose
  tip stopped being a descendant of the tip obsync last saw.
- Zero-length files, a 200MB attachment, a conflict copy that already exists.
- Off-by-one in any count, cap or ceiling — 50 conflicted paths, 5 failure runs,
  50 body lines.

Write tests for anything not already covered. If you can break it, fix it.

## 5. Analyze for code quality

- Reduce unnecessary complexity and nesting; consolidate related logic.
- Prefer **deep modules** — small interface, deep implementation. A wide
  interface whose methods pass straight through is the shape to flag.
- Clear names over clever ones; explicit over compact.
- Remove comments that describe obvious code — but **never remove a comment that
  explains a number**. Every timing constant, threshold and ceiling here has a
  measured or argued reason, and stripping it is how it becomes the next
  person's judgement call.
- Flag any load-bearing documentation line that lost its issue link, and any
  that was cut for brevity — its absence is a defect, not a gap.

## 6. Maintain balance

Avoid over-simplification that would reduce clarity, produce clever code that is
hard to debug, combine unrelated concerns, or remove a helpful abstraction.

## 7. Preserve functionality

Never change what the code does — only how it does it. All original behaviour
must remain intact.

# EXECUTION

The checks are the same ones the implementer ran (see `implement-prompt.md`),
from the repo root:

```bash
go build ./... && go vet ./... && go test ./...
test -z "$(gofmt -l .)" || gofmt -l .
golangci-lint run
go test -race ./...
PATH="$GIT_FLOOR:$PATH" go test ./...       # the git floor point of the matrix
```

And, when the change touches the image, the entrypoint, the healthcheck or
`compose.yaml`:

```bash
docker build -t obsync:dev . && docker run --rm --user 4242:4242 obsync:dev status
docker compose -f compose.yaml config
```

`implement-prompt.md`'s **SANDBOX ENVIRONMENT** section applies to you too —
read it rather than probing. In short: Go, `golangci-lint`, **two gits** — the
shipped image's version as plain `git`, and the 2.38 floor at `$GIT_FLOOR` —
plus `gh`, `rg` and a working Docker socket; node, `uv` and `pre-commit` are
not present; there is no root; and containers you start are network-isolated
from this one.

1. Run the checks first to confirm the current state passes
2. Run the invariant greps in step 2
3. Attempt to break the implementation with new tests — if you succeed, fix it
4. Make code quality improvements directly on this branch
5. Run the checks again, including `-race` and the floor point. They must be
   green before you commit. **Never weaken a lint rule or an assertion to go
   green**; loosen only when the intended contract genuinely changed, and say so
   in the commit message
6. Commit with a message starting with `RALPH: Review -` describing the
   refinements, and calling out any invariant or surface issue you found

If the code is already clean, well-tested and handles edge cases properly, do
nothing.

Once complete, output <promise>COMPLETE</promise>.
