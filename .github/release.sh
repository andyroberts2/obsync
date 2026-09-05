#!/usr/bin/env bash
#
# What one pushed annotated tag releases, and the gate that decides whether it
# may be released at all (§12, #43).
#
# usage: release.sh <tag> [notes-file]
#
# Reads the annotated tag in the repository it is run from, and either
#
#   * refuses, non-zero, saying what is wrong and what to write instead; or
#   * prints `version=` and `tags=` on stdout for the workflow to consume, and
#     writes the human's half of the release notes to <notes-file>.
#
# The gate it carries is the one §12 calls a verification obligation that is
# surface rather than sequence: **a release whose `docs/interface.md` moved
# since the previous tag, with an empty surface change section, is refused.**
# Mechanical, with no generation. The inversion is the whole point — the page
# does not have to be correct by construction, it has to be impossible to change
# silently.
#
# It lives in a file rather than inline in the workflow because a check nothing
# can run is a check nobody can trust: `release_test.go` drives this script
# against real repositories with real annotated tags, and asserts on its exit
# status the way every other test here asserts on obsync's.

set -euo pipefail

# The one page the version makes its promise over (§10). Named once.
SURFACE_PAGE="docs/interface.md"

die() {
	printf 'release: %s\n' "$1" >&2
	exit 1
}

tag="${1:?usage: release.sh <tag> [notes-file]}"
notes_file="${2:-}"

# The image the release publishes. Taken from the environment rather than
# spelled here, because the workflow reads it off `github.repository` and a
# second spelling is a second thing to keep in step.
IMAGE="${IMAGE:?IMAGE must name the image being published, e.g. ghcr.io/owner/obsync}"

# ---------------------------------------------------------------------------
# The tag
# ---------------------------------------------------------------------------

# **Annotated, or there is no release.** §12 cuts a release with a pushed
# annotated tag and nothing else, and the reason is not ceremony: the tag's
# message is where the human writes the surface change section, and a
# lightweight tag has no message to write it in.
ref="refs/tags/$tag"
kind="$(git cat-file -t "$ref" 2>/dev/null || true)"
case "$kind" in
tag) ;;
"") die "there is no tag $tag in this repository" ;;
*) die "$tag is a lightweight tag. A release is cut by a pushed *annotated* tag, because the tag
       message is where the release says what moved on the declared surface:

         git tag --annotate $tag" ;;
esac

# vX.Y.Z and nothing else. A pipeline that publishes whatever it is handed will
# one day publish a v1.0.0-rc1 as `latest`, `1.0` and `1`, which is the
# floating-tag failure §12 rejects wearing different clothes. Refusing a tag
# shape this does not understand costs a re-tag; the alternative costs the
# premise the immutable tag stands on.
case "$tag" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) die "$tag is not a release tag. obsync is versioned vMAJOR.MINOR.PATCH over $SURFACE_PAGE, and
       this pipeline publishes nothing it cannot read a version out of." ;;
esac
version="${tag#v}"
case "$version" in
*[!0-9.]* | *..* | .* | *. | *.*.*.*)
	die "$tag carries no plain MAJOR.MINOR.PATCH version"
	;;
esac

major="${version%%.*}"
minor="${version#*.}"
minor="${minor%%.*}"
patch="${version##*.}"
[ -n "$major" ] && [ -n "$minor" ] && [ -n "$patch" ] ||
	die "$tag carries no plain MAJOR.MINOR.PATCH version"

# ---------------------------------------------------------------------------
# The tag message, and the surface change section
# ---------------------------------------------------------------------------

subject="$(git for-each-ref --format='%(contents:subject)' "$ref")"
body="$(git for-each-ref --format='%(contents:body)' "$ref")"

# The section marker is not a Markdown heading, and that is measured rather than
# chosen. **git strips every line beginning with `#` from an annotated tag
# message** — `git tag`'s default cleanup is `strip`, which removes commentary,
# and it applies to `-m` and `-F` alike. Measured at both matrix points, 2.38.5
# and 2.52.0. A `## Surface changes` written into a tag message is simply not in
# the tag afterwards, so the mandatory section would vanish at the one moment it
# is being written. A bold line survives, and renders as a section head in the
# notes GitHub assembles.
#
# Other spellings are read anyway, because a release refused over a `#` is a
# release refused over the very thing git already took away.
MARKER='**Surface changes**'

is_marker() {
	local key
	key="$(printf '%s' "$1" |
		sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
			-e 's/^#\{1,\}[[:space:]]*//' -e 's/^\*\{1,2\}//' -e 's/\*\{1,2\}$//' \
			-e 's/[[:space:]]*:$//' |
		tr '[:upper:]' '[:lower:]')"
	[ "$key" = "surface changes" ]
}

# The section runs from its marker to the end of the message. There is nothing
# after it to parse, which is what makes "empty" unambiguous — and a tag message
# ending on the marker line is exactly §12's *present and empty when nothing
# moved*.
marked=no
section=""
while IFS= read -r line; do
	if [ "$marked" = no ]; then
		if is_marker "$line"; then marked=yes; fi
		continue
	fi
	section="$section$line
"
done <<EOF
$body
EOF

if [ "$marked" = no ]; then
	die "the tag message for $tag carries no surface change section, and it is mandatory — present
       and empty when nothing moved. Generated notes are commit titles, and they cannot answer the
       one question the version was made to promise: whether what an operator set and pinned still
       means what it meant. Write it into the tag message, last:

         $MARKER
         - <what moved on $SURFACE_PAGE, or nothing at all if nothing did>"
fi

# ---------------------------------------------------------------------------
# The gate
# ---------------------------------------------------------------------------

# The previous release, and the diff over the one page the version promises
# about. With no previous tag there is nothing to diff against and the whole
# surface is new, which is a surface change by any reading — so the first
# release owes the same sentence every later one does.
previous="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$tag^" 2>/dev/null || true)"

moved=yes
changed=""
if [ -n "$previous" ]; then
	moved=no
	# `--quiet` exits 1 on a difference and 128 on anything it could not read,
	# and both are treated as *moved* here. That is this project's own rule
	# applied to a check: an inconclusive answer degrades towards the safe side,
	# and the safe side of this question is being asked to say what changed.
	if ! git diff --quiet "$previous" "$tag" -- "$SURFACE_PAGE"; then
		moved=yes
		changed="$(git diff --stat "$previous" "$tag" -- "$SURFACE_PAGE" |
			sed 's/^/         /')"
	fi
fi

empty=yes
if printf '%s' "$section" | grep -q '[^[:space:]]'; then
	empty=no
fi

if [ "$moved" = yes ] && [ "$empty" = yes ]; then
	since="${previous:-the beginning of the repository}"
	die "$SURFACE_PAGE changed since $since and the surface change section of $tag's message is
       empty. A change to that page is a change to a promise: it is the nine variables, the four
       subcommands, the health contract and what obsync writes into a vault, and SemVer is measured
       over it. Say what moved, in the tag message:

         $MARKER
         - <what moved>
${changed:+
       What moved:

$changed}"
fi

# ---------------------------------------------------------------------------
# What the release publishes
# ---------------------------------------------------------------------------

if [ -n "$notes_file" ]; then
	# The human's half of the notes, verbatim. GitHub pre-pends a release body
	# to the notes it generates from commit titles, so this lands above them and
	# the generated half stays generated (§12).
	printf '%s\n\n%s\n' "$subject" "$body" >"$notes_file"
fi

# §12's tag set: the patch, the minor, the major, and `latest`. All four point
# at the *same* build, pushed in one `docker buildx build --push`, which is the
# whole of what keeps them from becoming several images with one name. The
# rejected alternative is instructive: a scheduled rebuild republishing only the
# floating tags makes `1` and `1.4.2` two different images sharing a name, and
# quietly destroys the premise both the immutable tag and the attestation stand
# on.
#
# The major is published pre-1.0 too, where §12 says there is no meaningful
# floating major. The docs decide what is *quoted* — the reference compose pins
# the current `0.x` line today and `1` from 1.0 — and a pipeline special case
# that fires exactly once, at the most dangerous release there is, is worse than
# a tag nothing points at. `latest` is here for the same reason: people expect
# it, and no document quotes it.
#
# **A floating name is published only by the release that is newest under it**,
# which is that same rejected alternative arriving from the other side. A
# release is not always the newest one: a backport — `v1.3.5` cut from a
# maintenance branch while 1.4.2 is out — is a real release, owed its own
# immutable tag and owed the `1.3` its own line has just advanced. It does not
# own `1` or `latest`, and taking them is worse than the scheduled rebuild §12
# rejects, because the page tells an operator to *pin the floating major*: a `1`
# that moved backwards downgrades every unattended sidecar following it to older
# code on its next pull, with nobody acting and nothing to read about it.
newest_minor=yes   # nothing higher in this MAJOR.MINOR line
newest_major=yes   # nothing higher in this MAJOR
newest_overall=yes # nothing higher anywhere
while IFS= read -r other; do
	other="${other#v}"
	# Only a plain MAJOR.MINOR.PATCH is a release, so only one can hold a
	# floating name away from this tag. A `v1.5.0-rc1` publishes nothing and
	# therefore withholds nothing — the same rule read the other way round.
	case "$other" in
	*[!0-9.]* | *..* | .* | *. | *.*.*.*) continue ;;
	*.*.*) ;;
	*) continue ;;
	esac

	other_major="${other%%.*}"
	other_minor="${other#*.}"
	other_patch="${other_minor##*.}"
	other_minor="${other_minor%%.*}"

	# Numerically throughout: compared as strings, 0.10.0 is older than 0.9.0,
	# and the tenth minor is exactly where a project that has been running
	# unattended for a year finds itself.
	if [ "$other_major" -gt "$major" ]; then
		newest_overall=no
	elif [ "$other_major" -eq "$major" ]; then
		if [ "$other_minor" -gt "$minor" ]; then
			newest_overall=no
			newest_major=no
		elif [ "$other_minor" -eq "$minor" ] && [ "$other_patch" -gt "$patch" ]; then
			newest_overall=no
			newest_major=no
			newest_minor=no
		fi
	fi
done <<EOF
$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*')
EOF

tags="$IMAGE:$version"
held=""
if [ "$newest_minor" = yes ]; then
	tags="$tags,$IMAGE:$major.$minor"
else
	held="$held $major.$minor"
fi
if [ "$newest_major" = yes ]; then
	tags="$tags,$IMAGE:$major"
else
	held="$held $major"
fi
if [ "$newest_overall" = yes ]; then
	tags="$tags,$IMAGE:latest"
else
	held="$held latest"
fi

# stdout is the workflow's `$GITHUB_OUTPUT` and nothing reads it in the log, so
# what was decided is said on stderr too. A release is a thing somebody reads
# the log of exactly once, when it went wrong.
printf 'release: cutting %s from %s; %s since %s\n' "$version" "$tag" \
	"$([ "$moved" = yes ] && printf '%s moved' "$SURFACE_PAGE" || printf '%s did not move' "$SURFACE_PAGE")" \
	"${previous:-the beginning of the repository}" >&2

if [ -n "$held" ]; then
	printf 'release: %s is not the newest release under%s, so those names stay where they are\n' \
		"$version" "$held" >&2
fi

printf 'version=%s\n' "$version"
printf 'moved=%s\n' "$moved"
printf 'tags=%s\n' "$tags"
