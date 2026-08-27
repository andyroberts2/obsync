# Credentials

obsync holds a credential that can write to your repository, and it runs
unattended. This page states the smallest credential that works for each remote,
what obsync does with it, and how you find out you got it wrong.

**The secret is always a file, never a variable.** `OBSYNC_TOKEN_FILE` names a
path obsync can read, and obsync re-reads that file every time git asks for a
credential. Rotating a token is therefore a write to the file, with no restart
and no redeploy. There is no `OBSYNC_TOKEN`.

---

## What each remote form needs

These are the repo forms obsync accepts, and the credential each one takes. The
forms themselves are part of the declared surface, and they are stated on
[`interface.md`](interface.md#the-repo-and-the-remote).

| `OBSYNC_REPO` | What obsync needs |
|---|---|
| `https://host/owner/vault.git` | `OBSYNC_TOKEN_FILE`, and `OBSYNC_USERNAME` if the remote wants a particular one |
| `http://host/owner/vault.git` | The same, and [read the warning below](#plain-http) |
| `ssh://git@host/owner/vault.git` | [Two ordinary mounts](#ssh) — the key and `known_hosts`, and a passwd line — and no variable at all |
| `git@host:owner/vault.git` | The same as `ssh://`; the two forms are the same repo to obsync |
| `file:///srv/vaults/vault.git` | Nothing. A path on a mounted filesystem needs no credential |

`OBSYNC_TOKEN_FILE` is required **if and only if** the repo URL is `http://` or
`https://`. Set it for the others and obsync will not use it.

---

## The minimum scope

Give obsync the least a working sync needs. On every remote here that is: read
and write the contents of **one** repository, and nothing else.

| Remote | Credential | Scope | `OBSYNC_USERNAME` |
|---|---|---|---|
| **GitHub** | Fine-grained PAT, scoped to the one repository | `Contents: Read and write`, plus the `Metadata: Read` that GitHub adds for you | Any non-empty value; obsync's default `obsync` is fine |
| **GitHub** | Classic PAT — the fallback rather than the recommendation | `repo`, which grants far more than this, across every repository you can reach | As above |
| **GitLab** | Project access token on the one project | `write_repository` | `oauth2` |
| **Gitea** | Token | Repository write | Your real username |
| **any** | SSH deploy key on the one repository | Write access enabled | — |

**Each provider's own page is the instruction.** A click-by-click walkthrough of
somebody else's UI goes out of date faster than this page can follow it.

- [Managing your personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
  — GitHub
- [Project access tokens](https://docs.gitlab.com/user/project/settings/project_access_tokens/)
  — GitLab
- [Managing deploy keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys)
  — GitHub, for the SSH row

A **fine-grained PAT expires**, and obsync will not remind you. When it does,
obsync keeps committing your vault locally and says so once an hour. Write the
new token into the same file, and the next run publishes everything that
accumulated meanwhile.

### How you know you got it right

**A wrong-scoped token looks exactly like a working one until the first push.**
A credential that can read but not write clones and fetches perfectly. So a
deployment that has never once worked can look, for hours, like a deployment
with nothing to do.

obsync is built so that you find out at the first push rather than the first
time you look, and it does that with no startup probe:

- The first push obsync **attempts** and does not land is an **ERROR
  immediately** and **unhealthy at once**. It is not backed off quietly, because
  nobody has ever seen this deployment work.
- Once a push has succeeded, the same failure becomes ordinary backoff with an
  hourly repeat.

So the check is: **write a note in the vault, wait a minute, and look.**

```bash
docker compose exec obsync obsync status   # says when a push last landed, or that none ever has
docker ps                                  # the health column, once it is past `starting`
git -C /path/to/vault log origin/<branch> --oneline -1
```

**Read `obsync status` first.** Do not read anything into `docker ps` on a
container you have just started. The image's `HEALTHCHECK` has a 120-second
start period, then a 60-second interval and two retries. A brand-new container
therefore reads `starting` for the first two minutes whatever is wrong, and
turns `unhealthy` a couple of minutes after that. `obsync status` answers from
the same record with no such delay.

`obsync status` tells *never attempted* apart from *attempted, never succeeded*.
The first is a quiet vault. The second is a credential problem.

---

## SSH

**SSH needs no knobs.** The URL scheme is what chooses the credential path.
obsync stores and manages neither the key nor the `known_hosts` file, and never
reads or writes either itself. They arrive as ordinary mounts.

The image's `HOME` is `/home/obsync`, which is where ssh's material goes:

```yaml
    volumes:
      - ./secrets/ssh:/home/obsync/.ssh:ro
      - ./secrets/passwd:/etc/passwd:ro
```

`./secrets/ssh` holds the two files ssh expects in a `.ssh` directory:

- **The private key**, mode `0600` and owned by the UID in the compose file's
  `user:` line. ssh refuses to use a private key that is group-readable or
  world-readable, and says so rather than falling back to anything.
- **`known_hosts`**, because obsync runs with no terminal and can never answer
  *"are you sure you want to continue connecting?"*. Produce it once, and check
  the fingerprint against what your host publishes:

  ```bash
  ssh-keyscan github.com > ./secrets/ssh/known_hosts
  ```

> **Load-bearing documentation** ([#12](../../issues/12)).
> **The second mount is the one nobody expects, and leaving it out fails
> silently.** The image bakes no `/etc/passwd` entry for the UID Docker's
> `user:` line names, and **ssh expands `~` out of that entry rather than out of
> `HOME`**.
>
> Measured in obsync's own image (alpine 3.23, OpenSSH 10.2): with no entry at
> all, ssh exits before it reads any configuration — `No user exists for uid
> 1000`. With an entry whose home is somewhere else, ssh looks for the key
> somewhere else. Either way git reports a remote it could not read, obsync
> treats that as an unreachable remote, and an unreachable remote is **healthy
> and quiet for 24 hours** — over a vault that has never once been backed up.
>
> One line for this UID is the whole file, and its home field must be the
> image's `HOME`. Never cut this.
>
> ```
> obsync:x:1000:1000:obsync:/home/obsync:/sbin/nologin
> ```
>
> obsync's own sync loop needs no entry and never will. Its git identity comes
> from its private git config, which is why there is no root entrypoint and no
> `PUID`/`PGID` setting. This mount is ssh's requirement rather than obsync's.

An SSH remote needs **no** `OBSYNC_TOKEN_FILE` and no `OBSYNC_USERNAME`. The
user is in the URL, and `git@host:owner/vault.git` and
`ssh://git@host/owner/vault.git` are the same repo as far as obsync is
concerned.

---

## Plain `http://`

**`http://` sends your credential and your entire vault across the network in
the clear.** obsync logs a WARN at startup naming it, and syncs anyway. This
audience self-hosts, and a remote on a wire you control is a real deployment
rather than a mistake to refuse.

This is the weakest warning in the documentation set, because the startup WARN
fires whether or not anybody read this page. If the remote is reachable from
anything you do not control, use `https://` or `ssh://`.

---

## What obsync does with the secret

Stated so that you can check it rather than trust it:

- **It never reaches a URL.** A token in the remote URL leaks into
  `git remote -v` and into every log line. It also leaks into the check obsync
  makes that `origin` is still the remote it was given.
- **It never reaches an argv**, so DEBUG logging is safe to turn on. DEBUG
  prints every git invocation with its full argv.
- **It never reaches a file obsync wrote**, a socket, or a daemon. obsync is its
  own git credential helper: git asks, obsync reads your file, and obsync prints
  it to git. There is no cache, because a cache exists specifically to *not*
  re-read.
- **The helper's output is never logged, at any level.**
- **obsync never writes your `.git/config`**, so the remote and the identity in
  it stay yours.

Mount the file read-only. obsync only ever reads it.

```yaml
      - ./secrets/obsync-token:/run/secrets/obsync-token:ro
```

A `OBSYNC_TOKEN_FILE` that obsync cannot read **at startup** is a config error.
obsync says so and exits 1, because the problem is decidable from the
environment block alone. The same file becoming unreadable **later** is not a
config error. That is a credential being rotated: obsync keeps committing
locally, and the next run after the file comes back publishes everything.
