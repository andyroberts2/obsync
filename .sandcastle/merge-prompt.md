# TASK

Merge the following branches into the current branch:

{{BRANCHES}}

For each branch:

1. Run `git merge <branch> --no-edit`
2. If there are merge conflicts, resolve them intelligently by reading both
   sides and choosing the correct resolution. Use the
   `mattpocock-skills:resolving-merge-conflicts` skill
   (`/mattpocock-skills:resolving-merge-conflicts`) for anything non-trivial
3. After resolving conflicts, run the checks and verify they are green, from the
   repo root:

   ```bash
   go build ./... && go vet ./... && go test ./...
   test -z "$(gofmt -l .)" || gofmt -l .
   golangci-lint run
   go test -race ./...
   PATH="$GIT_FLOOR:$PATH" go test ./...    # the git floor point of the matrix
   ```

   And, if any merged branch touched the image, the entrypoint, the healthcheck
   or `compose.yaml`:

   ```bash
   docker build -t obsync:dev . && docker run --rm --user 4242:4242 obsync:dev status
   docker compose -f compose.yaml config
   ```

   `implement-prompt.md`'s **SANDBOX ENVIRONMENT** section describes what this
   container has — read it rather than probing. In short: Go, `golangci-lint`,
   two gits (the shipped image's version as plain `git`, the 2.38 floor at
   `$GIT_FLOOR`), `gh`, `rg` and a working Docker socket; no node, `uv` or
   `pre-commit`; no root.

4. If a check fails, fix it before proceeding to the next branch — never weaken a
   lint rule or an assertion to go green

# THIS REPO'S MERGE HAZARDS

**`go.mod` / `go.sum`.** Two branches adding requirements conflict textually and
resolve badly by hand. Take both sides' `require` entries, then run
`go mod tidy` and let the tool write the file. Then **count the direct
dependencies**: obsync's rule is stdlib plus exactly two (a filesystem-
notification library and `golang.org/x/sys`). If the merged `go.mod` has a
third, that is not a merge artefact to smooth over — say so in the merge commit
and in the issue comment, because a third direct dependency is a decision
somebody has to make.

**`.github/workflows/`.** Two branches editing the same workflow is common here
(the CI matrix, the release job). Keep both sides' steps unless they genuinely
conflict; never drop a `permissions:` block or unpin a SHA-pinned action while
resolving.

**The git-floor file.** The floor is deliberately **one file that both CI and
the binary read**, so drift is structurally impossible rather than merely
detected. If a merge produces two sources for that number, the merge is wrong —
collapse it back to one.

**`docs/interface.md`.** This is the declared surface. If two branches both
changed it, keep both changes and make sure the union is still accurate. A
change here is a change to a promise, and the release pipeline fails a tag whose
interface page moved without a "Surface changes" note.

**`CONTEXT.md`.** The glossary is append-mostly. If two branches added terms,
keep both, in the section each belongs to. If two branches defined the *same*
term differently, that is a real disagreement — pick the one matching issue #21
and note it.

**Constants.** If a merge leaves two definitions of the same timing constant,
threshold or ceiling, collapse to one and keep the comment that explains the
number. Every one of them has a measured or argued reason; a merge that keeps
the value and loses the reason has lost the more valuable half.

Nothing here has database migrations, generated code, or a lockfile beyond
`go.sum`.

After all branches are merged, make a single commit summarizing the merge.
There is no `pre-commit` config in this repo — the checks above are the gate.

# CLOSE ISSUES

For each branch that was merged, close its issue with
`gh issue close <number> --comment "..."`.

**Do not close #21.** It is the parent spec of every ticket and it closes when
the whole system is built, not when one slice lands. Same for #1, the wayfinder
map.

Here are all the issues:

{{ISSUES}}

Once you've merged everything you can, output <promise>COMPLETE</promise>.
