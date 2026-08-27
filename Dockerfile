# obsync's container image — the supported artifact (§1, §12).
#
# Two stages: a builder that cross-compiles a static binary, and an alpine base
# carrying git. Distroless is ruled out because it has no git, and git is not an
# implementation detail here — every decision in obsync's design is expressed in
# git plumbing, driven as a subprocess.
#
# Both images are pinned by **digest, never by tag**. Alpine repoints a release
# tag on every patch, git included, so `alpine:3.23` alone does not deliver "the
# git version moves only when we move it" — which is the premise an immutable
# image tag and a build attestation both stand on. Dependabot moves these two
# lines, and a base CVE bump is a patch release.

# The builder always runs on the machine doing the building and emits a binary
# for the platform being built, so an arm64 image costs no emulation and a clean
# checkout still reproduces the image without CI copying a binary in.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS builder

# CGO off is what makes the binary static, which is what lets it be the only
# thing in the final stage besides git.
#
# GOTOOLCHAIN=local is what makes the pinned builder actually pinned: without
# it, a `go` line or a `toolchain` directive above this image's own Go
# downloads another toolchain mid-build and the digest above stops describing
# what compiled obsync. go.mod carries no toolchain directive for that reason
# and the suite fails if one appears; this is the same rule enforced at the one
# place it would actually be violated.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

WORKDIR /src

# The two direct dependencies first, so a source edit does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

# VERSION is stamped at link time and is what `obsync status` reports (§10,
# §12). It is deliberately not derived at runtime: the version's job is to
# identify the bytes of the image an operator pinned, so it has to come from the
# build. It defaults to `dev` because a local `docker build` is not a release.
ARG VERSION=dev

# -trimpath so the binary does not carry this builder's paths, and -s -w because
# a symbol table is weight in an image whose whole size argument is that it
# carries one static binary and git.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/obsync .

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

# git is the runtime, and openssh-client is git's transport for the two SSH repo
# forms §8 accepts — alpine's git package does not carry one. ca-certificates
# arrives with git, for the https forms.
#
# The version is not pinned here and that is deliberate: the base digest above
# is what pins it, and CI *derives* the matrix's upper point by asking this
# image what git it installs rather than by reading a number somebody typed. A
# pinned `git=2.52.0-r0` would be a second number to keep in step, and would
# break the moment alpine rebuilt the same version at -r1.
RUN apk add --no-cache git openssh-client

# Re-declared, because an ARG's scope ends at the next FROM. Same value, one
# --build-arg.
ARG VERSION=dev

COPY --from=builder /out/obsync /usr/local/bin/obsync

# The image redistributes the Apache-2.0 work obsync transcribes its credential
# isolation from, so it carries the licence it is redistributed under (§4(a)).
COPY --from=builder /src/LICENSE /usr/local/share/obsync/LICENSE

# obsync runs as an arbitrary UID with no `/etc/passwd` entry, so HOME cannot
# come from that file. It is named here so that the one thing an operator may
# have to mount into it has a documented place to go: an ssh key and a
# known_hosts, which is how SSH arrives given SSH needs no knobs (§8). Nothing
# obsync writes goes here — its private git config is a per-process temporary
# directory, and everything else it writes is an owned path inside the vault —
# so the directory is not writable, and does not need to be.
ENV HOME=/home/obsync
RUN mkdir -p /home/obsync

# A default that is not root, so an operator who forgets Docker's `user:` line
# does not get a container holding a write-scoped credential as UID 0. 1000:1000
# because that is what ignis defaults to (§8); any other UID works, which is
# what seam 2 checks at 4242.
USER 1000:1000

# Part of the declared surface, parameters included (§9, docs/interface.md).
# There is no HTTP server and no port behind it: the subcommand is the whole
# mechanism, and a single static binary means no `curl` needs to exist here to
# call it.
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=2 \
  CMD ["obsync", "healthcheck"]

# Exec form, and no init process: obsync is PID 1, receives Docker's SIGTERM
# itself, and waits on every git it spawns, so there is no orphan-capable
# process in this container by construction (§1, §8). A shell left at PID 1
# swallows the signal obsync's whole shutdown rule depends on — and measured,
# busybox's own shell execs a simple command and gets out of the way, so that
# harm is one spelling away rather than always present. The exec form is written
# rather than relied upon.
ENTRYPOINT ["obsync"]

LABEL org.opencontainers.image.title="obsync" \
      org.opencontainers.image.description="Bidirectional git sync for a self-hosted Obsidian vault, as a Docker sidecar" \
      org.opencontainers.image.source="https://github.com/andyroberts2/obsync" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
