# Git remote file-size limits — Research Report for obsync

Resolves research ticket [#19](https://github.com/andyroberts2/obsync/issues/19). Consumer: the resolution of
[#14](https://github.com/andyroberts2/obsync/issues/14), which exempts conflict copies from obsync's size ceiling
on the strength of the answer below.

Method. Two halves, both settled empirically rather than by inference:

- **Half A** (does a push transfer a blob the remote already has, when it appears at a new path?) was run against
  two local repositories with a `pre-receive` hook that dumps the quarantine directory. Every command and its
  actual output is reproduced in §2.
- **Half B** (what does GitHub's check inspect?) was run **against real github.com**, using the 50 MiB warning
  threshold — which GitHub documents as the same check as the 100 MiB block — to construct the discriminating
  scenario that cannot be constructed at the 100 MiB threshold. Commands and output in §3.

- **Generalisation** (§5) and **discoverability** (§6) rest on documentation and on reading the upstream source
  of GitLab, Gitaly, and Gitea. Those are **not** reproduced measurements against live GitLab or Gitea
  instances, and §5.1 says so explicitly where it matters.

Environment for all empirical work: `git version 2.51.0`, Linux, 2026-08-26. Throwaway private probe repository
`andyroberts2/obsync-sizecheck-probe`, pushed over both SSH and HTTPS+PAT.

Throughout, claims are marked **[verified]** (I ran it and observed the output), **[primary]** (quoted from the
document that owns the behaviour), or **[inference]** (reasoning from the two, flagged as such).

---

## Verdict

**GitHub's size check inspects the objects it actually receives. It does not walk the trees of the pushed
commits.** This is **[verified]** against live github.com, in both directions:

- A blob that is **in the pushed tip's tree but not in the pack** (because the remote already has it) is **not
  flagged** — not when copied to a second path, not when `git mv`'d to a new path, not when carried into a
  brand-new branch. Three separate shapes, all silent.
- A blob that is **in the pack but not in the pushed tip's tree** (added in one commit, deleted in the next, both
  pushed together) **is flagged**, naming a path that does not exist at the tip.

Those two results together pin the check exactly. No tree walk of any pushed commit produces that pair of
answers; only an inspection of received objects does.

**Consequence for obsync: #14's resolution stands, and now for a verified reason rather than an assumed one.**
A conflict copy takes a blob obsync has just fetched — already reachable from the remote's tip — and commits it
at a new path. **[verified]** in §2: that push transfers one commit and two trees, a 582-byte pack, for an 8 MiB
blob. **[verified]** in §3 against real GitHub: no size complaint of any kind. obsync will not build merge
commits it cannot push on account of a conflict copy.

**Three qualifications, none of which disturb that conclusion but all of which belong in the spec's statement of
remote requirements:**

1. **The "it names a path" objection is answered, not merely set aside.** GitHub names the path because it
   receives the *tree* too, and the tree names the path. It does not need a tree walk to say `big.bin`. §3.5.
2. **Per-blob is not the only shape of limit.** GitHub also enforces a **2 GiB per-push pack limit**
   (`remote: fatal: pack exceeds maximum allowed size`) and a 10 GB repository on-disk limit — **[primary]**,
   §4. A tree walk would never catch those, and neither would obsync's per-file ceiling. They are a different
   failure mode, and they fail the *whole push*, not one file.
3. **A client essentially cannot ask a remote what its limits are.** §6. The git wire protocol carries no such
   capability at all, and push does not even run on protocol v2. The single exception found is GitHub's ruleset
   endpoint (`repository-rule-max-file-size`, §6.3) — Team/Enterprise only, capped at 100 MB, and an empty
   result means "no *extra* rule", not "no limit". obsync's ceiling has to stay a configured number.

**And one thing this report confirms rather than changes:** `CONTEXT.md`'s **Size ceiling** entry already says
*"Never applied to bytes the remote already holds, which is why a conflict copy is exempt at any size."* That
wording is correct as written. No vocabulary change falls out of #19.

---

## 1. The mechanism, from git's own documentation

**[primary]** `git-receive-pack(1)`, "QUARANTINE ENVIRONMENT"
(<https://git-scm.com/docs/git-receive-pack>):

> When `receive-pack` takes in objects, they are placed into a temporary "quarantine" directory within the
> `$GIT_DIR/objects` directory and migrated into the main object store only after the `pre-receive` hook has
> completed. If the push fails before then, the temporary directory is removed entirely.

This is the structural fact the whole question turns on. A `pre-receive` hook is handed exactly the set of objects
that crossed the wire, in a directory of their own, *before* they join the repository. A size checker written
against that directory sees received objects. A size checker written against `git ls-tree -r $new` sees the
snapshot. Both are available to a hook author; they give different answers; the question is which one GitHub
wrote.

**[primary]** GitHub confirms it runs its own checks in that same quarantine model. The GitHub Enterprise Server
docs for custom pre-receive hooks list the environment variables always available to a hook, including
`$GIT_QUARANTINE_PATH`, and define `$GIT_OBJECT_DIRECTORY` as *"Path to a temporary directory containing the
objects from the push"*, noting that `$GIT_QUARANTINE_PATH` *"contains the same value as
`$GIT_OBJECT_DIRECTORY`"*
(<https://docs.github.com/en/enterprise-server@3.17/admin/enforcing-policies/enforcing-policy-with-pre-receive-hooks/creating-a-pre-receive-hook-script>).

**[verified]** that GitHub's size check is a pre-receive hook: a rejected push reports
`! [remote rejected] main -> main (pre-receive hook declined)` — see §3.6 for the full transcript.

---

## 2. Half A — does the blob actually cross the wire? **Verified: no.**

### 2.1 Setup

Two local repositories, no network. The bare "remote" carries a `pre-receive` hook that prints (a) the contents
of the quarantine directory, i.e. exactly what was received, and (b) what each of the candidate checker designs
would see.

```sh
git init --bare -q remote.git
# hooks/pre-receive prints $GIT_QUARANTINE_PATH contents and verify-pack of the received pack
git clone -q remote.git work
cd work
head -c 8388608 /dev/urandom > big.bin      # 8 MiB, incompressible
echo hello > note.md
git add . && git commit -q -m "initial with big.bin"
git push -u origin main
```

Observed on that first push — the blob is present in the quarantine, as expected for content the remote has never
seen:

```
remote:   RECEIVED-LOOSE type=blob size=8388608 oid=e410cd6ee7d28486d846b811c7d51232a6ad2754
remote:   RECEIVED-LOOSE type=blob size=6 oid=ce013625030ba8dba906f756967f9e9ca394464a
remote:   RECEIVED-LOOSE type=tree size=70 oid=b90d47b434b8c48874b8f6c7fb2dfac248e60b3b
remote:   RECEIVED-LOOSE type=commit size=139 oid=ba04533a5927dd7ea0696df11fc938f0cd1717c0
```

### 2.2 The conflict-copy push

```sh
cp big.bin "conflicts/big (conflicted copy).bin"
git add "conflicts/big (conflicted copy).bin"
git commit -q -m "conflict copy of big.bin at a new path"
git rev-parse "HEAD:conflicts/big (conflicted copy).bin"   # -> e410cd6e... (identical oid)
git rev-list --objects HEAD --not origin/main              # what git decides to send
git push --verbose origin main
```

`git rev-list --objects HEAD --not origin/main` — git's own answer to "what must I send":

```
2db860337a6ced47cd2fe339559dbbb4c1617750
175abe759f5b6d137674c015895724b10b0316c0
f2d3df372134669515008437cd503d1304518dc5 conflicts
```

One commit, two trees. **No blob.** And what the remote actually received:

```
remote:   RECEIVED-LOOSE type=tree size=53 oid=f2d3df372134669515008437cd503d1304518dc5
remote:   RECEIVED-LOOSE type=tree size=106 oid=175abe759f5b6d137674c015895724b10b0316c0
remote:   RECEIVED-LOOSE type=commit size=205 oid=2db860337a6ced47cd2fe339559dbbb4c1617750
```

**[verified] The 8 MiB blob was not transferred.** Push succeeded.

### 2.3 The true obsync shape — the blob arrives *from* the remote first

§2.2 has the client authoring the blob, which is not obsync's case. Repeating with the blob originating on the
remote side, so the client only ever received it:

```sh
# in a second clone `peer`:
head -c 6291456 /dev/urandom > remote-only.bin
git add remote-only.bin && git commit -q -m "peer adds remote-only.bin"
git push origin main            # blob IS transferred here: RECEIVED-LOOSE type=blob size=6291456
# back in `work`:
git fetch -q origin && git merge -q --ff-only origin/main
cp remote-only.bin "conflicts/remote-only (conflicted copy 2026-08-26).bin"
git add "conflicts/remote-only (conflicted copy 2026-08-26).bin"
git commit -q -m "conflict copy of a blob that came FROM the remote"
git push --verbose origin main
```

Blob oid at the new path is identical to the peer's: `9a692fe0b66b5f5689f42e5882be0061122e201e`. Received:

```
remote:   RECEIVED-LOOSE type=tree size=125 oid=6f4c267319db7821847f646540e9145a7728cfa7
remote:   RECEIVED-LOOSE type=tree size=187 oid=5beaae76af5814ddf55f964171626f90588ba983
remote:   RECEIVED-LOOSE type=commit size=216 oid=55f49f0567fffb8ec6528b9184ca5301ae70f40f
```

**[verified] Same result.** Trees and a commit; no blob.

### 2.4 Control — a genuinely new blob *is* transferred

So the measurement is not vacuous. Pushing a brand-new 4 MiB blob:

```
remote:   RECEIVED-LOOSE type=blob size=4194304 oid=be4a3c49021ead5203275918f9dcfe6946952cba
remote:   RECEIVED-LOOSE type=tree size=144 oid=4a5837cb95df72663208dc55d31cd4113c84454b
remote:   RECEIVED-LOOSE type=commit size=196 oid=eefd9043091a86b96787ccde3c7d55525de5900f
```

### 2.5 The three candidate checker designs, side by side

Re-running the conflict-copy push with `receive.unpackLimit=1` so a real pack lands, and a hook that runs all
three designs against the same push:

```
=== A. OBJECTS ACTUALLY RECEIVED (verify-pack of the quarantined pack) ===
  pack file on disk: pack-317fc697b8f66fdd815b01556d6c72e1e31b4420.pack = 582 bytes
  RECEIVED type=commit size=204 oid=9c8ec349884b8646cdbff9ccac14f931b80c2bb3
  RECEIVED type=tree size=30  oid=8e1542aab4094482b385f3c05e8cc6c742367e1c
  RECEIVED type=tree size=14  oid=e4e19248430ee05add9abd7943a90955f6e2d148
  RECEIVED type=tree size=187 oid=5beaae76af5814ddf55f964171626f90588ba983
  RECEIVED type=tree size=125 oid=6f4c267319db7821847f646540e9145a7728cfa7

=== B. CHECKER DESIGN 1: reachability delta, rev-list --objects $new --not --all ===
  DELTA type=commit size=204 path=<none>
  DELTA type=tree   size=187 path=<none>
  DELTA type=tree   size=180 path=conflicts

=== C. CHECKER DESIGN 2: FULL TREE WALK of the new tip, git ls-tree -r $new ===
  FULLTREE size=8388608 oid=e410cd6e... path=big.bin
  FULLTREE size=8388608 oid=e410cd6e... path=conflicts/big (conflicted copy B).bin
  FULLTREE size=6291456 oid=9a692fe0... path=conflicts/remote-only (conflicted copy 2026-08-26).bin
  FULLTREE size=4194304 oid=be4a3c49... path=newbig.bin
  FULLTREE size=6       oid=ce013625... path=note.md
  FULLTREE size=6291456 oid=9a692fe0... path=remote-only.bin
```

(Oids abbreviated for width, and the two `conflicts/...` paths are shown in full here — the raw hook output
truncated them at the first space through my `awk` formatting.)

**582 bytes on the wire for a push that places an 8 MiB blob at a new path.** Three things worth stating from
this table:

1. **A and B agree.** A "tree walk" implemented as a *reachability delta* (`--not` the refs the remote already
   has) is OID-based, so it excludes the already-present blob for exactly the same reason the packer does. The
   ticket's dichotomy is really a trichotomy, and only design C differs.
2. **Only C — a full snapshot walk of the new tip — would reject a conflict copy.**
3. **[inference]** C is implausible as a production design on its own terms, independent of the GitHub evidence:
   it re-checks the entire tree on every push, so a single over-limit blob anywhere in a repository would make
   *every subsequent push* to that repository fail, including pushes that touch nothing near it. That is a
   permanently-bricked repository, and it is not a behaviour anyone reports. Flagged as inference — §3 settles it
   by measurement instead.

### 2.6 A fourth design, and why it mattered

There is a fourth design the ticket did not name and which the local experiment cannot rule out: **walk the
trees that were received, and size-check every blob entry they name, whether or not that blob was received.**
It is cheap, bounded by the delta, naturally produces a path in its error message — and it *would* reject a
conflict copy, because the received `conflicts` tree names the 8 MiB blob at its new path. This was the live
risk to #14 and it is why §3 was run against real GitHub rather than settled by argument. **[verified]** in
§3.2: GitHub does not do this either.

---

## 3. Half B — what GitHub actually inspects. **Verified against live github.com.**

### 3.1 Why the 50 MiB threshold is the right instrument

The discriminating scenario needs an over-limit blob to be *already present on the remote*. At the 100 MiB
threshold that state is unreachable — GitHub will not accept the blob in the first place. The 50 MiB warning
threshold reaches it, and GitHub documents the two thresholds as one check:

**[primary]** *"Only files larger than 50 MB will be checked against the Git push limit."*
(<https://docs.github.com/en/enterprise/2.13/admin/installation/setting-git-push-limits>)

**[verified]** and, independently, both thresholds emit the same `GH001` diagnostic from the same pre-receive
hook — see the 50 MiB output in §3.2 and the 100 MiB output in §3.6. Same code, same message id, two severities.

### 3.2 The experiment

Private throwaway repo `andyroberts2/obsync-sizecheck-probe`, pushed over SSH.

**Step 1 — establish a 60 MiB blob on the remote.**

```sh
head -c 62914560 /dev/urandom > big.bin
echo hi > README.md
git add . && git commit -q -m "add 60 MiB big.bin"
git push --verbose -u origin main
```

```
remote: warning: See https://gh.io/lfs for more information.
remote: warning: File big.bin is 60.00 MB; this is larger than GitHub's recommended maximum file size of 50.00 MB
remote: warning: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.
 * [new branch]      main -> main
```

The blob is now on the remote. `2eb04aea95d0c591f1635c1efd83cb07563fb4da`, 62914560 bytes.

**Step 2 — the conflict copy. Same blob, new path.**

```sh
cp big.bin "conflicts/big (conflicted copy).bin"
git add "conflicts/big (conflicted copy).bin"
git commit -q -m "conflict copy: same blob, new path"
git rev-list --objects HEAD --not origin/main
git push --verbose origin main
```

Objects git will send:

```
58e6ff57cb3d66c7ff3a6166ddd2cb9649535cbb
acaba45f28bfebe2fc9a9d900b5d975463e2b6f5
a371d5c099fb81fd28dee52865529a58232f7374 conflicts
```

Push output, in full:

```
Pushing to github.com:andyroberts2/obsync-sizecheck-probe.git
To github.com:andyroberts2/obsync-sizecheck-probe.git
   fb3cca3..58e6ff5  main -> main
updating local tracking ref 'refs/remotes/origin/main'
```

**[verified] Silence.** No warning, no `GH001`. The pushed commit's tree holds a 60 MiB blob at
`conflicts/big (conflicted copy).bin`, and the *received* `conflicts` tree names it there. GitHub said nothing.
This rules out design C **and** design §2.6 simultaneously.

**Step 3 — `git mv` of the original.**

```sh
git mv big.bin renamed-big.bin
git commit -q -m "rename big.bin -> renamed-big.bin"
git push --verbose origin main
```

Objects sent: one commit, one tree. Push output:

```
   58e6ff5..905d6b6  main -> main
```

**[verified] Silence again.** A 60 MiB file appearing at a path it has never occupied before is not flagged,
because its bytes did not move.

**Step 4 — a brand-new ref whose tip holds the big blobs.**

The new-ref case is worth separating: `old` is all-zeros, so a checker written as `rev-list --objects $new`
without a `--not` would enumerate the entire history and see everything.

```sh
git checkout -q -b probe-branch
echo change >> README.md
git commit -q -am "tiny change on a new branch; tip still holds the 60 MiB blob"
git ls-tree -r -l HEAD
git push --verbose origin probe-branch
```

`ls-tree` of the tip — what any snapshot walk would see:

```
  10 bytes  README.md
  62914560 bytes  conflicts/big
  62914560 bytes  renamed-big.bin
```

(`conflicts/big` is truncated by the `awk` field split in my formatting one-liner — the real path is
`conflicts/big (conflicted copy).bin`, which contains spaces. The sizes are the point and they are exact.)

Push output:

```
 * [new branch]      probe-branch -> probe-branch
```

**[verified] Silence.** Two 60 MiB blobs at the tip of a ref the remote has never seen, and no complaint.

### 3.3 The other direction — a blob in the pack but *not* in the tip's tree

The three results above show the check is silent on something every tree walk would catch. This one shows it
fires on something no tree walk of the tip would catch — which is what pins it to received objects specifically,
rather than merely "not design C".

```sh
git checkout -q -B probe2 empty
head -c 62914560 /dev/urandom > interim.bin
git add interim.bin && git commit -q -m "add 60 MiB interim.bin"
git rm -q interim.bin && git commit -q -m "delete interim.bin"
git ls-tree -r -l HEAD          # tip holds ONLY README.md, 68 bytes
git push --verbose origin probe2
```

```
remote: warning: See https://gh.io/lfs for more information.
remote: warning: File interim.bin is 60.00 MB; this is larger than GitHub's recommended maximum file size of 50.00 MB
remote: warning: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.
 * [new branch]      probe2 -> probe2
```

**[verified]** GitHub named `interim.bin` — a path that does not exist in the pushed tip's tree, at any commit
the ref points to. The blob was in the pack. That is what it inspected.

This is also the mechanical explanation for the well-known behaviour that deleting a large file in a later commit
does not un-block a push, and that the remedy is a history rewrite: the remedy works because rewriting makes the
blob unreachable from the pushed ref, so the packer stops sending it. **[primary]** GitHub's guidance is
consistently "remove the file from the repository's history", not "remove it from the tip"
(<https://docs.github.com/en/repositories/working-with-files/managing-large-files/about-large-files-on-github>).

### 3.4 Same result over HTTPS + PAT

All of §3.2–3.3 ran over SSH. obsync authenticates with an HTTPS PAT
([#11](https://github.com/andyroberts2/obsync/issues/11)), so §3.2's step 1 and step 2 were repeated over smart
HTTP against the same repository, cloned as `https://x-access-token:$TOKEN@github.com/...`.

Step 1, a fresh 60 MiB blob:

```
remote: warning: File httpbig.bin is 60.00 MB; this is larger than GitHub's recommended maximum file size of 50.00 MB
remote: warning: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.
   083feb8..8a5e75f  HEAD -> main
```

Step 2, the same blob at a new path — `git rev-list --objects HEAD --not origin/main` returns one commit and
two trees, and the push reports its own body size:

```
Pushing to https://github.com/andyroberts2/obsync-sizecheck-probe.git
POST git-receive-pack (575 bytes)
To https://github.com/andyroberts2/obsync-sizecheck-probe.git
   8a5e75f..48ea729  HEAD -> main
```

**[verified] 575 bytes of request body for a commit placing a 60 MiB file at a new path, and no size complaint.**
Identical to the SSH result. The transport does not change the answer, which is expected — **[primary]**
push always negotiates at protocol v0 regardless of transport (§6.1).

### 3.5 The "it names a path" objection, resolved

The ticket flagged, correctly, that `File big.bin is 60.00 MB` naming a *path* is suggestive of a tree walk but
not conclusive, since packs carry path name hints for delta compression. Both halves of that caution can now be
retired:

- **The name-hint theory is not needed.** In every case where GitHub named a path, it had also received the
  *tree* that names that path — trees are cheap and are always in the pack when a commit is. Resolving blob → path
  from received trees alone is trivial and requires no walk beyond the received set. **[verified]** by §3.3: the
  path GitHub named, `interim.bin`, exists only in a received tree, not at the tip.
- **Naming a path is not evidence of a tree walk at all.** §3.2 is the proof: GitHub had a received tree naming a
  60 MiB blob at a new path, and stayed silent. It names paths for blobs it received; it does not check blobs it
  did not receive, whatever the trees say.

### 3.6 The 100 MiB block, for the record

```sh
head -c 115343360 /dev/urandom > toobig.bin
git add toobig.bin && git commit -q -m "110 MiB file, should be rejected"
git push --verbose origin main
```

```
remote: error: Trace: 3399212aeeb25b1b786f368b2f4cf51f596269e8abaa9fd10d59191653b47610
remote: error: See https://gh.io/lfs for more information.
remote: error: File toobig.bin is 110.00 MB; this exceeds GitHub's file size limit of 100.00 MB
remote: error: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.
 ! [remote rejected] main -> main (pre-receive hook declined)
error: failed to push some refs to 'github.com:andyroberts2/obsync-sizecheck-probe.git'
```

**[verified]** Identical `GH001` diagnostic and the same `See https://gh.io/lfs` line as the 50 MiB warning,
differing only in severity (`error:` vs `warning:`) and threshold wording — and `(pre-receive hook declined)`
confirms the check is a pre-receive hook. Together with the **[primary]** *"Only files larger than 50 MB will be
checked against the Git push limit"*, this closes the one gap between the instrument and the target: the
negative results in §3.2–3.5 are results about the same check that enforces the 100 MiB block.

**Residual uncertainty, stated plainly.** The one step not directly measured is the 100 MiB block being silent on
an already-present over-limit blob, because that state cannot be constructed on github.com. It rests on the two
thresholds being one check — which is **[primary]** documented and **[verified]** consistent in message shape,
but not itself observed at 100 MiB. I regard this as settled; a reader who does not can settle it on a GitHub
Enterprise Server instance, where the "Repository upload limit" is admin-configurable and the limit can be
*lowered* below the size of a blob already present.

### 3.7 GitHub's documented wording, and why it reads the way it does

**[primary]** *"If you attempt to add or update a file that is larger than 50 MiB, you will receive a warning from
Git."*
(<https://docs.github.com/en/repositories/working-with-files/managing-large-files/about-large-files-on-github>)

**[primary]** *"When pushing to your GitHub Enterprise Server instance, you'll receive a warning or error message
if you either add a new file or update an existing file that is larger than 50 MB."*
(<https://docs.github.com/en/enterprise/2.13/user/articles/conditions-for-large-files>)

The ticket read "add or update a file" as leaning toward a tree walk. **[inference]** In light of §3, the phrase
is better read as prose for "introduce new bytes": adding and updating are precisely the two operations that
produce a blob the remote does not have. A conflict copy is neither — it is a third operation the sentence does
not contemplate, and one that produces no new bytes.

Two further **[primary]** vocabulary signals, worth noting only as corroboration:

- The admin control is named **"Repository upload limit"**, and the value it takes is a **"maximum object size"**
  (<https://docs.github.com/en/enterprise/2.13/admin/installation/setting-git-push-limits>). Both are transfer
  and object-graph language, not path language.
- **[primary]** *"Single Object Size — The recommended maximum limit is 1MB. This is enforced at 100MB."*
  (<https://docs.github.com/en/repositories/creating-and-managing-repositories/repository-limits>) — again the
  unit is the *object*, not the file entry.

---

## 4. Limits that are not per-blob — the other failure shape

This is the part of the question a tree walk would never have reached, and it is the part obsync's per-file
ceiling cannot predict either. These fail the **whole push**, not one path, and no per-file ceiling — the
remote's or obsync's — is a guard against them.

**GitHub.com [primary]**, <https://docs.github.com/en/repositories/creating-and-managing-repositories/repository-limits>:

| Limit | Value |
| --- | --- |
| Repository on-disk size | 10 GB |
| Single object size | recommended 1 MB, **enforced at 100 MB** |
| Push size | **enforced at 2 GB** |
| Push rate | recommended max 6 pushes/minute/repository |
| Directory width / depth | 3,000 entries / 50 deep |

**[primary]** The push limit is a **pack** limit, not a sum of file sizes:
*"GitHub has a maximum 2 GiB limit for a single push"*, failing with
`remote: fatal: pack exceeds maximum allowed size`, and the documented remedy is to split the push into several
smaller pushes (<https://docs.github.com/en/get-started/using-git/troubleshooting-the-2-gb-push-limit>).

**[inference]** Two of these are live for obsync in ways the per-file ceiling is not:

- The **6 pushes/minute** recommendation is a rate, and obsync is a loop. Whatever obsync's push cadence ends up
  being, this is the documented number it is being measured against — and it is a *recommendation*, so exceeding
  it produces degradation or throttling rather than a clean rejection.
- The **10 GB repository** and **2 GiB push** limits are the realistic ways a vault full of attachments actually
  breaks a remote, and both are invisible to a per-file ceiling. They also fail differently: a pack-size failure
  is arguably *transient-ish* (split the push and it succeeds), where an over-ceiling blob is permanent until the
  history is rewritten. That distinction belongs to the permanent-vs-transient push-failure work
  ([#18](https://github.com/andyroberts2/obsync/issues/18)), not here.

**GitHub Enterprise Server** additionally makes the per-blob limit **admin-configurable** — **[primary]** *"By
default, GitHub Enterprise Server blocks files larger than 100 MiB. However, a site administrator can configure a
different limit for your GitHub Enterprise Server instance."*
(<https://docs.github.com/en/enterprise-server@3.17/repositories/working-with-files/managing-large-files/about-large-files-on-github>),
set via *"Under 'Repository upload limit', use the drop-down menu and click a maximum object size"*
(<https://docs.github.com/en/enterprise/2.13/admin/installation/setting-git-push-limits>). The current-version
page carries the same policy under "Enforcing a policy for Git push limits" — **[primary]** *"By default, when
you enforce repository upload limits, people cannot add or update files larger than 100 MB"*
(<https://docs.github.com/en/enterprise-server@3.17/admin/policies/enforcing-policies-for-your-enterprise/enforcing-repository-management-policies-in-your-enterprise>).
So even for a GitHub remote, 100 MiB is a default and not a constant — obsync must not hard-code it.

---

## 5. Does the answer generalise?

**Yes — and for GitLab and Gitea it generalises for two different reasons, neither of which is a tree walk.**
No remote was found, anywhere, that validates by walking the pushed commits' trees.

### 5.1 GitLab — a per-blob check exists, and it is explicitly OID-based

GitLab does have a push-time per-blob size check. **[primary]** Two limits share one code path:

- **Push rule `max_file_size`** (MB, `0` = unlimited), Premium/Ultimate, per project — *"Added or updated files
  must not exceed this file size (in MB)… Files tracked by Git LFS are exempted"*
  (<https://docs.gitlab.com/user/project/repository/push_rules/>).
- **Plan limit `file_size_limit_mb` = 100 MiB** on GitLab.com Free — *"A 100 MiB per-file limit applies when
  pushing new files to any project in the Free tier"* (<https://docs.gitlab.com/user/free_push_limit/>). Gated
  on `Gitlab::Saas.feature_available?(:instance_push_limit)`, so **SaaS-only and inert on self-managed**.

**[primary]** The chain is `Gitlab::Checks::ChangesAccess#bulk_access_checks!` →
`Gitlab::Checks::FileSizeLimitCheck` → the EE override in
`ee/lib/ee/gitlab/checks/file_size_limit_check.rb` → `HookEnvironmentAwareAnyOversizedBlobs`, which branches:

```ruby
def find(timeout: nil)
  if ignore_alternate_directories?            # GIT_OBJECT_DIRECTORY_RELATIVE set
    oversized_blobs(timeout: timeout)         # A — the real push path
  else
    any_oversize_blobs.find(timeout: timeout) # B
  end
end
```

- **Branch A**, the production path, calls Gitaly `ListAllBlobs` with
  `git_alternate_object_directories` cleared (`lib/gitlab/gitaly_client/blob_service.rb`), so Gitaly sees **only
  the quarantine directory** — implemented as `git cat-file --batch-all-objects`
  (`internal/gitaly/service/blob/blobs.go`). It then explicitly drops anything the main object database already
  holds:

  ```ruby
  blobs.reject { |blob| map_blob_id_to_existence[blob.id].present? }
  ```

- **Branch B** is `git rev-list --objects --not --all --not <newrevs>`
  (`lib/gitlab/git/repository.rb:469-479`) — the reachability delta, design B from §2.5.

**Both branches are OID set-differences over the received objects. Neither is `ls-tree -r`.** Branch A does not
merely *fail* to see an already-present blob; it has a line of code whose entire job is to remove it.

**[primary]** corroboration from the user-visible error: GitLab's rejection hands you blob **OIDs** and tells you
to run `git ls-tree -r HEAD | grep $BLOB_ID` *yourself* to find the path. The check never resolved a path,
because it never looked at a tree. (Contrast `Gitlab::Checks::DiffCheck`, which *is* path-aware via
`find_changed_paths` — that is how the **filename** push rule works. The **size** rule does not use it.)

**[inference]**, flagged as such: an already-present oversized blob placed at a new path is not caught by
GitLab. This follows directly from the `reject` above rather than from a measurement — the delegated search
found no upstream GitLab issue acknowledging the behaviour either way, so treat it as code-evident but
unacknowledged. It was not reproduced against a live GitLab instance.

### 5.2 Gitea — no per-blob push limit exists at all

**[primary]** `routers/private/hook_pre_receive.go` checks write permission, default-branch deletion, protected
branches, force-push, signed commits, protected file *patterns*, push allowlists, and merge permissions
(`preReceiveBranch`, `preReceiveTag`, `preReceiveFor`). There is no size check anywhere in it, and unprotected
branches return early with essentially no checks at all. `routers/web/repo/githttp.go` pipes the request body
unbounded into `git receive-pack`.

Every `*MAX_SIZE` knob Gitea has is off the push path: `LFS_MAX_FILE_SIZE` (the LFS HTTP batch API,
`services/lfs/server.go`), `[repository.upload] FILE_MAX_SIZE` (web UI only — and the source carries a literal
`// FIXME: need to check the file size according to setting.Repository.Upload.FileMaxSize`),
`[repository.release] FILE_MAX_SIZE`, `[attachment] MAX_SIZE`.

**[primary]** This is a maintainer position, not an oversight, and it has held for five years:
[go-gitea/gitea#13712](https://github.com/go-gitea/gitea/issues/13712) was closed in two days (*"Making this a
default hook I'm not sure"*), and [#22567](https://github.com/go-gitea/gitea/issues/22567) remains open with the
guidance to enable per-repo git hooks yourself. No PR adding one has ever merged.

**Forgejo** adds `checkQuota`/`quotaExceeded` to pre-receive, but **[primary]**
`models/quota/limit_subject.go` shows every quota subject is an **aggregate storage total**
(`size:repos:all`, `size:git:lfs`, …). There is no per-file subject. So Forgejo has a quota, not a blob ceiling
— which is §5.3's shape, not this one.

### 5.3 Limits that are not per-blob, on self-hosted remotes

This is the different failure shape the ticket asked about, and it is the one that actually applies to a
self-hosted obsync remote.

- **`receive.maxInputSize`** — git's own pack ceiling. **[primary]** enforced **incrementally** in
  `builtin/index-pack.c` (`consumed_bytes > max_input_size` → `die(_("pack exceeds maximum allowed size (%s)"))`),
  so the client uploads until it crosses the line and is killed mid-stream. Not a pre-flight rejection.
- **GitLab's "max push size" *is* that config**, injected per-request by Rails —
  `lib/api/internal/base.rb` (SSH) and `lib/gitlab/workhorse.rb` (HTTP) both append
  `receive.maxInputSize=#{receive_max_input_size.megabytes}` to the git config options. **[primary]** Gitaly
  itself has no byte limit of its own (`limithandler` is concurrency and rate, not bytes). **Self-managed
  default is unset** — `db/structure.sql` declares `receive_max_input_size integer,` with no `DEFAULT`, so NULL
  → `0` → unlimited. **GitLab.com is 5000 MiB**, plus a separate **Cloudflare 5 GiB per-HTTP-request** cap.
- **GitLab `repository_size_limit`** — a storage quota, instance default `0`, inherited project > group >
  instance; 10 GB Free / 500 GiB Premium on GitLab.com. **[primary]** Note that
  `Gitlab::GitAccess#check_changes_size` computes it from the *identical* delta —
  `['--not','--all','--not'] + newrevs` summed — which for a quota is arguably the correct unit, since a
  re-pathed blob genuinely stores no new bytes.
- **Reverse-proxy body limits.** nginx's stock `client_max_body_size` is 1 MB, but **[primary]** GitLab
  Omnibus overrides it to `0` (unlimited) — `nginx_helper.rb` sets `'client_max_body_size' => 0`, with the
  template comment *"Or if you want to accept large git objects over http"*. So a stock Linux-package GitLab
  does **not** cap pushes at the proxy; a hand-rolled reverse proxy in front of Gitea very well might, and it
  surfaces as **HTTP 413**, not as a git-level message.
- **Gitea writes no `receive.*` size config** at all (`modules/git/config.go` writes only `core.quotePath`,
  `receive.advertisePushOptions`, `core.commitGraph`, `receive.procReceiveRefs`, `core.longpaths`,
  `core.protectNTFS`, `uploadpack.*`), though an admin can inject `receive.maxInputSize` through the
  `[git.config]` passthrough.

### 5.4 What generalises, and what obsync should require

**[verified]** for GitHub (§3). **[primary, code-level]** for GitLab and Gitea. Every implementation examined
decides from the set of objects the push introduces, and the two candidate designs that would break a conflict
copy — a full snapshot walk (§2.5 design C) and a received-tree walk (§2.6) — were found in no product.

**[inference]** The reason this converges is structural rather than coincidental: `receive-pack` hands a hook the
quarantine directory and nothing else convenient (§1), so the received-object set is the cheap thing to check and
a full tree walk is the expensive thing. A full walk would also re-reject content the remote already accepted,
which is a bricked repository (§2.5).

For the spec's remote requirements, the honest statement is therefore: **obsync requires a remote whose size
enforcement is per-received-object. That is what GitHub, GitLab, and Gitea all do.** The realistic ways a
self-hosted obsync remote actually refuses a push are pack-size (`receive.maxInputSize`, GitLab's push limit),
storage quota (GitLab, Forgejo), or a proxy 413 — all of which fail the whole push regardless of any per-file
ceiling, and none of which obsync's ceiling can predict.

---

## 6. Can a client discover the remote's limit in advance?

**No. Not from the git protocol, and not reliably from any API a sync tool would hold a token for.**

### 6.1 The wire protocol carries nothing — **[verified]** against live github.com

`receive-pack`'s capability advertisement is the only thing a server tells a pushing client before the client
decides what to send. Captured from github.com over SSH with `GIT_TRACE_PACKET=1 git push --dry-run`:

```
packet: push< 083feb87... refs/heads/main\0report-status report-status-v2 delete-refs side-band-64k
        ofs-delta atomic object-format=sha1 quiet
        agent=github/spokes-receive-pack-687be0241dd8aec81f41d987816db233191a3f99
        session-id=9DE4:1CABD:9AE88:14399A:6A8F11CE push-options
```

That is the complete list. `report-status`, `report-status-v2`, `delete-refs`, `side-band-64k`, `ofs-delta`,
`atomic`, `object-format`, `quiet`, `agent`, `session-id`, `push-options`. **Not one of them carries a size,
a quota, or a limit of any kind.**

**[primary]** That advertisement is built from a fixed literal in `builtin/receive-pack.c` (`show_ref()`) —
`"report-status report-status-v2 delete-refs side-band-64k quiet"` plus conditional `atomic`, `ofs-delta`,
`push-cert`, `push-options`, `session-id`, `object-format`, `agent`. There is no code path that could emit a
size. The canonical enumeration in `Documentation/gitprotocol-capabilities.adoc` agrees, and states that a
*"server MUST NOT advertise capabilities it does not understand"* — so a vendor extension is not permitted
either (<https://github.com/git/git/blob/master/Documentation/gitprotocol-capabilities.adoc>).

**[primary] Protocol v2 does not enter into it: push has no v2 implementation.** `builtin/receive-pack.c`:

```c
case protocol_v2:
    /*
     * push support for protocol v2 has not been implemented yet,
     * so ignore the request to use v2 and fallback to using v0.
     */
    break;
```

So the v0 advertisement above is what obsync will always see on push, whatever `protocol.version` is set to.
For completeness, the v2 fetch-side advertisement from the same repository,
`GIT_TRACE_PACKET=1 git -c protocol.version=2 ls-remote`:

```
packet: ls-remote< version 2
packet: ls-remote< agent=git/github-9955570c15e5-Linux
packet: ls-remote< ls-refs=unborn
packet: ls-remote< fetch=shallow wait-for-done filter
packet: ls-remote< server-option
packet: ls-remote< object-format=sha1
```

Also complete, also nothing. **[primary]** `server-option` is client→server — *"any number of server specific
options can be included in a **request**"* — so it is not a loophole even on the fetch side; `object-info`
returns the size of objects the server *already has*, not a limit; and `agent` is explicitly disqualified as a
feature-detection channel (*"purely informative… MUST NOT be used to programmatically assume the presence or
absence of particular features"*).

**[primary]** On the server side, git's own pack-size guard is `receive.maxInputSize`, and it is defined purely
as a server-side abort with no advertisement: *"If the size of the incoming pack stream is larger than this
limit, then git-receive-pack will error out, instead of accepting the pack file. If not set or set to 0, then
the size is unlimited."* (<https://git-scm.com/docs/git-config>; **[verified]** verbatim against the local
`git-config(1)` man page at git 2.51.0). No advertisement, no query. The client discovers it by hitting it.

**[verified]** consistent with §4: GitHub's 2 GiB limit surfaces as `remote: fatal: pack exceeds maximum allowed
size` — mid-push, after the bytes have been sent, not before.

### 6.2 What this means for obsync

The size ceiling stays a **configured** number, as
[#11](https://github.com/andyroberts2/obsync/issues/11) has it. There is no version of "we can check" available
at the protocol layer, which is the only layer that is host-agnostic — and obsync is documented against any
remote. Even where a host-specific API exists (§6.3), obsync would need per-host code, a token with the right
scope, and a fallback for every host it does not recognise; the fallback is the configured number, so the
configured number has to exist regardless.

**[inference]** The one thing obsync *can* do without any discovery is read the rejection when it happens.
`report-status-v2` is advertised (§6.1) and carries per-ref status plus server messages, which is how
`GH001: Large files detected` and `pre-receive hook declined` reach the client at all. That is a matter for
#18's permanent-vs-transient classification, not for the ceiling.

### 6.3 Host APIs — one real exception, and it is narrow

**GitHub REST/GraphQL: absent.** Checked against the machine-readable schemas rather than rendered pages
(<https://github.com/github/rest-api-description>, <https://docs.github.com/public/fpt/schema.docs.graphql>).
The `full-repository` schema's 105 properties contain exactly one size field, `size` ("The size of the
repository, in kilobytes") — a usage figure, not a limit. `GET /meta` returns key fingerprints and IP ranges
only. GraphQL `Repository` offers `diskUsage` and `planFeatures`, and `RepositoryPlanFeatures` contains only
`codeowners`, `draftPullRequests`, `maximumAssignees`, `maximumManualReviewRequests` — no file-size field
despite the type's name. No documented response header conveys a size limit; the documented headers are the
`x-ratelimit-*` family and `retry-after`.

**The one genuine exception — GitHub ruleset push rules.** **[primary]**
`GET /repos/{owner}/{repo}/rules/branches/{branch}` (<https://docs.github.com/en/rest/repos/rules>) returns the
active rules for a branch, including `repository-rule-max-file-size` carrying
`parameters.max_file_size` — an integer in MB, minimum 1, **maximum 100**, documented as not applying to Git
LFS. It is readable before pushing, and the endpoint answers for branches that do not exist yet. Three caveats
that between them stop this from changing obsync's config surface:

1. Rulesets are a Team/Enterprise feature, so most obsync users will have none.
2. An empty array means *"no ruleset configured"*, **not** *"no limit"* — the platform's own 100 MiB still
   applies underneath. GitHub's changelog says so directly: current GitHub limits are still enforced on top of
   push rules.
3. It can only ever return a value **at or below** 100 MB, so it can tighten obsync's ceiling but never
   discover a raised one.

**GHES: configurable, readable by nobody.** The admin can set "Repository upload limit" (§4), but the strings
`max_object_size`, "maximum object size", "upload limit" and "push limit" appear **zero times** in the full GHES
3.18 OpenAPI description, and the Management Console `GET /manage/v1/config/settings` schema has no size, limit,
or quota field. The only read path is `ghe-config` over administrative SSH — not something a sidecar has.

**GitLab.** `receive_max_input_size` ("Maximum push size") exists only on `GET /api/v4/application/settings`,
which is **admin-only** — *"You must have administrator access to the instance"*
(<https://docs.gitlab.com/api/settings/>), and `lib/api/settings.rb` guards the class with
`before { authenticated_as_admin! }`. `GET /projects/:id?statistics=true` returns *usage* (`repository_size`,
`lfs_objects_size`) but never the limit. The per-file plan limit `file_size_limit_mb` is exposed by no API at
all. Two things an ordinary token *can* reach:

- **`GET /projects/:id/push_rule` → `max_file_size`** (integer, MB), project-scoped, Premium/Ultimate. The
  closest GitLab analogue to GitHub's ruleset field.
- **`/help/instance_configuration`** — `app/models/instance_configuration.rb` exposes `receive_max_input_size`
  under `size_limits`, and `help_controller.rb` skips authentication. Verified live by the delegated research:
  `https://gitlab.com/help/instance_configuration` returns 200 unauthenticated with a Size Limits table reading
  **Maximum push size 4.883 GiB**. It is **HTML only** — there is no JSON equivalent, so consuming it means
  scraping. Noted for completeness; not something obsync should build on.

GraphQL additionally exposes `Project.actualRepositorySizeLimit` to ordinary users — but that is the storage
quota, not a per-push or per-file limit, and there is no GraphQL field for push size at all.

**Gitea.** `/api/v1/settings/{ui,api,attachment,repository}` are public and unauthenticated, but
`GeneralRepoSettings` is six booleans with no size field, and `/settings/api`'s `default_max_blob_size` is a
read-side cap on API *responses*, not a push limit. Nothing exposes a push-time size limit **because none
exists** (§5.2).

**LFS batch API — a probe, not a query.** **[primary]** the batch response schema is exactly `transfer`,
`objects[]` (`oid`, `size`, `authenticated`, `actions`), `hash_algo` — no limit or quota field
(<https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md>). But the *request* is metadata-only and the
server answers per object, so a client can learn whether an object would be accepted (422 / 413 / 507 / 509)
**before transferring a content byte**. Only relevant if obsync ever grows LFS support; noted so the option is
on record.

### 6.4 One correction to a widely-repeated claim

The delegated research reported that the `GH001` rejection string could not be found in current GitHub
documentation and suggested treating it as folklore. **[verified]** it is not folklore — it is the live wire
format, captured verbatim in §3.2, §3.3 and §3.6 today, at both severities. It is, however, **undocumented**,
which is the useful half of that caution: obsync may observe it but must not pattern-match on it as a contract.
The nearest thing to a documented version is the archived GHES page, which gives the message text without the
`GH001` prefix (<https://docs.github.com/en/enterprise/2.13/user/articles/conditions-for-large-files>).

---

## 7. What this means for obsync

1. **#14's exemption is sound.** **[verified]** A conflict copy of a blob the remote already holds transfers a
   tree and a commit and nothing else (§2), and GitHub does not size-check it (§3). obsync will not build
   unpushable merge commits on this account. #14's resolution can drop its "left open, not resolved here"
   caveat.
2. **The honest unit is the new object, and #14 already found it.** §2.5's table is the mechanical restatement of
   #14's rule: a path holds new bytes iff its oid differs from its oid in both parents. That is the same set the
   packer computes, which is the same set the remote inspects. The size ceiling and the remote's check are
   measuring the same thing, by the same rule, at two ends of the wire.
   `CONTEXT.md`'s **Size ceiling** entry already says this — *"Never applied to bytes the remote already holds,
   which is why a conflict copy is exempt at any size"* (`CONTEXT.md:152-158`). This report confirms that
   wording rather than revising it; no vocabulary change falls out of #19.
3. **The ceiling still cannot predict acceptance, for reasons that have nothing to do with per-file size.**
   §4 and §5: pack-size limits, repository quotas, per-instance overrides, and non-size pre-receive hooks all
   reject pushes that pass any per-file ceiling. #18's design must hold regardless — which is what #19 was
   ticketed as *not* deciding, and that stands.
4. **The ceiling stays configured, not discovered** (§6) — and #11's config surface does not change. The one
   queryable field found, GitHub's ruleset `max_file_size`, can only tighten a ceiling obsync already has, only
   on Team/Enterprise plans, and its absence proves nothing. Every other host either hides the value behind an
   admin token (GitLab) or has no value to hide (Gitea). Any pre-flight check obsync ever adds is an
   optimisation on top of a configured number, never a replacement for one.
5. **For the spec's remote requirements:** obsync requires a remote that enforces size limits on **received
   objects**. **[verified]** true of GitHub; **[primary, code-level]** true of GitLab (which explicitly discards
   already-present blob OIDs before checking) and vacuously true of Gitea (which has no per-blob push check at
   all). No remote was found that validates by walking the pushed commits' trees — §2.5 and §2.6 name the two
   designs that would break conflict copies, and neither exists in any product examined. obsync should state the
   requirement and treat a hypothetical tree-walking remote as unsupported rather than design around it.
6. **The realistic ways a remote actually refuses obsync's push are not per-file at all** (§4, §5.3): a
   pack-size ceiling (`receive.maxInputSize`, GitHub's 2 GiB, GitLab's push limit), a storage quota (GitLab,
   Forgejo), or a reverse-proxy **HTTP 413**. **[primary]** the pack ceiling is enforced *incrementally* inside
   `index-pack`, so the client uploads until it is killed mid-stream — a failure that arrives late, costs the
   full transfer, and is not reported in git's own vocabulary. These fail the whole push and are worth naming
   distinctly in #18's permanent-vs-transient taxonomy: a pack-size failure is *recoverable by splitting the
   push*, where an over-ceiling blob is permanent until history is rewritten.

---

## Appendix — reproducing this

All of §2 is reproducible offline in under a minute with `git` alone; the hook and driver commands are given
inline above in full. §3 requires a GitHub account and roughly 300 MiB of upload.

**Cleanup owed:** the probe repository `andyroberts2/obsync-sizecheck-probe` still exists. Its branches were
deleted and `main` was force-pushed to a single-commit orphan holding one 68-byte file, so it carries no large
objects on any ref, but the repository itself was **not** deleted — that needs the `delete_repo` OAuth scope,
which the researching token did not hold. **It should be deleted by hand.**

**The one unmeasured step**, restated so it is not lost: §3 establishes the answer at the 50 MiB warning
threshold and argues (on **[primary]** documentation plus **[verified]** identical message shape) that the
100 MiB block is the same check. Constructing the discriminating scenario at 100 MiB is impossible on
github.com, because github.com will not accept the blob that the scenario requires to be already present. A
GitHub Enterprise Server instance would settle it directly: push a blob, then *lower* the admin-configurable
"Repository upload limit" beneath it, then re-path the blob.
