package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release pipeline (§12, #43): one pushed annotated tag, a multi-arch image
// in GHCR under four tags, an attestation and an SBOM, and release notes
// carrying a **surface change** section that is present and empty when nothing
// moved.
//
// The gate under test here is the one §12 calls a verification obligation that
// is surface rather than sequence: **CI fails a release if `docs/interface.md`
// changed since the previous tag and the "Surface changes" section is empty.**
// The inversion is the point — the page does not need to be correct by
// construction, it needs to be impossible to change silently.
//
// This is seam 1's discipline rather than a third seam: a real repository with
// real annotated tags, real git underneath, and the shipped check driven
// through its own process boundary, asserted on by its exit status and what it
// said. Nothing here asserts on how it decided.
const releaseGate = ".github/release.sh"

// The image the release publishes to. Passed in rather than baked into the
// gate, because the workflow reads it off `github.repository` and this suite
// checks separately that the reference compose pins the same one.
const releaseImage = "ghcr.io/andyroberts2/obsync"

// releaseRepo is a real repository the gate can be pointed at: real commits,
// real annotated tags, and a real `docs/interface.md` that moves or does not.
type releaseRepo struct {
	t   *testing.T
	dir string
}

func newReleaseRepo(t *testing.T) *releaseRepo {
	t.Helper()

	r := &releaseRepo{t: t, dir: t.TempDir()}
	r.mustGit("init", "--initial-branch=main")
	r.mustGit("config", "user.name", "obsync test")
	r.mustGit("config", "user.email", "obsync@obsync.invalid")
	return r
}

func (r *releaseRepo) mustGit(args ...string) string {
	r.t.Helper()

	out, code := runGit(r.t, r.dir, args...)
	if code != 0 {
		r.t.Fatalf("git %s exited %d: %s", strings.Join(args, " "), code, out)
	}
	return out
}

// write puts a file in the repository and commits it, which is the only way a
// tag can have anything to say about it.
func (r *releaseRepo) write(path, content, subject string) {
	r.t.Helper()

	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("making %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatalf("writing %s: %v", path, err)
	}
	r.mustGit("add", "--", path)
	r.mustGit("commit", "-m", subject)
}

// tag cuts the release the way §12 says a release is cut: one pushed annotated
// tag, whose message is the human's half of the notes.
func (r *releaseRepo) tag(name, message string) {
	r.t.Helper()
	r.mustGit("tag", "--annotate", "--message", message, name)
}

// cut runs the shipped gate against this repository and returns what it said,
// what it wrote into the notes file, and whether it let the release through.
func (r *releaseRepo) cut(tag string) (said, notes string, code int) {
	r.t.Helper()

	gate, err := filepath.Abs(releaseGate)
	if err != nil {
		r.t.Fatalf("finding %s: %v", releaseGate, err)
	}
	// Read in process, for the reason every other test here reads the file it
	// stands on: `go test` keys its result cache on the files a test opens,
	// and a script that reaches this test only through a subprocess is one the
	// cache cannot see changing.
	if _, err := os.ReadFile(gate); err != nil {
		r.t.Fatalf("reading %s: %v", releaseGate, err)
	}

	notesFile := filepath.Join(r.t.TempDir(), "notes.md")
	cmd := exec.Command("bash", gate, tag, notesFile)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"IMAGE="+releaseImage,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		r.t.Fatalf("running %s: %v", releaseGate, err)
	}

	if written, err := os.ReadFile(notesFile); err == nil {
		notes = string(written)
	}
	return out.String(), notes, code
}

// The section marker is not a Markdown heading, and that is a measurement
// rather than a preference. **git strips every line beginning with `#` from an
// annotated tag message**: `git tag`'s default cleanup is `strip`, which
// removes commentary, and it applies to `-m` and `-F` alike. Measured at both
// matrix points, 2.38.5 and 2.52.0, and the two agree — a `## Surface changes`
// written into a tag message is simply not in the tag afterwards, which would
// make the mandatory section vanish at the one moment it is being written. A
// bold line survives, renders as a section head in the notes GitHub builds, and
// is what the gate's own refusal tells a human to write.
const surfaceChangesMarker = "**Surface changes**"

// A tag message that is subject, then the mandatory section, then whatever the
// release has to say about the surface. The section runs to the end: there is
// nothing after it to parse, so "empty" is unambiguous.
func tagMessage(subject string, surfaceChanges ...string) string {
	message := subject + "\n\n" + surfaceChangesMarker + "\n"
	for _, line := range surfaceChanges {
		message += "\n" + line + "\n"
	}
	return message
}

// A surface page that moved without a word said about it is the one thing this
// gate exists to refuse. It is the expensive direction by construction: an
// operator moving `:0.3` to `:0.4` reads the notes to find out whether what
// they set and pinned still means what it meant, and an empty section is an
// answer rather than an absence.
func TestAReleaseThatMovedTheSurfacePageWithNothingToSayAboutItIsRefused(t *testing.T) {
	t.Parallel()

	r := newReleaseRepo(t)
	r.write("docs/interface.md", "# The declared surface\n\nNine variables.\n", "the surface page")
	r.tag("v0.3.0", tagMessage("obsync v0.3.0"))

	r.write("docs/interface.md", "# The declared surface\n\nTen variables.\n", "a tenth variable")
	r.tag("v0.4.0", tagMessage("obsync v0.4.0"))

	said, _, code := r.cut("v0.4.0")

	if code == 0 {
		t.Fatalf("%s let v0.4.0 through with docs/interface.md moved since v0.3.0 and an empty "+
			"Surface changes section: the page does not have to be correct by construction, it has "+
			"to be impossible to change silently (§12)\n\n%s", releaseGate, said)
	}
	if !strings.Contains(said, "docs/interface.md") {
		t.Errorf("%s refused the release without naming the page that moved, which is the whole of "+
			"what the person who has to fix it needs to know:\n\n%s", releaseGate, said)
	}
}

// §12's tag set: the patch, the minor, the major, and `latest`. All four are
// derived from the one version in the tag rather than typed, because a
// scheduled rebuild republishing only the floating tags makes `1` and `1.4.2`
// two different images with one name — which quietly destroys the premise the
// immutable tag and the attestation both stand on.
func TestTheTagSetIsDerivedFromTheVersionRatherThanTyped(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		tag  string
		want []string
	}{
		{"v1.4.2", []string{"1.4.2", "1.4", "1", "latest"}},
		// Pre-1.0 there is no meaningful floating major, and the major tag is
		// published anyway: the docs decide what is quoted, and a special case
		// that fires exactly once — at 1.0, the most dangerous release there
		// is — is worse than a tag nothing points at.
		{"v0.4.1", []string{"0.4.1", "0.4", "0", "latest"}},
		{"v0.10.0", []string{"0.10.0", "0.10", "0", "latest"}},
	} {
		t.Run(row.tag, func(t *testing.T) {
			t.Parallel()

			r := newReleaseRepo(t)
			r.write("docs/interface.md", "# The declared surface\n", "the surface page")
			r.tag(row.tag, tagMessage("obsync "+row.tag, "- the surface, stated for the first time"))

			said, _, code := r.cut(row.tag)
			if code != 0 {
				t.Fatalf("%s refused %s, which says what moved:\n\n%s", releaseGate, row.tag, said)
			}

			got := valueOf(t, said, "tags")
			var wanted []string
			for _, tag := range row.want {
				wanted = append(wanted, releaseImage+":"+tag)
			}
			if got != strings.Join(wanted, ",") {
				t.Errorf("%s publishes %s at %q, want %q (§12)", releaseGate, row.tag, got,
					strings.Join(wanted, ","))
			}
			if version := valueOf(t, said, "version"); version != strings.TrimPrefix(row.tag, "v") {
				t.Errorf("%s reports version=%q for %s, and the version is what the binary is "+
					"stamped with and what identifies the bytes an operator pinned (§12)",
					releaseGate, version, row.tag)
			}
		})
	}
}

// valueOf reads one `key=value` line out of what the gate printed. The gate's
// stdout is what the workflow feeds into $GITHUB_OUTPUT, so the format is the
// contract between the two.
func valueOf(t *testing.T, said, key string) string {
	t.Helper()

	for _, line := range strings.Split(said, "\n") {
		if name, value, found := strings.Cut(line, "="); found && name == key {
			return value
		}
	}
	t.Fatalf("%s printed no %s= line, and the workflow reads one:\n\n%s", releaseGate, key, said)
	return ""
}

// The other half of the rule, and the reason the section is *mandatory* rather
// than *required when something moved*: a release that touched nothing an
// operator set or pinned still has to say so, and an empty section is how it
// says it. Nothing here is generated — the gate reads and refuses, it never
// writes the sentence for anyone.
func TestAReleaseThatMovedNothingOnTheSurfaceMayLeaveTheSectionEmpty(t *testing.T) {
	t.Parallel()

	r := newReleaseRepo(t)
	r.write("docs/interface.md", "# The declared surface\n\nNine variables.\n", "the surface page")
	r.tag("v0.3.0", tagMessage("obsync v0.3.0", "- the surface, stated for the first time"))

	r.write("internal/loop/loop.go", "package loop\n", "a change nobody configures against")
	r.tag("v0.3.1", tagMessage("obsync v0.3.1"))

	said, notes, code := r.cut("v0.3.1")
	if code != 0 {
		t.Fatalf("%s refused v0.3.1, which moved nothing on the declared surface and says so with "+
			"an empty section — present and empty when nothing moved is the rule (§12):\n\n%s",
			releaseGate, said)
	}
	if !strings.Contains(notes, surfaceChangesMarker) {
		t.Errorf("the release notes for v0.3.1 carry no surface change section:\n\n%s\n\nThe section "+
			"is present in every release's notes, so that an operator reading them never has to "+
			"work out whether its absence means nothing moved or nobody looked (§12)", notes)
	}
}

// The tag's own words are the human half of the notes, and they reach the
// release verbatim. GitHub pre-pends a body to the notes it generates from
// commit titles, so the generated half stays generated and the one thing
// generation cannot produce is written by the person who knows it.
func TestTheReleaseNotesCarryWhatTheTagSaidAboutTheSurface(t *testing.T) {
	t.Parallel()

	r := newReleaseRepo(t)
	r.write("docs/interface.md", "# The declared surface\n", "the surface page")
	r.tag("v0.4.0", tagMessage("obsync v0.4.0",
		"- `OBSYNC_SIZE_CEILING` now accepts `GB` as well as `MB`.",
		"- The `status` subcommand reports the build version.",
	))

	said, notes, code := r.cut("v0.4.0")
	if code != 0 {
		t.Fatalf("%s refused v0.4.0, which says what moved:\n\n%s", releaseGate, said)
	}
	for _, want := range []string{
		"obsync v0.4.0",
		surfaceChangesMarker,
		"`OBSYNC_SIZE_CEILING` now accepts `GB` as well as `MB`.",
		"The `status` subcommand reports the build version.",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the release notes for v0.4.0 do not carry %q:\n\n%s", want, notes)
		}
	}
}

// The section is mandatory whether or not anything moved, so its absence is
// refused on a release that moved nothing at all. Without this the rule is
// "say something when the page changed", and a maintainer who never writes the
// section gets a green release every time the page happens to sit still — which
// is the habit the one release that does move it depends on nobody having.
func TestATagMessageWithNoSurfaceChangeSectionIsRefusedEvenWhenNothingMoved(t *testing.T) {
	t.Parallel()

	r := newReleaseRepo(t)
	r.write("docs/interface.md", "# The declared surface\n", "the surface page")
	r.tag("v0.3.0", tagMessage("obsync v0.3.0", "- the surface, stated for the first time"))

	r.write("README.md", "# obsync\n", "prose")
	r.tag("v0.3.1", "obsync v0.3.1\n\nA bug fix and nothing else.\n")

	said, _, code := r.cut("v0.3.1")
	if code == 0 {
		t.Fatalf("%s cut v0.3.1 from a tag message with no surface change section at all (§12):"+
			"\n\n%s", releaseGate, said)
	}
	if !strings.Contains(said, surfaceChangesMarker) {
		t.Errorf("%s refused the release without showing what to write instead, which is the one "+
			"thing the person reading it needs:\n\n%s", releaseGate, said)
	}
}

// A lightweight tag has no message, so there is nowhere for the surface change
// section to be. §12 cuts a release with a pushed *annotated* tag, and this is
// what that word is load-bearing for rather than ceremonial.
func TestALightweightTagCannotCutARelease(t *testing.T) {
	t.Parallel()

	r := newReleaseRepo(t)
	r.write("docs/interface.md", "# The declared surface\n", "the surface page")
	r.mustGit("tag", "v0.4.0")

	said, _, code := r.cut("v0.4.0")
	if code == 0 {
		t.Fatalf("%s cut a release from a lightweight tag, which carries no message and so cannot "+
			"carry the one section every release's notes owe (§12):\n\n%s", releaseGate, said)
	}
	if !strings.Contains(said, "annotate") {
		t.Errorf("%s refused a lightweight tag without naming the fix:\n\n%s", releaseGate, said)
	}
}

// A tag the pipeline cannot read a version out of publishes nothing. The trap
// this closes is specific: a `v1.0.0-rc1` run through a tag set derived by
// truncation becomes `latest`, `1.0` and `1`, so a release candidate lands on
// every floating tag an unattended deployment follows.
func TestATagThatIsNotAPlainVersionPublishesNothing(t *testing.T) {
	t.Parallel()

	for _, tag := range []string{"v1.0.0-rc1", "v1.0", "nightly", "v1.0.0.1"} {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()

			r := newReleaseRepo(t)
			r.write("docs/interface.md", "# The declared surface\n", "the surface page")
			r.tag(tag, tagMessage("obsync "+tag, "- everything"))

			said, _, code := r.cut(tag)
			if code == 0 {
				t.Errorf("%s cut a release from %s. obsync is versioned MAJOR.MINOR.PATCH over the "+
					"declared surface, and a pipeline that publishes what it cannot parse will one "+
					"day put a release candidate on `latest` (§12):\n\n%s", releaseGate, tag, said)
			}
		})
	}
}

// The first release has no previous tag to diff against, so there is no
// question about what moved: the whole declared surface is new. It owes the
// same sentence every later release does, and the gate has no special case
// that lets the one release nobody has read yet through with nothing said.
func TestTheFirstReleaseOwesTheSameSentenceEveryLaterOneDoes(t *testing.T) {
	t.Parallel()

	silent := newReleaseRepo(t)
	silent.write("docs/interface.md", "# The declared surface\n", "the surface page")
	silent.tag("v0.4.0", tagMessage("obsync v0.4.0"))

	said, _, code := silent.cut("v0.4.0")
	if code == 0 {
		t.Fatalf("%s cut the first release with an empty surface change section. There is no "+
			"previous tag to diff against, so the whole page is new — which is a surface change by "+
			"any reading (§12):\n\n%s", releaseGate, said)
	}

	spoken := newReleaseRepo(t)
	spoken.write("docs/interface.md", "# The declared surface\n", "the surface page")
	spoken.tag("v0.4.0", tagMessage("obsync v0.4.0", "- the whole of it, stated for the first time"))

	if said, _, code := spoken.cut("v0.4.0"); code != 0 {
		t.Fatalf("%s refused a first release that says what the surface is:\n\n%s", releaseGate, said)
	}
}

// ---------------------------------------------------------------------------
// The workflow the gate runs in
// ---------------------------------------------------------------------------

const releaseWorkflow = ".github/workflows/release.yml"

// A release is cut by a pushed annotated tag and by nothing else. §12 chose
// that deliberately — release automation earns its keep with many contributors
// and this project has one — and the consequence is that every other way of
// starting a publish is a way for one to happen that nobody meant. A manual
// dispatch is the sharpest of them: it publishes whatever is at HEAD under
// whatever the last tag said.
func TestTheReleaseIsCutByAPushedAnnotatedTagAndNothingElse(t *testing.T) {
	t.Parallel()

	trigger := yamlBlock(t, releaseWorkflow, "on")
	if !strings.Contains(trigger, "push:") || !strings.Contains(trigger, "tags:") {
		t.Errorf("%s does not run on a pushed tag, which is the whole of how a release is cut "+
			"(§12):\n\n%s", releaseWorkflow, trigger)
	}
	for _, other := range []string{"workflow_dispatch", "schedule", "release:", "pull_request"} {
		if strings.Contains(trigger, other) {
			t.Errorf("%s also runs on %s. A release is cut by a pushed annotated tag and nothing "+
				"else: every other trigger is a way for a publish to happen that nobody meant, and "+
				"a scheduled one republishing the floating tags is exactly the failure §12 rejects "+
				"— `1` and `1.4.2` become two images with one name:\n\n%s",
				releaseWorkflow, other, trigger)
		}
	}
}

// `permissions: contents: read` at workflow level, with write scopes only in
// the job that publishes (§12). The default token is otherwise write-scoped
// across the whole repository for every step of a workflow that builds a
// container image from a checkout.
func TestWriteScopesLiveOnlyInTheJobThatPublishes(t *testing.T) {
	t.Parallel()

	for _, workflow := range []string{releaseWorkflow, ciWorkflow} {
		permissions := yamlBlock(t, workflow, "permissions")
		if strings.TrimSpace(permissions) != "contents: read" {
			t.Errorf("%s declares workflow-level permissions of %q, want exactly `contents: read` "+
				"(§12)", workflow, strings.TrimSpace(permissions))
		}
	}

	// And the write scopes the publish needs are inside a job rather than above
	// it: `jobs:` is the last top-level key, so anything below it is scoped to
	// one job by construction.
	text := read(t, releaseWorkflow)
	jobs := strings.Index(text, "\njobs:")
	if jobs < 0 {
		t.Fatalf("%s has no jobs", releaseWorkflow)
	}
	for _, scope := range []string{"contents: write", "packages: write"} {
		at := strings.Index(text, scope)
		switch {
		case at < 0:
			t.Errorf("%s asks for no %s, and it publishes an image to GHCR and cuts a GitHub "+
				"Release (§12)", releaseWorkflow, scope)
		case at < jobs:
			t.Errorf("%s grants %s above `jobs:`, so every step of the workflow holds it (§12)",
				releaseWorkflow, scope)
		}
	}
}

// Every action pinned to a full commit SHA, in every workflow. A tag is a
// mutable pointer, and this is the workflow that holds a credential able to
// publish the image an operator's vault sidecar runs.
func TestEveryActionAWorkflowUsesIsPinnedToASHA(t *testing.T) {
	t.Parallel()

	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	used := 0
	for _, workflow := range []string{releaseWorkflow, ciWorkflow} {
		for _, line := range strings.Split(read(t, workflow), "\n") {
			_, after, isUses := strings.Cut(line, "uses:")
			if !isUses {
				continue
			}
			used++
			action := strings.Fields(after)
			if len(action) == 0 {
				t.Errorf("%s has a `uses:` with nothing after it", workflow)
				continue
			}
			name, ref, pinned := strings.Cut(action[0], "@")
			if !pinned || !sha.MatchString(ref) {
				t.Errorf("%s uses %s at %q, which is not a commit SHA. A tag is a mutable pointer, "+
					"and this repository's workflows build the container image that holds a "+
					"write-scoped credential (§12)", workflow, name, ref)
			}
		}
	}
	if used == 0 {
		t.Fatal("no workflow uses an action at all, so this check cannot be measuring anything")
	}
}

// amd64 and arm64, both built, with a build-provenance attestation and an SBOM
// and no cosign — a third verification tool for a guarantee the attestation
// already gives (§12).
func TestTheReleasePublishesBothArchitecturesWithProvenanceAndAnSBOM(t *testing.T) {
	t.Parallel()

	workflow := read(t, releaseWorkflow)
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if !strings.Contains(workflow, platform) {
			t.Errorf("%s does not build %s. Both are built and amd64 is what is run: a NAS or a Pi "+
				"running ignis is plausible, and the architecture risk is near zero when every "+
				"expensive operation is a git subprocess (§12)", releaseWorkflow, platform)
		}
	}
	for _, attestation := range []string{"provenance", "sbom", "attest"} {
		if !strings.Contains(workflow, attestation) {
			t.Errorf("%s attaches no %s to what it publishes (§12)", releaseWorkflow, attestation)
		}
	}
	// Comments are exempt, and deliberately: what §12 declines is *running*
	// cosign, and a file forbidden to name the thing it declined cannot say
	// why it declined it.
	for _, line := range strings.Split(workflow, "\n") {
		if code := strings.TrimSpace(line); !strings.HasPrefix(code, "#") &&
			strings.Contains(strings.ToLower(code), "cosign") {
			t.Errorf("%s reaches for cosign, which §12 declines: it is a third verification tool "+
				"for a guarantee the attestation already gives — %q", releaseWorkflow, code)
		}
	}
}

// The version the image is stamped with, and the four tags it is pushed under,
// both come from the gate rather than from a second reading of the tag. There
// is one place that turns `v1.4.2` into what gets published, and the suite
// drives it.
func TestTheReleaseStampsAndTagsWhatTheGateDecided(t *testing.T) {
	t.Parallel()

	workflow := read(t, releaseWorkflow)
	if !strings.Contains(workflow, releaseGate) {
		t.Fatalf("%s does not run %s, so the surface gate does not stand between a pushed tag and "+
			"a published image (§12)", releaseWorkflow, releaseGate)
	}
	if !strings.Contains(workflow, "VERSION=") {
		t.Errorf("%s passes no VERSION to the build, so the image is stamped `dev` and the version "+
			"cannot identify the bytes an operator pinned (§12)", releaseWorkflow)
	}
	if strings.Contains(workflow, ":latest") {
		t.Errorf("%s spells a tag itself. The tag set is derived from the one version in the tag, "+
			"in %s, so that the four names can never be four builds (§12)", releaseWorkflow, releaseGate)
	}
}

// Nothing reaches the registry before the gate has run. The gate's whole job is
// to stand between a pushed tag and a published image, and an image already in
// GHCR when the check fails is a release that happened.
func TestNothingIsPublishedBeforeTheGateHasRun(t *testing.T) {
	t.Parallel()

	workflow := read(t, releaseWorkflow)
	gate := strings.Index(workflow, releaseGate)
	push := strings.Index(workflow, "--push")
	login := strings.Index(workflow, "docker login")
	if gate < 0 || push < 0 || login < 0 {
		t.Fatalf("%s no longer both runs the gate and pushes an image, so this check cannot tell "+
			"which happens first", releaseWorkflow)
	}
	if gate > push || gate > login {
		t.Errorf("%s pushes before it runs %s. A gate that runs after the image is in the registry "+
			"is a report rather than a gate (§12)", releaseWorkflow, releaseGate)
	}
}

// yamlBlock is one top-level key's block, as text. It is a reader rather than a
// parser: obsync has two direct dependencies and neither is a YAML library, and
// what these checks ask of a workflow — which keys are at the top, and what is
// under them — is answerable from indentation alone.
func yamlBlock(t *testing.T, workflow, key string) string {
	t.Helper()

	var block []string
	inside := false
	for _, line := range strings.Split(read(t, workflow), "\n") {
		switch {
		case line == key+":":
			inside = true
		case inside && (line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			block = append(block, line)
		case inside:
			inside = false
		}
	}
	if len(block) == 0 {
		t.Fatalf("%s has no top-level `%s:` block", workflow, key)
	}
	return strings.Join(block, "\n")
}

// ---------------------------------------------------------------------------
// What the documentation may say about what the release publishes
// ---------------------------------------------------------------------------

// `latest` exists because people expect it to, and is never quoted in the docs
// (§12). What that forbids is precise and mechanical: no document tells anyone
// to *pin* it. Naming it in the versioning section — it exists, nothing here
// points at it — is the opposite of quoting it, and is why this checks for an
// image reference rather than for the word.
func TestNoDocumentPinsTheLatestTag(t *testing.T) {
	t.Parallel()

	// Scoped to GHCR, which is where obsync publishes and the only registry it
	// has a position about. The reference compose's `nobbe/ignis:latest` is
	// ignis's business — obsync promises nothing about ignis's releases, and a
	// file taking a position on a neighbour's tagging practice would be this
	// project doing to somebody else what it declined to do to itself.
	pinned := regexp.MustCompile(`ghcr\.io/[a-z0-9._/-]+:latest`)
	for _, piece := range documentationSet {
		for _, at := range pinned.FindAllString(read(t, piece.path), -1) {
			t.Errorf("%s quotes %s. `latest` is published because people expect it to be, and no "+
				"document points at it: a name two builds can share is the one thing an unattended "+
				"sidecar must not follow (§12)", piece.path, at)
		}
	}
}

// One image, spelled the same in the file an operator copies, the page they
// configure against, and the workflow that publishes it — and the workflow's
// own spelling is read off the repository rather than typed, so it cannot be
// the one that drifts.
func TestTheReferenceComposeAndTheSurfacePageNameTheImageTheReleasePublishes(t *testing.T) {
	t.Parallel()

	for _, piece := range []string{"compose.yaml", "docs/interface.md"} {
		if !strings.Contains(read(t, piece), releaseImage) {
			t.Errorf("%s does not name %s, which is where obsync's releases are published — GHCR "+
				"only, one registry and one credential (§12)", piece, releaseImage)
		}
	}
	if !strings.Contains(read(t, releaseWorkflow), "ghcr.io/${{ github.repository }}") {
		t.Errorf("%s spells the image it publishes rather than reading it off the repository, so "+
			"the name a fork publishes under is this repository's (§12)", releaseWorkflow)
	}
}

// The versioning section is where the page says what the version is a promise
// *about*, and three of those facts are the release pipeline's: where the image
// is published, what the four tags mean, and that a base image bump is a patch
// release. The last one is not obvious and is the reason the base is
// digest-pinned at all — Alpine repoints a release tag on every patch, so
// without a release of obsync's own there is nothing for an operator to move to.
func TestTheSurfacePageStatesWhatAReleasePublishes(t *testing.T) {
	t.Parallel()

	versioning := section(t, "docs/interface.md", "## Versioning")
	for _, fact := range []struct{ phrase, why string }{
		{releaseImage, "where the image is published"},
		{"latest", "that the floating tags exist, including the one no document pins"},
		{"patch", "that a base image bump is a patch release"},
		{"annotated tag", "what cuts a release"},
		{"Surface changes", "the section every release's notes carry"},
	} {
		if !strings.Contains(versioning, fact.phrase) {
			t.Errorf("the versioning section of docs/interface.md does not state %s (%q). It is "+
				"the page SemVer is measured over, and these are the facts an operator deciding "+
				"what to pin needs from it (§12)", fact.why, fact.phrase)
		}
	}
}

// section is one Markdown section of a document, heading included, up to the
// next heading of the same level or above.
func section(t *testing.T, path, heading string) string {
	t.Helper()

	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	var found []string
	inside := false
	for _, line := range strings.Split(read(t, path), "\n") {
		switch {
		case line == heading:
			inside = true
		case inside && strings.HasPrefix(line, "#") &&
			len(line)-len(strings.TrimLeft(line, "#")) <= level:
			inside = false
		}
		if inside {
			found = append(found, line)
		}
	}
	if len(found) == 0 {
		t.Fatalf("%s has no %q section", path, heading)
	}
	return strings.Join(found, "\n")
}

// ---------------------------------------------------------------------------
// Seam 2: the version, on the image
// ---------------------------------------------------------------------------

// The version has to identify the image, and that decided the rest of §12 — so
// the image an operator pulls says which build it is without being run. The
// label is what `docker inspect` answers from, and a published image whose
// label reads `dev` is a build nothing can identify after the fact: not the
// operator, not a report, not the person trying to work out whether a vault ran
// the version that had the bug.
//
// The two spellings have to agree, which is why both are asserted here rather
// than in two places: the label the image carries and the version the binary
// inside it reports come from one `--build-arg`, and one of them being stale is
// exactly the drift a second reader catches.
func TestTheImageLabelsItselfWithTheVersionItWasStampedWith(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := seam2Image(t)

	label := strings.TrimSpace(docker(t, "inspect", "--format",
		"{{index .Config.Labels \"org.opencontainers.image.version\"}}", image))
	if label != stampedVersion {
		t.Errorf("the image is labelled org.opencontainers.image.version=%q and was built at %q. "+
			"The label is what `docker inspect` answers from, and it is the only thing that says "+
			"which build an image is without running it (§12)", label, stampedVersion)
	}

	report := docker(t, "run", "--rm", "--user", obsyncUID, image, "status")
	if !strings.Contains(report, stampedVersion) {
		t.Errorf("obsync status in the image printed %q, and the image is labelled %q: the label "+
			"and the binary come from one --build-arg, and one of them being stale is a published "+
			"image nobody can identify (§12)", report, label)
	}
}
