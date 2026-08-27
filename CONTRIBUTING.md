# Contributing

obsync's design is [#21](../../issues/21), its vocabulary is
[`CONTEXT.md`](CONTEXT.md), and its rules for code are
[`.sandcastle/CODING_STANDARDS.md`](.sandcastle/CODING_STANDARDS.md). This file
carries one rule that is not in any of them, because it is a licence obligation
rather than a preference.

## The transcription rule

obsync carries one piece of code it did not write. The credential isolation —
the private per-process `GIT_CONFIG_GLOBAL`, `GIT_CONFIG_NOSYSTEM`, and the
`credential.helper` and `core.askPass` settings written into it — is
**transcribed verbatim** from [`kubernetes/git-sync`](https://github.com/kubernetes/git-sync),
which is Apache-2.0, rather than reimplemented from a reading of it. Being wrong
about credential isolation is more expensive than the duplication.

> **Load-bearing documentation** (§12, [#17](../../issues/17)).
> **The transcription lives in exactly one file, and stays there.**
> [`internal/git/isolation.go`](internal/git/isolation.go) carries the upstream
> Apache-2.0 header, the upstream copyright line, the exact upstream commit, and
> the line range every transcribed fragment came from.
>
> - **Do not scatter it.** A transcribed line moved into a neighbouring package
>   takes the licence obligation with it and leaves nothing behind that says so.
> - **Do not clean it up.** Rewriting it into this project's own idiom is how a
>   file stops being auditable: nobody can then tell which lines carry the
>   obligation and which are obsync's.
> - **Add to it the same way.** Anything else transcribed goes in that file,
>   under a header naming its origin, its licence and its line range.
>
> This is what keeps the obligation **auditable** as well as satisfied, which is
> what was wanted anyway on the part of obsync where being wrong is most
> expensive. Never cut this rule, and never move the file's header away from the
> code it covers.

obsync is licensed [Apache-2.0](LICENSE) for the whole repository rather than
dual-licensed, because that is the licence of the code above.
