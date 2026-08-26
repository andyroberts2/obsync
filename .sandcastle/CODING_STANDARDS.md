# obsync coding standards

obsync is one Go module and one container image. It is a daemon that holds a
write-scoped credential and drives `git` against a directory a human has open in
an editor, so the cost of being wrong is asymmetric: a missed backup is an
inconvenience, and a destroyed vault is unrecoverable. Nearly every rule below is
that asymmetry applied to a specific place.

The design is [issue #21](https://github.com/andyroberts2/obsync/issues/21) —
body plus both comments, which are parts 2 and 3 rather than discussion.
Section references (§1–§12) throughout are its sections.

---

## Vocabulary

`CONTEXT.md` is the glossary and it is binding on code, not just on prose.
**Sync loop**, **sync run**, **committable set**, **ignore floor**, **refused
path**, **settle guard**, **conflict copy**, **full freeze**, **network
freeze**, **aborted run**, **git floor**, **third writer**, **vault sentinel**,
**owned paths** — these are the names of types, functions, packages, log keys
and test names. `CONTEXT.md` also lists the words each term replaces; a
synonym in an identifier is a defect, because the whole value of a glossary is
that one word means one thing everywhere.

A term that turns out to be missing goes into `CONTEXT.md` in the same commit
that introduces it. A design document nobody may amend stops being read.

---

## The invariants

These are not preferences. Each one is a line the design deliberately declined
to cross, and most are stated in the README's never-list, which makes softening
one a visible amendment rather than an edit.

**obsync never**: force-pushes (not even `--force-with-lease`) · rebases ·
runs `git checkout` after bootstrap · writes the vault's `.git/config` ·
runs `git remote set-url` · re-clones or self-repairs · discards history ·
runs `git fsck` · rewinds a commit the remote refused · caps how far the local
branch runs ahead · diagnoses a remote rejection · overwrites a conflict copy ·
exits on a sync failure.

**Write them so a grep can check them.** The rules are phrased as absolutes —
"never `checkout` after bootstrap" rather than "checkout only in situation X" —
specifically so that `rg '"checkout"'` returning one hit in the bootstrap path
is a proof rather than a hint. Keep git subcommand names as plain string
literals at the call site; do not assemble them from variables or a table that
defeats the search.

**Fail open locally, fail closed outward.** A commit is non-destructive; a push
or a vault write publishes or overwrites. An inconclusive check therefore
degrades to a **network freeze**. **Full freeze** is reserved for "even
committing would be wrong".

**obsync may discard derived state; it never discards history.** `.git/index` is
derived and may be deleted. Commits, blobs and a human's files are not.

**obsync never writes a file the human owns.** Everything it writes lives in the
**owned paths** it declared (§10): `obsync-attention.md`, conflict copies,
`.git/info/exclude`, `.git/obsync/`, `.git/obsync.lock`,
`refs/obsync/failed-apply`. Adding a write outside that set is a change to the
declared surface, not an implementation detail.

**Every freeze self-clears when its cause is repaired, without a restart.**
Gates are re-checked at the top of every run; the damage freeze self-clears by
retrying a read-only probe. No latch may key on process lifetime — that is
exactly why gate 9 keys on a *ref*.

---

## Dependencies

**Stdlib plus exactly two direct dependencies**: a filesystem-notification
library, and `golang.org/x/sys` for `flock` and `statfs`. Logging is
`log/slog`. Configuration is `os.Getenv` and hand-written parsing — there is no
flag library and no config-file library to need, because there are no flags and
no config file.

A third direct dependency is not forbidden, it is **argued for**: it needs a
comment in the PR saying what stdlib could not do, and it is a change to the
supply chain of a container holding a write-scoped credential. Indirect
dependencies count — a library that drags in five is five.

Test-only dependencies are held to the same bar. `testing` plus real git is
enough for everything in this repo; an assertion library is not an argument.

---

## Driving git

Every decision in this design is expressed in git plumbing, so the subprocess
boundary is the whole risk surface.

**Never parse git output meant for humans.** Machine formats and NUL separation
everywhere: `status --porcelain=v2 -z`, `-z` on every listing command,
`--porcelain` on push, `-z` on `diff-tree -r`. A vault path may contain spaces,
unicode and — legally — a newline. Line-splitting git output is a bug even when
it passes.

**Two sharpenings, both from git's own design, neither an exception:**

- **git's own words may _name_ a failure; only persistence may _escalate_ one.**
  A corrupt-object message or an auth string may make the human's message say
  *looks like a corrupt object* instead of *git failed*. No behaviour ever
  branches on prose. Time is the classifier — damage is permanent, bad luck is
  not.
- `git push --porcelain` splits a documented enum from "a human-readable
  explanation". **Branch on the enum, relay the parenthetical verbatim**,
  labelled as the remote's words rather than obsync's.

**Pin the environment on every invocation**, not once at startup:
`LC_ALL=C`, `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`,
`GIT_CONFIG_NOSYSTEM=1`, and a per-process `GIT_CONFIG_GLOBAL` holding obsync's
private config. The vault's own `.git/config` outranks that global by design —
it is the escape hatch a repo with legacy-malformed objects uses.

**Timeouts are asymmetric.** Network commands: 120s. Local commands: **never
timed out**. A hung local git means the disk or kernel is in trouble, and
killing a `reset --keep` halfway manufactures the one unrecoverable state in
this design. Do not add a "safety" timeout to a local command.

**Every git runs in its own process group, and the kill signals the group**
(SIGTERM, then SIGKILL after a grace period), so `git-remote-https` and `ssh`
die with it. Always `Wait()` — there is no init process in the image, and there
is not meant to be.

**The credential helper's output is never logged, at any level.** DEBUG logs
full argv deliberately, and that is safe only because the token is never in a
URL, an argv or a file obsync wrote.

---

## Errors, and the three tiers

**Every runtime failure maps to exactly one of three tiers**: aborted run,
network freeze, full freeze. The list is closed. A new condition gets a cadence
and a health input, not a fourth category.

Model that in the type system rather than in prose at each call site: a failure
carries its tier, and the loop dispatches on it. Use typed sentinel errors and
`errors.Is`/`errors.As`; never classify by string-matching an error's own
`Error()` text — that is the same mistake as parsing git's prose, one level up.

- **Wrap with context** (`fmt.Errorf("...: %w", err)`) so the failing git argv
  and the path survive to the log line and the attention note.
- **The abort tier reports nothing** above debug. A transient loss is not news,
  and making it news is how the signal becomes noise.
- **Never `panic` on the loop path**, and never `os.Exit` from a sync failure.
  obsync parks alive; exiting discards backoff state and turns a diagnosable
  stuck state into a crash loop.
- The one place exiting is right is a **config error decidable from the
  environment block alone** (§8). If the check needs a syscall on the vault or a
  network round trip, it is a gate and it parks.

---

## Configuration: no new knobs

The config surface is **nine environment variables, one required**, and it is
part of the declared surface (§10). Adding a tenth is a spec change.

A value earns a knob by being **a fact about the deployment, never a taste**.
The quiet window, the max-wait cap, the tick, the push floor, the backoff floor,
the storm ceiling and the settle interval are all constants in code with the
reason for the number in a comment beside them. The settle interval especially:
**a knob there is a waiver with extra steps.**

Constants live in one place per section and are named after the glossary term,
not after their value.

---

## Go style

**Deep modules: small interface, deep implementation.** A few functions with
simple parameters hiding real complexity. The merge resolver's caller should not
learn about stage 1/2/3 blobs; the loop's caller should not learn about
`merge-tree`. Ask, of every new exported name: can I reduce the number of
functions, simplify the parameters, hide more inside?

Avoid shallow modules — a wide interface whose methods pass straight through.

**Accept dependencies, don't construct them — but only the two that are
seams.** obsync injects **the clock and the watcher, and nothing else**. Do not
add a `GitRunner` interface so git can be swapped in tests; that is precisely
the thing this project must not do (see Testing). Injectability is not a virtue
here, it is a specific, budgeted concession to determinism.

**Return results, don't produce side effects**, wherever the design allows. The
out-of-tree merge is the model: compute the tree, then apply it, so the
computing half is a pure function of two commits and is testable as one.

**Optional parameters and zero values deserve suspicion.** A struct field whose
zero value is a valid-looking setting is a bug by omission waiting to happen —
prefer a constructor that cannot produce a half-built value, and prefer
correctness over backwards compatibility when the two collide.

**Concurrency.** There is one serialized sync loop and only one run in flight,
by construction — not by a mutex bolted on afterwards. Resist goroutines: the
watcher feeds a channel, the loop consumes it, and that is the whole
concurrency model. `-race` is on in CI regardless.

**Logging is logfmt to stderr**, stdlib `log/slog`, one format, no knob. stdout
belongs to the subcommands. Levels have fixed meanings (§9): ERROR means a human
is needed; WARN means true but self-healing; INFO is the startup knob echo and
**only runs that changed something**; DEBUG is every git invocation with argv,
exit status and duration. **Healthy is quiet** — a log line on a successful
no-op run is a defect, because `docker logs --since 1h` being empty is a
designed signal.

**Filesystem writes go write-then-rename** through `.git/obsync/tmp/`. That is
the same rule for the status file, the attention note and anything added later.

---

## Testing

### Never fake git

**This is the most load-bearing rule in the project.** Every decision here is
expressed in git plumbing, so a faked git tests obsync's *beliefs* about git —
and those beliefs are the entire risk surface. `merge-tree --write-tree`'s
conflict output, `reset --keep`'s refusal conditions, `.gitignore`-over-
`info/exclude` precedence, the porcelain push enum and the exit codes damage
produces were all **measured** while this design was made. A fake reproducing
them faithfully would be a git reimplementation.

**Exactly two things are fakeable: the clock and the watcher.** Every timing
rule is then deterministic instead of slept through — the 10s quiet window, the
5min cap, the 60s tick, the 1s settle interval, the 15m backoff ceiling, the
hourly cadence, the 24h health ceiling, the 5-run persistence threshold. The
watcher fake supplies wake events and can simulate `ENOSPC` so tick-only mode is
reachable. **A test that calls `time.Sleep` to wait for obsync is a bug**; a
test that sleeps to let a *file* age is using the clock it cannot fake and
should say so.

### Test external behaviour only

"External" is unusually well defined here: obsync's observable output is **the
state of two real git repositories and one real directory**. A test asserts on
commits in the bare remote, bytes and files in the vault, the contents of the
attention note, the exclude file, the presence of a ref, and the output and exit
code of a subcommand. **It never asserts on how obsync got there** — not on
which git commands ran, in what order, or how many times.

```go
// GOOD — asserts on the world obsync changed.
func TestConflictKeepsBothSides(t *testing.T) {
    env := newVault(t)                    // real vault + real bare remote over file://
    env.writeNote("Daily/2026-08-24.md", "local edit")
    env.remoteCommit("Daily/2026-08-24.md", "remote edit")

    env.tick()

    env.assertVaultFile("Daily/2026-08-24.md", "local edit")
    env.assertConflictCopyOf("Daily/2026-08-24.md", "remote edit")
}
```

```go
// BAD — asserts on the implementation. Breaks on a refactor that changed nothing.
func TestConflictCallsMergeTree(t *testing.T) {
    got := recorder.commands()
    if got[2] != "merge-tree --write-tree" { ... }
}
```

Red flags: mocking an internal collaborator; testing an unexported function
that has an observable path to it; asserting on call counts or ordering;
a test name that describes *how*; a test that breaks under a refactor with no
behaviour change.

### The two seams

**Seam 1 — obsync's process boundary, with real git underneath.** The primary
seam and the only one for behaviour. A test builds a real vault directory and a
`git init --bare` remote reached over `file://`, injects the clock and the
watcher, and drives the loop end to end. Everything — the merge resolution, the
gates, the tiers, the settle guard, the ignore floor, the config surface, the
attention note, the status file — is reached through here.

**The bare local remote is not a compromise.** It takes a real `pre-receive`
hook and a real `receive.maxInputSize`, so every verdict §7's push disposition
table keys on is reproducible offline with no flake. A suite needing a
credential cannot run on a fork's PR. The cases a bare repo cannot produce — a
proxy 413, GitHub's exact rejection prose — are exactly the ones obsync relays
verbatim and never diagnoses, so there is no assertion to give up.

**Seam 2 — the container image.** The only seam that can see properties of the
*assembly*: the image running as an **arbitrary UID with no `/etc/passwd`
entry**, the git-floor gate firing, the `HEALTHCHECK` being wired, and the
credential file being re-read. Run at `--user 4242:4242` against a throwaway
vault and the same bare remote. The reference `compose.yaml` is exercised the
same way, because it is normative.

**Two seams, one per shipped artifact, and no third.**

### Practicalities

- `t.TempDir()` for every vault and remote; never a fixed path, never `/tmp`
  by hand.
- Table-driven tests for the closed tables — §4's conflict table and §7's push
  disposition table each have a row-per-case suite, and a row added to the spec
  without a row added here is an incomplete change.
- `t.Parallel()` wherever the test owns its own temp dirs. The loop is
  serialized inside one obsync, not across two.
- Prefer one helper package that builds a vault-plus-remote environment with a
  fluent surface, so a new test is three lines and reads as behaviour.
- Assert on error *tiers* and observable outcomes, not on error strings.

---

## The git floor

**The git floor is one file that both CI and the binary read** — embedded into
the binary for the startup gate, and read by the workflow to build the matrix's
lower point. Two independently-typed numbers is a promise a human keeps, and a
promise a human keeps is not a definition. **Drift must be structurally
impossible, not merely detected.**

The matrix's upper point is **derived** from the base image (`git --version`
inside it), never declared, so it follows the digest bump automatically.

In the AFK sandbox both points are source-built and neither comes from the
distro: the plain `git` on PATH is the version the shipped image carries, and
the floor sits at `$GIT_FLOOR` (`/opt/git/floor/bin`). Run the floor point with:

```bash
PATH="$GIT_FLOOR:$PATH" git --version   # confirm you got the floor
PATH="$GIT_FLOOR:$PATH" go test ./...
```

Any change that touches git plumbing must pass at both points before it is
committed.

---

## Documentation that is part of the code

**Load-bearing documentation is a named class**: a documented line is
load-bearing when it is **the only thing standing where the design deliberately
declined to put code**. The test is *"the code chose not to do this"*, never
*"this is important"*.

Consequences that a general commitment to good docs does not have: a
load-bearing line **names its owning decision** with a resolvable issue link, is
**never cut for brevity**, and its absence is a **defect rather than a gap**. It
is marked in place and visibly. There is no index — the marker greps.

The eight known members: the `user: "1000:1000"` compose mapping that replaces a
`PUID`/`PGID` knob · the `WRITE_COALESCE_MS=0` line on the ignis service · the
Headless Sync warning · the damaged-repo recovery recipe · the
remote-rejection recipe and *relays, never diagnoses* · *"this clears on its own
once fixed; no restart needed"* · the transcription quarantine rule · the
README's never-list.

**Docs state what obsync does; they never argue for it.** The arguing lives in
the tickets.

**A change to `docs/interface.md` is a change to a promise.** The declared
surface (§10) is strictly larger than the config surface: the nine variables,
the four subcommands, the health contract, and the owned paths. CI fails a
release whose interface page changed since the last tag without a "Surface
changes" note. Treat that as the rule it encodes rather than as a CI hoop.

**The transcription quarantine.** The credential isolation transcribed from
`kubernetes/git-sync` lives in **one file**, carrying the upstream Apache-2.0
header plus the exact upstream commit and line range. Do not scatter it, and do
not "clean it up" into surrounding packages — the point is that the licence
obligation stays auditable.

---

## Comments

Comment the *why*, and specifically the numbers. Every timing constant,
threshold and ceiling in this design has a reason that took a measurement or an
argument to establish — 10s clears ignis's write pipeline with margin, 1s is
>3× ignis's 300ms stability threshold, 95MB sits under GitHub's 100MB hard
block and above its 50MB soft warning. A constant without its reason beside it
is the next person's judgement call at the keyboard, which is exactly what the
spec spent nineteen tickets removing.

Do not comment the *what* when the code already says it.
