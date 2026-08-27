# obsync's container image. This is the artifact the project supports.
#
# The build has two stages. A builder stage compiles a static binary. The final
# stage is an Alpine base that carries git.
#
# git is not an implementation detail here. obsync drives git as a subprocess
# for every operation, so a base image without git is not an option.
#
# Both images are pinned by digest instead of by tag. Alpine moves a release
# tag on every patch, and the git version moves with it. A digest is the only
# pin that holds the git version still. Dependabot moves these two lines, and a
# new base image is a patch release of obsync.

# The builder runs on the machine that does the build and emits a binary for
# the target platform. An arm64 image needs no emulation, and a clean checkout
# rebuilds the image without CI copying a binary in.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS builder

# CGO off makes the binary static. A static binary is the only thing the final
# stage needs beside git.
#
# GOTOOLCHAIN=local keeps the pinned builder pinned. Without it, a `toolchain`
# directive in go.mod can download a different Go version mid-build, and the
# digest above stops describing what compiled obsync.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

WORKDIR /src

# Dependencies first, so a source edit does not download them again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

# VERSION is stamped at link time, and it is what `obsync status` reports. It
# identifies the exact bytes of the image you pinned, so it comes from the
# build rather than from the runtime. The default is `dev`, because a local
# `docker build` is not a release.
ARG VERSION=dev

# -trimpath keeps the builder's paths out of the binary. -s -w drops the symbol
# table, which is weight in an image that carries one binary and git.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/obsync .

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

# git is the runtime. openssh-client is git's transport for the two SSH repo
# forms, and Alpine's git package does not carry one. ca-certificates arrives
# with git, for the https forms.
#
# The git version is not pinned here on purpose. The base digest above pins it,
# and CI asks this image which git it installs rather than reading a number
# somebody typed. A second number is a second thing to keep in step.
RUN apk add --no-cache git openssh-client

# Declared again, because an ARG's scope ends at the next FROM.
ARG VERSION=dev

COPY --from=builder /out/obsync /usr/local/bin/obsync

# The image redistributes Apache-2.0 code, so it carries that licence.
COPY --from=builder /src/LICENSE /usr/local/share/obsync/LICENSE

# obsync runs as an arbitrary UID with no `/etc/passwd` entry, so HOME is named
# here. obsync writes nothing into this directory. Its git config is a
# temporary directory per process, and everything else it writes is inside the
# vault. The directory does not need to be writable.
#
# This is also where you mount an SSH key and a known_hosts file, if your remote
# is an SSH one. That mount is necessary but not sufficient: ssh reads the home
# directory for `~` out of the UID's passwd entry rather than out of HOME, so an
# SSH remote also needs a passwd line for the UID. With no entry at all, ssh
# exits with `No user exists for uid 1000` before it reads any configuration.
# docs/credentials.md carries the whole instruction.
ENV HOME=/home/obsync
RUN mkdir -p /home/obsync

# A default that is not root, so a compose file with no `user:` line does not
# run a container holding a write-scoped credential as UID 0. The pair is
# 1000:1000 because that is ignis's default. Any other UID works.
USER 1000:1000

# Part of the declared surface, parameters included. See docs/interface.md.
# There is no HTTP server and no port: the subcommand is the whole mechanism,
# and a single static binary means no `curl` needs to exist here.
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=2 \
  CMD ["obsync", "healthcheck"]

# Exec form, and no init process. obsync is PID 1, it receives Docker's SIGTERM
# itself, and it waits on every git it spawns. A shell at PID 1 swallows the
# signal that obsync's whole shutdown rule depends on.
ENTRYPOINT ["obsync"]

LABEL org.opencontainers.image.title="obsync" \
      org.opencontainers.image.description="Bidirectional git sync for a self-hosted Obsidian vault, as a Docker sidecar" \
      org.opencontainers.image.source="https://github.com/andyroberts2/obsync" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
