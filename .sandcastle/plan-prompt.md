# ISSUES

Here are the open issues in the repo:

<issues-json>

!`gh issue list --state open --json number,title,body,labels,comments --jq '[.[] | {number, title, body, labels: [.labels[].name], comments: [.comments[].body]}]'`

</issues-json>

# TASK

Work out which issues are **unblocked right now**, and return those.

## How blocking is recorded in this repo

**Read each ticket's `## Blocked by` section.** obsync's work tickets are
`/to-tickets` output and every one of them ends with a `## Blocked by` section
naming the issues it depends on by number, or saying `None — can start
immediately.` GitHub's native `blocked-by` field is **not** populated here, so
the prose section is the source of truth. Do not infer a dependency graph you
could have read.

An issue is **unblocked** when every issue named in its `## Blocked by` section
is closed (or absent from the open list above).

Two secondary checks, applied after that:

- **Overlapping files.** These slices are mostly a chain, so this rarely bites,
  but if two otherwise-unblocked tickets would both rewrite the same Go package,
  the same `.github/workflows/` file, `go.mod`, `docs/interface.md`, or
  `CONTEXT.md`, take the lower-numbered one and leave the other for the next
  iteration. A merge conflict in `go.mod` or a workflow costs more than a
  serialised iteration.
- **Decisions not yet made.** If a ticket's shape depends on an interface an
  earlier ticket establishes, it is blocked even when the `## Blocked by`
  section does not say so.

## Issues to exclude

Never select an issue that:

- Is **#21** — the spec. It is the parent of every work ticket and describes the
  whole system; it is not implementable and closing it would be wrong. Same for
  **#1**, the wayfinder map, which is a tracker.
- Is labelled `wayfinder:map`, `wayfinder:grilling`, `wayfinder:prototype` or
  `wayfinder:research` — trackers and human-in-the-loop tickets, not work.
  `wayfinder:task` + `ready-for-agent` is the combination that means "an
  unattended agent may build this".
- Is labelled `wontfix`, `needs-info` or `ready-for-human`.
- Already has an assignee.
- Is a parent issue with implementation issues linking to it.

If an issue is ambiguous or under-specified, leave it out rather than guessing.
Note that obsync's tickets are *not* usually under-specified — the spec fixed
every timing constant, threshold and default deliberately, so if a ticket reads
as vague, re-read `gh issue view 21 --comments` before concluding it is.

## Branch names

For each unblocked issue, assign a branch name using the **exact, deterministic**
format `sandcastle/issue-{number}` — just the issue number, with **no title
slug**. This is critical: the branch name must be byte-for-byte identical every
time the same issue is planned, so its git worktree is **reused** across
iterations instead of a brand-new worktree (and a cold `go mod download` plus a
cold build cache) being created each run. Never append a slug or any words
derived from the title — `sandcastle/issue-24` must stay `sandcastle/issue-24`
on every iteration.

Note the two numbering schemes and do not confuse them: the ticket titles carry
a two-digit slice number (`01 — Project skeleton…` is issue **#22**). The branch
and every reference use the **GitHub issue number**.

# OUTPUT

Output your plan as a JSON object wrapped in `<plan>` tags:

<plan>
{"issues": [{"number": 22, "title": "01 — Project skeleton: a version-stamped binary and a green CI", "branch": "sandcastle/issue-22"}]}
</plan>

Include only unblocked issues. If every issue is blocked, include the single
highest-priority candidate — the one with the fewest or weakest dependencies,
which in a chain like this one means the lowest slice number still open.
