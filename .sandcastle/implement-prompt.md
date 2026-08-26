# TASK

Build issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

Pull in the issue with `gh issue view {{ISSUE_NUMBER}} --comments`.

Only work on the issue specified.

Work on branch {{BRANCH}}. Make commits, run the checks below, and close the
issue when done.

# THE SPEC

**Read `gh issue view 21 --comments` before you touch anything, and read all
three parts of it.** Issue #21 is obsync's whole design and it is the parent of
every work ticket. GitHub caps an issue body at 64KB, so the spec is split:

- the **body** — Problem Statement, Solution, User Stories, Implementation
  Decisions §1–§6 (runtime · the sync loop · the branch · conflicts · what is
  tracked · torn writes)
- **comment 1** — §7–§12 (safety interlocks · configuration and credentials ·
  signal · the declared surface · documentation · build and release)
- **comment 2** — Testing Decisions · Out of Scope · Further Notes

`gh issue view 21` alone gives you a third of the design and it will not tell
you that. Your ticket names the sections it implements; those sections are
authoritative over your own judgement about what is reasonable, because
essentially every number in them was measured or argued for rather than picked.

The spec links nineteen decision tickets. **Do not read them.** Nine of them
amend an earlier one and the spec is the later word — reading #9 alone builds
eight gates and a latch; reading #11 alone builds a credential cache daemon that
#12 deleted. The links are archaeology.

# CONTEXT

Here are the last 10 commits:

<recent-commits>

!`git log -n 10 --format="%H%n%ad%n%B---" --date=short`

</recent-commits>

# EXPLORATION

Explore the repo and fill your context window with what you need. obsync is
**one Go module and one container image** — a Docker sidecar that keeps a
mounted Obsidian vault in step with one remote git branch, driving `git` as a
subprocess and never through a library binding.

Before touching code, read:

- **`CONTEXT.md`** — the glossary, and it is binding on identifiers, not just
  prose. It also lists the words each term replaces. Use its terms; a synonym in
  a package or function name is a defect.
- **`.sandcastle/CODING_STANDARDS.md`** — the invariants, the two-dependency
  rule, the subprocess rules, the tier model, and the testing rules. Note the
  `.sandcastle/` prefix; there is no root-level copy.
- **`README.md`** — including its never-list once it exists.
- **`docs/research/`** — prior art and measurements taken before any decision
  was made: `kubernetes-git-sync.md`, `simonthum-git-sync.md`,
  `ignis-and-obsidian-vaults.md`, `git-remote-size-limits.md`. The last one's
  local half was verified through `$GIT_QUARANTINE_PATH` on a bare repo, which
  is exactly the test seam you will be writing against.
- Whatever already exists of `docs/interface.md`, `docs/operations.md`,
  `docs/credentials.md` and the root `compose.yaml`.

Read the existing tests before writing one. Early in the project there may be
none — in which case the prior art is the **measurements recorded in the spec**,
several of which are stated with their observed output and each of which is a
test waiting to be written.

# THE RULES THAT ARE NOT NEGOTIABLE

Your ticket may not restate these. `CODING_STANDARDS.md` has them in full.

1. **Never fake git.** Tests drive real `git` against real repos, with a
   `git init --bare` remote over `file://`. A fake tests obsync's beliefs about
   git, and those beliefs are the entire risk surface.
2. **Only the clock and the watcher are injectable.** Do not introduce a
   `GitRunner` interface so git can be swapped. Every timing rule is tested by
   driving the fake clock, never by sleeping.
3. **Test observable behaviour** — the state of the vault, the bare remote, the
   attention note, the exclude file, a ref, a subcommand's stdout and exit code.
   Never which git commands ran or in what order.
4. **Stdlib plus exactly two direct dependencies** (a filesystem-notification
   library, and `golang.org/x/sys`). A third is *argued for* in the commit
   message, not slipped in. This includes test-only dependencies.
5. **Never parse git output meant for humans.** `--porcelain=v2 -z`, `-z`
   everywhere, `LC_ALL=C`, `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`,
   `GIT_CONFIG_NOSYSTEM=1` pinned per invocation. Vault paths contain spaces,
   unicode, and may legally contain a newline.
6. **Timeouts are network-only, at 120s. Local git is never timed out.**
7. **No new knobs.** Nine environment variables, one required. A tenth is a spec
   change, not an implementation detail.
8. **The never-list holds** — no force-push, no rebase, no `checkout` after
   bootstrap, no writing the vault's `.git/config`, no `remote set-url`, no
   re-clone, no self-repair, no `fsck`, no discarding history, no exiting on a
   sync failure. Write them so `rg` can check them.

If your ticket appears to require breaking one of these, it does not — re-read
the spec section, and if it genuinely conflicts, say so in the issue comment
rather than choosing.

# EXECUTION

Use the `mattpocock-skills:implement` skill (`/mattpocock-skills:implement`) and
follow it, with the **ticket body as the spec and §21 as the design**. It drives
`/tdd` at pre-agreed seams — agree those seams against the two this project has,
and no third:

- **Seam 1:** obsync's process boundary, with real git underneath. A real vault
  directory, a real bare remote over `file://`, an injected clock and watcher,
  and the loop driven end to end. Everything behavioural goes here.
- **Seam 2:** the container image, run at `--user 4242:4242`. The only seam that
  can see the no-`/etc/passwd` claim, the git-floor gate, the `HEALTHCHECK`
  wiring and the credential re-read.

**One exception to that skill:** skip its final `/code-review` step. A dedicated
reviewer agent runs against this branch immediately after you finish — see
`review-prompt.md`.

# SANDBOX ENVIRONMENT

You are in a container built from `.sandcastle/Containerfile`. Don't spend turns
probing for tooling — this is what is here, and what isn't:

- **Present:** the **Go toolchain** (`go`, `gofmt`), `golangci-lint`, **two real
  gits** (see below), `gh`, `jq`, `rg`, `inotifywait`, `ps`, the Docker CLI +
  Compose and Buildx plugins.
- **Absent, deliberately:** node, npm, `uv`, `pre-commit`, Playwright, Postgres,
  Redis. This repo has no JavaScript, no Python project and no database. If you
  find yourself wanting one, you are off the map.
- **No root.** There is no sudo and no `apt-get`. If something is genuinely
  missing, note it in your commit message for the next iteration rather than
  working around it — the fix belongs in `.sandcastle/Containerfile`.
- `GOTOOLCHAIN=local` is set, which is not incidental: §12 forbids a `toolchain`
  directive in `go.mod` because it makes a pinned builder not pinned. A stray
  one fails here rather than in the release build.

**Two gits, because the matrix has two points and both live here.** Neither
comes from apt — Debian's own git is 2.39.5, which is neither point, and it is
shadowed on PATH so a test can never accidentally run against it.

- **`git`** is `/opt/git/current/bin/git` — the same version the shipped image
  carries (alpine 3.23's), and the one the spec's measurements were taken on.
  It tests the **product**.
- **`$GIT_FLOOR`** (`/opt/git/floor/bin`) is git **2.38**, the oldest obsync
  promises to work on, fixed there by `merge-tree --write-tree`. It tests the
  **promise**.

Anything touching git plumbing must pass at both:

```bash
git --version                             # the current point
PATH="$GIT_FLOOR:$PATH" git --version     # the floor — confirm you got it
PATH="$GIT_FLOOR:$PATH" go test ./...
```

In CI the current point is **derived** from the base image
(`docker run --rm alpine:3.23 git --version`) rather than declared, so it
follows the digest bump automatically. In this image it is a pinned ARG; if the
two disagree, the Containerfile is stale and that is worth a note in your commit
message.

**The Docker socket is a real one** (the host's rootless Podman, Docker-API-
compatible), so `docker build`, `docker run` and `docker compose` all work —
which is what makes seam 2 runnable. But **containers you start are
network-isolated from this one**: there is no route to them by IP, published
ports are dead from in here, and `--network host` is unavailable. That costs
obsync nothing, because its remote in tests is a `file://` bare repo on a bind
mount and never a network service. Reach a container you started by execing into
it or by attaching to the same Docker network — not by its address.

# FEEDBACK LOOPS

**All must be green before every commit.** Run from the repo root.

```bash
go build ./...
go vet ./...
go test ./...
test -z "$(gofmt -l .)" || gofmt -l .
golangci-lint run

# The race detector. obsync has one serialized loop and a watcher feeding a
# channel; that is a small enough surface that -race should always be clean.
go test -race ./...

# The git floor point of the matrix. Required for anything touching plumbing.
PATH="$GIT_FLOOR:$PATH" go test ./...
```

Tests drive real git and real filesystems, so they are slower than pure-Go unit
tests but nowhere near a browser suite. Run targeted packages while iterating
(`go test ./internal/<pkg>`) and the full suite plus `-race` plus the floor
point before committing.

**Seam 2, when your ticket touches the image, the entrypoint, the healthcheck or
`compose.yaml`:**

```bash
docker build -t obsync:dev .
docker run --rm --user 4242:4242 obsync:dev status   # no /etc/passwd entry
docker compose -f compose.yaml config                # the reference stack parses
```

**Never weaken a lint rule, an assertion, or a test to go green.** Loosen one
only when the intended contract genuinely changed, and say so in the commit
message.

# COMMIT

Commit normally. There is no `pre-commit` config in this repo — `gofmt`,
`go vet` and `golangci-lint` above are the gate, and they are yours to run.

The commit message must:

1. Start with `RALPH:` prefix
2. Include the task completed + the issue reference (`#{{ISSUE_NUMBER}}`, and
   `#21` as the parent spec)
3. Key decisions made — and explicitly, any **new direct dependency** with the
   argument for it, or any change to the **declared surface** (§10: the nine
   variables, the four subcommands, the health contract, the owned paths)
4. Files changed
5. Blockers or notes for the next iteration

Keep it concise.

# THE ISSUE

If the task is not complete, leave a comment on the GitHub issue with what was
done and what remains.

If the task is complete, close the issue. Do **not** close #21 — it is the
parent spec and it closes when the whole system is built.

Once complete, output <promise>COMPLETE</promise>.

# FINAL RULES

ONLY WORK ON A SINGLE TASK.
