# Security policy

obsync runs unattended with a credential that can write to a repository, beside
a vault whose contents may be the only copy that has ever existed. Please report
anything affecting either of those privately, before it is public.

## Reporting a vulnerability

**Use GitHub's private vulnerability reporting**:
[open a report](https://github.com/andyroberts2/obsync/security/advisories/new)
from the repository's **Security** tab. It is private to the maintainers, it
takes an attachment, and it turns into a published advisory if the report
becomes one.

**Please do not open a public issue for a vulnerability.** Everything else — a
bug, a wrong number, a documentation defect — belongs in a public issue.

You should get an acknowledgement within a week. obsync is one person's project,
so a fix takes as long as it takes; the advisory is where the state of it is
recorded.

## What is in scope

Anything that can lose or expose a vault, or misuse the credential obsync holds:

- The credential reaching a URL, an argv, a log line, `git remote -v`, or any
  file obsync writes.
- obsync writing outside the paths it declares in
  [`docs/interface.md`](docs/interface.md#4-what-obsync-writes-into-the-vault).
- obsync doing anything on the README's never-list — a force-push, a rebase, a
  re-clone, a discarded commit.
- A path in the container image: the base image, the Go module dependencies, or
  the build.

## Versions

obsync is 0.x, and only the most recent release is supported. A security fix is
a new patch release; a base-image CVE bump is one too.
