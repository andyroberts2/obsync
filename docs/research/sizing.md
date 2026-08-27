# Sizing — Research Report for obsync

Resolves ticket [#44](https://github.com/andyroberts2/obsync/issues/44). Consumers: the README's **Sizing**
section, which states the deployment facts, and `docs/operations.md` when [#42](https://github.com/andyroberts2/obsync/issues/42)
writes it.

This is the one thread the spec ([#21](https://github.com/andyroberts2/obsync/issues/21), Further Notes) left
open, and it left it open deliberately as *a measurement rather than a decision*: three costs known in kind and
unknown in size, none of which could be settled before there was code to run. There is now, so they were run.

Method. The first two costs are Go benchmarks in this repository, at **seam 1** — a real vault directory, a real
`git init --bare` remote over `file://`, real git underneath, and obsync's own code driving both. They are in
[`sizing_test.go`](../../sizing_test.go), and the commands that produced the two tables are in §5. The third is
not a rate and is not benchmarked: it is a fact about one merge, and it is
`TestKeepingBothSidesOfAnAttachmentDoublesItInTheVaultAndNotInTheObjectStore` in the same file.

Claims are marked **[measured]** (a benchmark or a command run here, with its output) or **[inference]**
(reasoning from one, flagged as such). Nothing below rests on a document that was not run.

---

## Verdict

**1. `git status` per run costs about 4ms plus about 0.8µs per tracked path.** **[measured]** 4.7ms at a
thousand notes, 12.4ms at ten thousand, 42.7ms at fifty thousand, and the two points of the git matrix agree to
within 4%. The tick is 60 seconds, so even at fifty thousand notes — past what this audience has — a vault
syncing at one run per tick spends **0.07% of one core** on the one command every run pays.

**2. A divergence costs about 68ms at a thousand notes and 224ms at fifty thousand.** **[measured]** The out-of-tree
merge is computed on every divergence by design (§4), and a divergence is the designed-for case rather than an
anomaly, so this is a cost a vault pays routinely. At fifty thousand notes it is under a quarter of a second, of
which about 43ms is the `git status` the write-side settle guard already runs (§6). **[inference]**

**3. Vault-side disk doubling is real, is bounded, and is working-tree-only.** **[measured]** Keeping both sides
of a conflicted 4 MiB attachment leaves the vault's working tree carrying 8 MiB — the file twice, because a
working tree is files. The object store gains **no bytes at all**: the conflict copy is the same blob object the
fetch already brought down, named at a second path. What bounds it per merge is the conflict-storm ceiling, and
what bounds it over time is the human deleting the copy, because obsync never deletes a file from a vault of its
own accord.

**No design change falls out of any of the three, and nothing here contradicts a spec decision**, so no new
ticket is raised. One thing is recorded that was not previously written down, in §1: the **racily-clean index**,
which is the one place obsync's `GIT_OPTIONAL_LOCKS=0` pin has a measurable cost, and which is bounded to one
second's worth of paths in any vault somebody is actually writing in.

---

## 1. `git status` cost per run

### What obsync actually runs

Once per sync run, whatever else the run does:

```
git status --porcelain=v2 -z -uall
```

`Repo.Changed`, `internal/git/git.go`. **The watcher never contributes to this list** — it wakes the loop and
never says what changed (§2) — so the cost is a function of the vault rather than of what moved in it. `-uall`
is why untracked directories are walked to their leaves: obsync stages paths rather than directories.

The run rate this is paid at is bounded from above by the loop, not by the vault. A tick is 60s; a wake-up
brings a run forward to the end of a 10s quiet window; continuous writing is capped at one run per 5 minutes.
The worst sustainable rate is therefore about **six runs a minute**, in a vault written in bursts ten seconds
apart. **[inference]**, from §2's constants.

### Numbers

Minimum of three runs of 100 iterations each, at both points of the git matrix. **[measured]**

| Vault | git 2.52.0 — the version the shipped image carries | git 2.38.5 — the **git floor** |
|---|---|---|
| 1,000 notes | **4.74 ms** | 4.92 ms |
| 10,000 notes | **12.40 ms** | 12.41 ms |
| 50,000 notes | **42.72 ms** | 42.63 ms |

The two points agree to within 4% everywhere, which is the answer to the only question the matrix asks of this
measurement: **nothing about the cost of a sync run depends on which of the two gits obsync is running.**

Fitted, and the fit is what makes this three points rather than a table to look values up in: **≈4ms plus
≈0.8µs per tracked path**, which predicts all three measurements to within 5%. **[inference]** The fixed term is
process start, reading the index, and walking a vault's hundred directories; the linear term is one `lstat` per
tracked path, which git spreads over as many as twenty threads.

### What that means for a deployment

| Vault | One run | At one run per tick (60s) | At six runs a minute |
|---|---|---|---|
| 1,000 notes | 4.7 ms | 0.008% of one core | 0.05% |
| 10,000 notes | 12.4 ms | 0.021% | 0.12% |
| 50,000 notes | 42.7 ms | 0.071% | 0.43% |

There is no vault size in this range at which the per-run cost is visible next to the tick, and no reason in
these numbers to shorten the tick, lengthen it, or make it a knob.

### Side finding: the racily-clean index, and `GIT_OPTIONAL_LOCKS=0`

An index entry whose recorded mtime is **not strictly older than the index file's own timestamp** cannot be
judged from the stat alone, so git re-reads the file's content rather than trusting it. *Racily clean* is git's
own name for that state. The comparison is made at **one-second granularity** on the git this measurement was
taken against — **[measured]**, from the behaviour below rather than from reading a build flag, which is why it
is stated as an observation and not as git's documented contract.

git ordinarily clears the state by writing the refreshed index back after a status. **obsync never can**:
`GIT_OPTIONAL_LOCKS=0` (§1) is exactly the pin that forbids that write-back, and it is there so obsync never
takes the index lock away from a human's own git.

Measured on a thousand-note vault, running the same command obsync runs. First against an index that has never
been refreshed, then after a status refreshed it:

```
never refreshed:                                    13.35 ms/op
after one index-writing status:                      5.45 ms/op
```

And on a freshly built vault, refreshing at two different moments — which is what pins the granularity at one
second:

```
index refreshed in the same second as the commit:    9.95 ms/op
index refreshed once that second had passed:         5.25 ms/op
```

**[measured]** — a thousand notes costing what ten thousand cost, for as long as nothing else writes the index.

**It is not a deployment cost, and that is why it is a side finding rather than a ticket.** The racy set is only
the paths whose mtime falls in the same second as the last index write, and obsync writes the index on every run
that commits. A vault somebody is writing notes in therefore has at most one second's worth of paths in it, and
those paths are modified anyway, so git was going to read them. The residual case is a vault that never changes
at all, where obsync never commits and never rewrites the index: it keeps re-reading whatever landed in the last
second of the bootstrap clone, which is a handful of notes and no growth. **[inference]**

The benchmark waits that second out and refreshes the index once through a git that does not carry the pin, so
that what it measures is a vault in the state a vault actually syncs in. It does so at both matrix points, which
costs a second and nothing else at a point that would not have needed it. Without that, a thousand-note vault
measures at ~15ms an op and lands *slower than ten thousand*, which is how this was found.

---

## 2. Merge time as the tree grows

### What obsync actually runs

On a divergence, `Repo.Reconcile` fetches, checks the remote's history is still the history obsync last saw,
classifies with `rev-list --count --left-right`, and then runs §4's out-of-tree merge:
`merge-tree --write-tree`, obsync's own substitution per conflicted path through a temporary index, a
`commit-tree` with both parents, the write-side settle guard, and `reset --keep` to apply. The vault is
untouched until the last step.

The benchmark drives one real divergence per iteration with **one conflicted path** — both sides editing the
same daily note, which is user story 10 and the shape that reaches every expensive part of §4, including
writing a conflict copy. Building the divergence is outside the timer.

### Numbers

Minimum of two runs of ten iterations each, at both points. **[measured]**

| Vault | git 2.52.0 | git 2.38.5 — the **git floor** |
|---|---|---|
| 1,000 notes | **67.5 ms** | 66.1 ms |
| 10,000 notes | **100.5 ms** | 97.2 ms |
| 50,000 notes | **223.6 ms** | 232.4 ms |

The two points agree to within 4% again, on the operation that fixes the floor where it is: `--write-tree`
landed in 2.38, and this is what it costs there.

### What that means for a deployment

Under a quarter of a second, on a fifty-thousand-note vault, for a merge that keeps both sides of a conflicted
note. Three things follow, none of them a change:

- **The out-of-tree merge is affordable on every divergence**, which is what §4 assumed when it rejected
  fast-forward-only-and-freeze on frequency. **[inference]**
- **Abandoning a merge and recomputing it next run is cheap**, which is what §4 said when it made a
  `reset --keep` refusal an aborted run rather than an error. The whole merge is the thing being thrown away,
  and it is a fraction of a tick. **[inference]**
- **Growth is far slower than the vault's.** Ten times the notes cost 1.5× the merge from a thousand to ten
  thousand, and five times again cost 2.2× from ten thousand to fifty thousand. The merge is bounded by what
  diverged plus the tree-walking the apply has to do, not by the vault. **[inference]** About 43ms of the
  fifty-thousand-note figure is the `git status` the write-side settle guard runs (§6), which is the same
  command §1 measured. **[inference]**

---

## 3. Vault-side disk doubling

This one is not a rate, so it is not benchmarked. It is a fact about one merge, and it has an exact cause and an
exact bound.

### The cause

§4's keep-both rule: on a conflicted path the vault's version stays at the canonical path and the losing
version — almost always the remote's — is written beside it as a **conflict copy**, byte-identical, committed
inside the merge commit. Both are ordinary files in the working tree, and a git working tree does not hardlink
or deduplicate anything, so **the vault volume carries those bytes twice**.

Measured: a conflicted 4 MiB attachment leaves the vault's working tree holding **8,388,611 bytes** where it
held 4,194,307 before — the attachment twice, plus the three bytes of `.obsidian/app.json`. **[measured]**

### What is *not* doubled, and why the remote pays nothing

The object store. The conflict copy is not a second blob: it is **the blob the fetch already brought down**,
named at a second path. Measured as object identity rather than as a size — the copy's blob at the merge commit
and the losing version's blob at the merge's second parent are the same object name. **[measured]**

That is the vault-side half of what [#19](https://github.com/andyroberts2/obsync/issues/19) measured from the
remote's side, where a push carrying a blob the remote already holds transfers a 582-byte pack for an 8 MiB
file, and GitHub's size check — which inspects received objects rather than walking trees — says nothing. It is
also why a conflict copy is **exempt from the size ceiling at any size** (§4): there are no new bytes for a
ceiling to be about.

### The bound

- **Per merge**: at most the losing versions of the conflicted paths in that merge. §4 bounds that count with
  the **conflict-storm ceiling** at ~50 paths, past which the merge is a network freeze that applies nothing —
  so by design one merge adds at most the total size of up to fifty losing versions. **[inference]** **That
  ceiling is not implemented yet**; it is [#31](https://github.com/andyroberts2/obsync/issues/31)'s, and the
  README's Not-yet list says so, so until it lands the per-merge bound is every path that conflicted.
- **Over time**: unbounded by obsync, and deliberately so. A conflict copy exists until a human deletes it, and
  **obsync never deletes a file from a vault of its own accord** — that is a never-list entry. The standing cost
  is the sum of every unresolved copy, and the operator holds that bound.
- **Not bounded by the size ceiling.** Conflict copies are exempt from it, and obsync has never gated what it
  *receives*: a file over the ceiling that arrives from the remote lands in the vault on any ordinary pull.

### The deployment fact

**Size the vault volume for the attachments, not for the notes.** A vault of notes doubles a few kilobytes; a
vault where a 90 MB video can be edited on two devices doubles 90 MB, on the volume rather than at the remote.
obsync does not check free space before it writes, and there is no knob for a disk threshold. §7's rule is that
**statfs labels, never gates** — free space belongs in the diagnosis of a failed local command and never in a
decision to act — and that reporting is [#34](https://github.com/andyroberts2/obsync/issues/34)'s and is not
built yet. Either way the planning is the operator's, which is why this is documentation's material.

---

## 4. What this changes

**Nothing.** Every number above sits comfortably inside the assumptions the spec made:

| Spec decision | What it assumed | What was measured |
|---|---|---|
| §2, the 60s tick | that a `git status` per tick is affordable | 43ms at fifty thousand notes: 0.07% of a tick |
| §2, one run per wake-up | that the whole loop can run on every wake-up | the local half's growing term is one `lstat` per path |
| §4, out-of-tree merge on every divergence | that recomputing a merge is cheap | 68–224ms, and an abandoned one costs the same to redo |
| §4, conflict copies exempt from the size ceiling | that a copy introduces no new bytes to the remote | the copy is the same blob object, at a second path |
| §7, statfs labels, never gates | that disk headroom is the operator's to plan | the doubling is real, bounded, and now written down |

**Nothing here contradicts a spec decision, so nothing is raised as a new ticket** — which is what
[#44](https://github.com/andyroberts2/obsync/issues/44)'s last acceptance criterion asks for either way round.

---

## 5. Reproducing this

The numbers are from a container on a shared virtual machine, so they are indicative rather than a
specification: a reading taken while the host is busy elsewhere can be several times the quiet one, which is why
**every figure quoted above is the minimum of the runs recorded** — the standard way to read a benchmark on a
machine you do not own. Nothing in the verdict is sensitive to that, because the margin between the cost and the
tick is three orders of magnitude either way.

```bash
# The current point of the git matrix, then the floor.
go test -run '^$' -bench BenchmarkStatusCostPerRun       -benchtime=100x -count=3 ./
go test -run '^$' -bench BenchmarkMergeCostPerDivergence -benchtime=10x  -count=2 ./
PATH="$GIT_FLOOR:$PATH" go test -run '^$' -bench BenchmarkStatusCostPerRun       -benchtime=100x -count=3 ./
PATH="$GIT_FLOOR:$PATH" go test -run '^$' -bench BenchmarkMergeCostPerDivergence -benchtime=10x  -count=2 ./

# The third cost, which is an assertion rather than a rate.
go test -run TestKeepingBothSidesOfAnAttachment ./
```

`-benchtime` is given in iterations rather than in seconds because a vault of fifty thousand notes costs more to
build than to measure, and the question is what one run costs rather than how many runs fit in a second.

**Environment**: Linux 6.17.0-41-generic x86_64, 16 vCPU (QEMU virtual CPU), 15 GB RAM, vaults built on
overlayfs, Go 1.25.12; **git 2.52.0** (the version the shipped image carries) and **git 2.38.5** (the git
floor). 2026-08-27.

**Vault shape**: notes of about 1.2 KB spread over a hundred folders, plus `.obsidian/app.json`, committed and
pushed to the bare remote before anything is measured. The three sizes are chosen rather than round: a thousand
notes is a vault a year old and the size almost every deployment is; ten thousand is a decade of notes, or an
import; fifty thousand is past what this audience has, deliberately, because a cost still invisible there is
invisible.
