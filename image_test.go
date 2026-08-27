package main

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is seam 2: the container image, run as an arbitrary UID against a real
// bare remote, and the reference compose exercised the same way.
//
// It is the only seam that can see properties of the *assembly* — the image
// working as a UID with no `/etc/passwd` entry, the git floor gate firing, the
// `HEALTHCHECK` being wired, and the credential file being re-read. Everything
// behavioural belongs at seam 1, in the rest of this suite, and there is no
// third seam.
//
// The clock is not injectable here, because the thing under test is a process
// in a container rather than a loop in this one. That is the whole reason no
// timing rule is asserted below: the tests here wait for obsync to have done
// something and never for it to have done it at a particular moment, so the
// only real seconds this suite spends are spent waiting on the assembly rather
// than on a constant. Every timing rule in §2 is pinned at seam 1, deterministically.
//
// The remote is the same bare repository over `file://` that seam 1 uses, for
// the same reasons: it takes real hooks, it needs no credential, and it runs on
// a fork's PR. It lives in a named volume rather than a bind mount because a
// bind mount is a path in the *daemon's* filesystem, which is not this
// process's filesystem wherever the daemon is not local.

// seam2Switch asks for this suite. It is deliberately not `OBSYNC_`-prefixed:
// that prefix is the config surface (§8, nine variables), and a tenth name in
// it — even one only a test reads — is the sort of thing that gets counted.
//
// Building and running the image costs minutes and a Docker socket, so the
// ordinary `go test ./...` loop skips it and CI asks for it by name. A suite
// that silently ran the image whenever a socket happened to exist would make
// every other slice's feedback loop pay for this one.
//
// The workflow selects these tests by name — `TestTheImage…` and
// `TestTheReferenceCompose…` — so a seam-2 test named outside those two is one
// CI never runs.
const seam2Switch = "SEAM2"

// ciWorkflow is the PR and main half of the pipeline, which builds this image
// and runs the smoke test below.
const ciWorkflow = ".github/workflows/ci.yml"

// seam2Tag and seam2FloorTag are what this suite builds. Fixed names rather
// than unique ones, so that a re-run reuses the layer cache; every *container*
// and *volume* below is named per test and removed by it.
const (
	seam2Tag         = "obsync:seam2"
	seam2FloorTag    = "obsync:seam2-floor"
	seam2NoPasswdTag = "obsync:seam2-nopasswd"
	seam2SSHTag      = "obsync:seam2-ssh"
)

// obsyncUID is what this suite runs obsync as: an arbitrary UID:GID pair with
// no entry in the image's passwd file, and deliberately not the 1000:1000 the
// reference compose uses — a test at 1000 would pass on an image that happened
// to carry a user of that name.
const obsyncUID = "4242:4242"

// needsSeam2 skips unless this run asked for the image.
func needsSeam2(t *testing.T) {
	t.Helper()

	if os.Getenv(seam2Switch) == "" {
		t.Skipf("seam 2 builds and runs the container image; set %s=1 to run it", seam2Switch)
	}
}

// seam2Image is the image under test, built once for the whole suite.
//
// It is built from this checkout rather than pulled, because what is under test
// is the Dockerfile beside these tests — and the version it is stamped with is
// the one seam 1 stamps the binary with, so that "the version identifies the
// bytes" is one claim checked in two places rather than two claims.
func seam2Image(t *testing.T) string {
	t.Helper()

	if err := buildImage(); err != nil {
		t.Fatalf("building the image: %v", err)
	}
	return seam2Tag
}

var buildImage = sync.OnceValue(func() error {
	return buildTaggedImage(seam2Tag, "")
})

// buildTaggedImage runs one image build, and is the only place this suite spells the
// build command.
//
// `docker buildx build --load` rather than `docker build`: the build has to end
// with the image in the daemon's own store whatever driver is configured, and a
// container driver otherwise leaves it in a build cache nothing can run.
func buildTaggedImage(tag, dockerfile string) error {
	// The build's own two inputs, read here in process and then thrown away.
	// That is load-bearing rather than pointless, for the reason gomod_test.go
	// states: `go test` keys its result cache on the files a test itself opens,
	// and everything else these tests stand on reaches them through a Docker
	// socket, which it cannot see. Without this, editing the Dockerfile and
	// re-running returns a cached pass — measured, and a guard that goes stale
	// on the one edit it exists to catch is worse than no guard.
	for _, input := range []string{"Dockerfile", ".dockerignore"} {
		if _, err := os.ReadFile(input); err != nil {
			return err
		}
	}

	args := []string{"buildx", "build", "--load", "-t", tag, "--build-arg", "VERSION=" + stampedVersion}
	if dockerfile != "" {
		args = append(args, "--file", dockerfile)
	}
	args = append(args, ".")

	cmd := exec.Command("docker", args...)
	var said bytes.Buffer
	cmd.Stdout, cmd.Stderr = &said, &said
	if err := cmd.Run(); err != nil {
		return errors.New(err.Error() + ": " + said.String())
	}
	return nil
}

// dockerRun runs one docker command to completion and returns its stdout,
// stderr and exit status. A docker that could not be started at all is fatal:
// this suite was asked for, and a missing socket is not a skip once it has been.
func dockerRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	cmd := exec.Command("docker", args...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()

	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running docker %s: %v", strings.Join(args, " "), err)
	}
	return out.String(), errs.String(), code
}

// docker is dockerRun for a command that has to succeed.
func docker(t *testing.T, args ...string) string {
	t.Helper()

	stdout, stderr, code := dockerRun(t, args...)
	if code != 0 {
		t.Fatalf("docker %s exited %d: %s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// imageFile is one file out of the image's own filesystem, copied off a
// container that is created and never started so that nothing a runtime does at
// start-up is mistaken for something the image carries.
func imageFile(t *testing.T, image, path string) string {
	t.Helper()

	content, err := os.ReadFile(copyOutOfImage(t, image, path))
	if err != nil {
		t.Fatalf("reading %s out of the image: %v", path, err)
	}
	return string(content)
}

// copyOutOfImage copies one path out of the image and returns where it landed
// locally.
func copyOutOfImage(t *testing.T, image, path string) string {
	t.Helper()

	container := strings.TrimSpace(docker(t, "create", "--user", obsyncUID, image, "status"))
	t.Cleanup(func() { _, _, _ = dockerRun(t, "rm", "-f", container) })

	local := filepath.Join(t.TempDir(), "copied")
	docker(t, "cp", container+":"+path, local)
	return local
}

// The binary in the image is static, which is what makes the image one binary
// and git rather than a binary, git and a C library's worth of assumptions.
//
// Nothing about running on alpine proves it — a cgo build on this builder links
// against the same musl the base carries and works fine — so the loader is what
// is checked. A static binary carries no PT_INTERP: there is no interpreter to
// name, which is the whole property. It is also the difference between "a bare
// binary is a non-goal" and "a bare binary is impossible" (§1).
func TestTheImagesBinaryIsStaticAndNamesNoLoader(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	binary, err := elf.Open(copyOutOfImage(t, seam2Image(t), "/usr/local/bin/obsync"))
	if err != nil {
		t.Fatalf("reading the binary out of the image: %v", err)
	}
	defer func() { _ = binary.Close() }()

	for _, segment := range binary.Progs {
		if segment.Type == elf.PT_INTERP {
			t.Errorf("the binary in the image names a dynamic loader, so it was not built with "+
				"CGO_ENABLED=0: the image is one static binary and git, and a cgo build links "+
				"against this builder's C library and still runs here, which is why the loader "+
				"rather than the run is what says so (§1). Segment: %v", segment)
		}
	}
}

// seam2Volume is a throwaway vault and its bare remote, in a named volume.
//
// A named volume rather than a bind mount of a `t.TempDir()`, because a bind
// mount is a path in the *daemon's* filesystem: wherever the daemon is not this
// process — a remote socket, a rootless runtime in another namespace — a bind
// of a local temporary directory silently mounts an empty directory of the same
// name and the test measures nothing. The volume is the daemon's own, so it
// means the same thing everywhere.
//
// The remote is seeded through the image's own git, by a container that runs as
// root only to hand the whole thing to the UID obsync will run as afterwards.
// That is scaffolding rather than obsync: what is under test is the container
// obsync runs in, and that one never starts as root.
//
// The owner is a parameter because it is the thing gate 1 is about. obsync runs
// as the UID it was given and never changes it, so a vault owned by another one
// is a full freeze with a named cause rather than silent corruption — which is
// what earns Docker's `user:` line the right to replace a PUID/PGID knob (§8).
func seam2Volume(t *testing.T, purpose, owner string) string {
	t.Helper()

	// The seeding container is the shipped image's own git, so the shipped
	// image has to exist — asked for here rather than remembered at each call
	// site, because a caller running a *derived* image would otherwise be one
	// forgotten line away from seeding against nothing.
	seam2Image(t)

	name := fmt.Sprintf("obsync-seam2-%s-%d", purpose, os.Getpid())
	_, _, _ = dockerRun(t, "volume", "rm", "-f", name)
	docker(t, "volume", "create", name)
	t.Cleanup(func() { _, _, _ = dockerRun(t, "volume", "rm", "-f", name) })

	// A vault is a directory holding `.obsidian/` — that is the vault sentinel,
	// and a directory without one is not a vault obsync will touch (§7). The
	// credential file is seeded beside it because an operator's is a mount that
	// is already there when the container starts: one configured and unreadable
	// *at startup* is a config error and obsync exits on it (§8).
	docker(t, "run", "--rm", "--user", "0:0", "-v", name+":/data", "--entrypoint", "sh", seam2Tag, "-c", `
set -e
git init -q --bare -b main /data/remote.git
mkdir -p /data/seed/.obsidian /data/vault
printf '{}\n' > /data/seed/.obsidian/app.json
printf 'welcome\n' > /data/seed/Welcome.md
printf 'the-first-token\n' > /data/token
cd /data/seed
git init -q -b main .
git add -A
git -c user.name='A Human' -c user.email='human@example.invalid' commit -q -m 'the vault'
git push -q /data/remote.git main
cd /
rm -rf /data/seed
chown -R `+owner+` /data
`)
	return name
}

// startObsync starts the image against that volume and returns the container's
// name, cleaned up — and its log printed — when the test ends.
func startObsync(t *testing.T, image, volume string, environment map[string]string) string {
	t.Helper()

	name := fmt.Sprintf("obsync-seam2-%d-%d", os.Getpid(), containers.Add(1))
	_, _, _ = dockerRun(t, "rm", "-f", name)

	args := []string{
		"run", "--detach", "--name", name,
		// Docker's own `user:` line, which is what the reference compose sets
		// and what replaces a PUID/PGID knob (§8).
		"--user", obsyncUID,
		"--volume", volume + ":/data",
		"--env", "OBSYNC_REPO=file:///data/remote.git",
		"--env", "OBSYNC_VAULT_PATH=/data/vault",
	}
	for key, value := range environment {
		args = append(args, "--env", key+"="+value)
	}
	docker(t, append(args, image)...)

	t.Cleanup(func() {
		if t.Failed() {
			stdout, stderr, _ := dockerRun(t, "logs", name)
			t.Logf("obsync's own log:\n%s%s", stdout, stderr)
		}
		_, _, _ = dockerRun(t, "rm", "--force", name)
	})
	return name
}

// containers numbers the containers this suite starts, so that two parallel
// tests never name one the same thing.
var containers atomic.Int64

// waitFor blocks until something obsync did has happened, and fails the test
// with obsync's own log if it has not happened within the window.
//
// It sleeps, and that is not the thing the testing rules forbid: the fake clock
// exists so that seam 1 never sleeps through a *timing rule*, and no timing
// rule is under test here. What is being waited on is a real process in another
// container doing real work, which this one has no clock to move on its behalf.
// So nothing below asserts *when* — only that it happened at all.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()

	// Long enough for a Docker HEALTHCHECK on a 60s interval to have run twice,
	// because that is the slowest thing anything here waits on.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("waited three minutes for %s and it did not happen", what)
}

// processOne is what is running as PID 1 in the container.
func processOne(t *testing.T, container string) string {
	t.Helper()

	// /proc/1/cmdline is NUL-separated, like everything else worth reading by
	// machine.
	return strings.ReplaceAll(strings.TrimRight(
		docker(t, "exec", container, "cat", "/proc/1/cmdline"), "\x00"), "\x00", " ")
}

// remoteTree is every path the bare remote holds on the tracked branch — the
// state of the second of the two repositories every assertion in this project
// is about.
func remoteTree(t *testing.T, container string) string {
	t.Helper()

	stdout, _, _ := dockerRun(t, "exec", container, "git", "-C", "/data/remote.git",
		"ls-tree", "-r", "--name-only", "main")
	return stdout
}

// assertRuntimeHealth waits for the runtime's own health verdict, where the
// runtime reports one.
//
// `docker inspect` is the operator-facing half of §9 — it is what makes
// `docker ps` show whether obsync needs a human — and the assertion is
// therefore worth making rather than inferring from the subcommand. Not every
// Docker-API-compatible runtime implements it, though: podman's compatibility
// endpoint reports no health at all, and a suite that failed there would be
// failing the runtime rather than the image. So it is asserted where it can be
// seen and said out loud where it cannot, with the two things it stands on —
// the HEALTHCHECK's own command, and the directive that wires it — checked
// unconditionally either side of this.
func assertRuntimeHealth(t *testing.T, container string) {
	t.Helper()

	reported := func() string {
		stdout, _, _ := dockerRun(t, "inspect", "--format",
			"{{if .State.Health}}{{.State.Health.Status}}{{end}}", container)
		return strings.TrimSpace(stdout)
	}
	if reported() == "" {
		t.Logf("this runtime reports no container health through `docker inspect`, so the "+
			"HEALTHCHECK's verdict is checked here only by running its own command; the directive "+
			"that wires it is checked by %s", "TestTheImagesHealthcheckIsTheOneTheInterfacePageDeclares")
		return
	}
	waitFor(t, "`docker inspect` to report the container healthy", func() bool {
		return reported() == "healthy"
	})
}

// The image runs as whatever UID Docker's own `user:` line names, and that UID
// has no entry in `/etc/passwd`. It is the claim §1 makes about the image and
// the reason there is no `PUID`/`PGID` knob and no root entrypoint: identity
// comes from obsync's private git config, so the passwd file has nothing to say
// about it and never has to be written at start-up by a root process holding a
// write-scoped credential.
//
// 4242 is an arbitrary UID on purpose. The reference compose says 1000:1000
// because that is what ignis defaults to, and a test at 1000 would pass on an
// image that happened to carry a user of that name.
func TestTheImageRunsAsAnArbitraryUIDWithNoPasswdEntry(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := seam2Image(t)

	// The image's own passwd file, read out of a container that was created and
	// never started. That indirection is the point: a runtime may *synthesise*
	// an entry for a UID it was handed and none of them agree about it — podman
	// writes one into the container's file and Docker does not — so a `getent`
	// inside a running container would answer about the runtime rather than
	// about the image, and would hide exactly the thing being checked.
	passwd := imageFile(t, image, "/etc/passwd")
	for _, line := range strings.Split(passwd, "\n") {
		if fields := strings.Split(line, ":"); len(fields) > 2 && (fields[2] == "4242" || fields[2] == "1000") {
			t.Errorf("the image bakes a passwd entry for the UID obsync runs as (%q), and the one "+
				"thing this test is for is that obsync needs none: identity comes from obsync's "+
				"private git config, which is what replaces a PUID/PGID knob and a root "+
				"entrypoint (§1, §8)", line)
		}
	}

	// obsync answering at all as that UID is the property; the version is what
	// says the answer came from this build rather than from a stale image.
	report := docker(t, "run", "--rm", "--user", obsyncUID, image, "status")
	if !strings.Contains(report, stampedVersion) {
		t.Errorf("obsync status in the image printed %q, want the version this image was built "+
			"with — the version's whole job is to identify the bytes an operator pinned (§12)",
			report)
	}

	// No root entrypoint: an image whose default user is root is one that runs
	// as root wherever an operator forgets the `user:` line, which is exactly
	// the container that must not be holding a write-scoped credential.
	if user := strings.TrimSpace(docker(t, "run", "--rm", "--entrypoint", "id", image, "-u")); user == "0" {
		t.Errorf("the image runs as root by default; obsync has no root entrypoint (§1, §8)")
	}

	// ~35MB all-in is what §1 budgets, and 29MB is what this Dockerfile
	// measures at. The bound is loose because the number that matters is the
	// order of magnitude: a build that copied the module cache, the source
	// tree or a dynamically-linked toolchain in lands a multiple of this, and
	// that is the mistake worth failing on rather than a base image gaining a
	// few megabytes.
	const roomForGitAndOneStaticBinary = 100 << 20
	if size := imageSize(t, image); size > roomForGitAndOneStaticBinary {
		t.Errorf("the image is %dMB; §1 budgets ~35MB all-in for one static binary and git, so "+
			"something is being copied in that is not one of those two", size>>20)
	}
}

// imageSize is what the runtime reports the image weighs, in bytes.
func imageSize(t *testing.T, image string) int64 {
	t.Helper()

	said := strings.TrimSpace(docker(t, "image", "inspect", "--format", "{{.Size}}", image))
	size, err := strconv.ParseInt(said, 10, 64)
	if err != nil {
		t.Fatalf("reading the image's size out of %q: %v", said, err)
	}
	return size
}

// withNoPasswdEntries is the shipped image with its passwd file emptied.
//
// It exists because the property this suite is here for — obsync working as a
// UID with no `/etc/passwd` entry — is not what a running container on every
// runtime actually presents. Docker leaves the image's passwd file alone, so a
// UID its `user:` names genuinely has no entry; rootless podman *synthesises*
// one for it, name, home and all. So on this sandbox's podman-backed socket
// every container the rest of this suite starts has an entry after all, and the
// claim would pass for a reason unrelated to the image.
//
// Emptying the file reproduces Docker's condition on a runtime that would
// otherwise hide it, and reproduces it strictly harder: no UID has an entry
// rather than merely this one. obsync never runs as root here, so nothing is
// lost by root losing its entry too.
func withNoPasswdEntries(dockerfile string) string {
	return asRoot(dockerfile, "rm -f /etc/passwd && ln -s /dev/null /etc/passwd")
}

// The whole loop, in the image, as a UID with genuinely no passwd entry.
//
// This is §1's claim about the image stated as behaviour rather than as a fact
// about a file: identity comes from obsync's private git config, so nothing on
// the path from an empty vault directory to a commit in the bare remote may
// need a passwd entry to exist. The neighbouring test checks that the image
// bakes no entry; this one checks that none is needed, which is the half a
// runtime that synthesises one would otherwise answer on the image's behalf.
func TestTheImageSyncsAsAUIDWithGenuinelyNoPasswdEntry(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := derivedImage(t, seam2NoPasswdTag, "no passwd entries at all", withNoPasswdEntries)
	volume := seam2Volume(t, "nopasswd", obsyncUID)
	container := startObsync(t, image, volume, nil)

	// The *container's* own view rather than the image's, which is the whole
	// difference between this test and its neighbour: a runtime that
	// synthesises an entry would leave that one measuring an image nobody runs.
	uid, _, _ := strings.Cut(obsyncUID, ":")
	if entry, _, code := dockerRun(t, "exec", container, "getent", "passwd", uid); code == 0 {
		t.Fatalf("obsync is running in a container that does have a passwd entry for its own UID "+
			"(%q), so this test is not measuring the thing it is named after", strings.TrimSpace(entry))
	}

	waitFor(t, "obsync to clone the remote into the vault", func() bool {
		_, _, code := dockerRun(t, "exec", container, "test", "-f", "/data/vault/Welcome.md")
		return code == 0
	})

	docker(t, "exec", container, "sh", "-c",
		`mkdir -p /data/vault/Daily && printf 'a note\n' > "/data/vault/Daily/2026-08-27.md"`)
	waitFor(t, "the note to reach the bare remote", func() bool {
		return strings.Contains(remoteTree(t, container), "Daily/2026-08-27.md")
	})

	if _, _, code := dockerRun(t, "exec", container, "obsync", "healthcheck"); code != 0 {
		t.Errorf("obsync healthcheck exited %d as a UID with no passwd entry over a vault it had "+
			"just published, want 0: no part of obsync reads that file, which is what makes "+
			"Docker's `user:` line enough on its own (§1, §8)\n%s",
			code, docker(t, "exec", container, "obsync", "status"))
	}
}

// SSH is the one thing in the image that does read the passwd file, and the
// reference compose is what tells an operator so.
//
// §8 accepts `ssh://` and scp-style remotes and gives them no knobs at all: the
// key and a `known_hosts` arrive as ordinary mounts. The image names HOME as
// where they go. But ssh expands `~` out of the UID's passwd entry and never
// out of HOME, and with no entry at all it exits before reading any
// configuration — so on a real Docker, where `user: "1000:1000"` names a UID
// this image deliberately has no entry for, an unmounted or wrongly-homed
// passwd line means git reports a remote it could not read. obsync tiers that
// as an unreachable remote, which is healthy and silent for a day (§9): the
// expensive direction. That is what earns the passwd mount its place in a
// normative file, and this is the measurement it rests on.
//
// The positive half is the promise: with the line the compose documents, the
// key mounted where the compose says lands where ssh looks. `ssh -G` is what is
// asked because there is no reachable sshd here and none is wanted — it is
// ssh's own canonical view of the configuration it would use, and the
// known-hosts path is the one `~` it prints expanded.
func TestTheImagesSSHTransportFindsTheKeyWhereTheReferenceComposeSaysToMountIt(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	home := shippedHome(t)
	uid, _, _ := strings.Cut(obsyncUID, ":")

	withEntry := derivedImage(t, seam2SSHTag, "a passwd entry the compose documents",
		func(dockerfile string) string {
			return asRoot(dockerfile, "printf 'obsync:x:"+uid+":"+uid+":obsync:"+home+
				":/sbin/nologin\\n' >> /etc/passwd")
		})

	knownHosts := sshResolves(t, withEntry, "userknownhostsfile")
	if want := path.Join(home, ".ssh"); !strings.HasPrefix(knownHosts, want+"/") {
		t.Errorf("with the passwd entry the reference compose documents, ssh looks for known_hosts "+
			"at %q rather than under %q — so the mount the compose tells an operator to make is "+
			"not the directory ssh reads, and an ssh remote silently never authenticates (§8)",
			knownHosts, want)
	}

	// And the reason the line is there at all. A red assertion here is good
	// news rather than a defect: it means ssh no longer needs the entry, and
	// the compose's second mount can go.
	noEntry := derivedImage(t, seam2NoPasswdTag, "no passwd entries at all", withNoPasswdEntries)
	// The same pins obsync puts on every git it runs, so that a build where ssh
	// got further than it does today asks nobody anything and waits for nothing.
	stdout, stderr, _ := dockerRun(t, "run", "--rm", "--user", obsyncUID, "--entrypoint", "sh",
		"--env", "GIT_TERMINAL_PROMPT=0",
		"--env", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=5",
		noEntry, "-c", "git ls-remote ssh://git@127.0.0.1/owner/vault.git")
	if said := stdout + stderr; !strings.Contains(said, "No user exists for uid "+uid) {
		t.Errorf("with no passwd entry, git's ssh transport said %q — the reference compose "+
			"documents a passwd mount because ssh refuses to start without one, and if that has "+
			"stopped being true the mount has stopped being load-bearing (§8, §11)", said)
	}

	// The compose is where the whole instruction lives, and a load-bearing line
	// is never cut for brevity: its absence is a defect rather than a gap.
	const passwdMount = ":/etc/passwd:ro"
	if compose := read(t, referenceCompose); !strings.Contains(compose, passwdMount) {
		t.Errorf("%s names no %q mount for an ssh remote. It is the one thing in the SSH path an "+
			"operator cannot infer, it is silent when missing, and this file is the one whose "+
			"correctness they inherit by copying it (§8, §11)", referenceCompose, passwdMount)
	}
}

// sshResolves is what the image's own ssh makes of one of its configuration
// keys, for a host it never contacts.
func sshResolves(t *testing.T, image, key string) string {
	t.Helper()

	said := docker(t, "run", "--rm", "--user", obsyncUID, "--entrypoint", "ssh", image,
		"-G", "vault.example.invalid")
	for _, line := range strings.Split(said, "\n") {
		if name, value, found := strings.Cut(strings.TrimSpace(line), " "); found && name == key {
			// ssh lists more than one known_hosts file; the first is the one it
			// writes and the one an operator mounts.
			first, _, _ := strings.Cut(value, " ")
			return first
		}
	}
	t.Fatalf("the image's ssh reported no %s:\n%s", key, said)
	return ""
}

// obsync is PID 1, and PID 1 is where a missing signal handler stops being a
// slow shutdown and becomes no shutdown at all: the kernel applies no default
// action for a signal PID 1 has not asked for, so a SIGTERM obsync did not
// handle would be discarded rather than kill it, and Docker would SIGKILL the
// container at the end of the grace period instead.
//
// That is exactly what the reference compose's `stop_grace_period` is set
// against, so the period is read from the compose rather than typed here: what
// is under test is that an operator who copies that file gets a container which
// finishes the run it is in and exits, rather than one killed part-way through
// applying a tree (§1, §11).
func TestTheImageExitsOnSIGTERMWithinTheReferenceComposesGracePeriod(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	grace, err := time.ParseDuration(composeConfig(t, referenceCompose).Services["obsync"].StopGracePeriod)
	if err != nil {
		t.Fatalf("reading the reference compose's stop_grace_period: %v", err)
	}

	image := seam2Image(t)
	volume := seam2Volume(t, "sigterm", obsyncUID)
	container := startObsync(t, image, volume, nil)

	// Stopped once it is demonstrably in the loop rather than still starting,
	// so that what SIGTERM interrupts is obsync's own wait.
	waitFor(t, "obsync to clone the remote into the vault", func() bool {
		_, _, code := dockerRun(t, "exec", container, "test", "-f", "/data/vault/Welcome.md")
		return code == 0
	})

	docker(t, "stop", "--timeout", strconv.Itoa(int(grace.Seconds())), container)

	code := strings.TrimSpace(docker(t, "inspect", "--format", "{{.State.ExitCode}}", container))
	if code != "0" {
		t.Errorf("the container exited %s after `docker stop` inside the reference compose's %s "+
			"grace period, want 0. 137 is the SIGKILL that follows a SIGTERM nothing acted on, and "+
			"a container killed mid-run is how a shutdown manufactures a half-applied tree (§1)",
			code, grace)
	}
}

// The whole of what obsync does, in the image, as an arbitrary UID: it clones
// the remote into an empty vault directory, commits what somebody writes there,
// and pushes it — and Docker's own health signal says so.
//
// This is the smoke test §12 asks for, and the assertions are the same kind
// seam 1's are: what is in the bare remote, and what the process boundary
// answers. Nothing here asserts which git ran.
func TestTheImageClonesCommitsAndPushesAsAnArbitraryUID(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := seam2Image(t)
	volume := seam2Volume(t, "smoke", obsyncUID)
	container := startObsync(t, image, volume, nil)

	// obsync is PID 1 and there is no init process in front of it: it waits on
	// every git it spawns, so nothing in this container can be orphaned, and a
	// shell at PID 1 would swallow the SIGTERM the whole shutdown rule depends
	// on (§1, §8).
	if pid1 := processOne(t, container); !strings.Contains(pid1, "obsync") || strings.Contains(pid1, "sh") {
		t.Errorf("PID 1 in the container is %q, want obsync itself — the image has no init process "+
			"and no entrypoint shell by construction (§1, §8)", pid1)
	}

	// The clone is obsync's own bootstrap: an empty directory pointed at a
	// remote that has a branch (§3). What it left behind is a vault holding the
	// remote's commit.
	waitFor(t, "obsync to clone the remote into the vault", func() bool {
		_, _, code := dockerRun(t, "exec", container, "test", "-f", "/data/vault/Welcome.md")
		return code == 0
	})

	// A note written into the vault by somebody else — which is every note, as
	// far as obsync is concerned — reaches the remote with no further help.
	docker(t, "exec", container, "sh", "-c",
		`mkdir -p /data/vault/Daily && printf 'a note\n' > "/data/vault/Daily/2026-08-27.md"`)
	waitFor(t, "the note to reach the bare remote", func() bool {
		return strings.Contains(remoteTree(t, container), "Daily/2026-08-27.md")
	})

	// What Docker's HEALTHCHECK runs, run the way Docker runs it. A deployment
	// that has just published its first commit is one that needs nobody (§9).
	if _, _, code := dockerRun(t, "exec", container, "obsync", "healthcheck"); code != 0 {
		t.Errorf("obsync healthcheck exited %d over a vault it had just published, want 0: health "+
			"answers whether this needs a human, and a working deployment does not (§9)\n%s",
			code, docker(t, "exec", container, "obsync", "status"))
	}

	assertRuntimeHealth(t, container)
}

// The `HEALTHCHECK` directive is on the declared surface, parameters included
// (§9, §10), and `docs/interface.md` is the canonical statement of it. So the
// image and the page are checked against each other rather than each against a
// number somebody typed twice: an image whose interval moved is a surface
// change whatever the page still says, and a page the image no longer matches
// is a promise nothing keeps.
//
// This is the one test here that needs no Docker: the directive is a fact about
// the Dockerfile, and reading it is not the same question as running it.
func TestTheImagesHealthcheckIsTheOneTheInterfacePageDeclares(t *testing.T) {
	t.Parallel()

	declared := healthcheckIn(t, read(t, interfacePage))
	built := healthcheckIn(t, read(t, "Dockerfile"))
	if declared == "" {
		t.Fatalf("%s states no HEALTHCHECK directive, and the health contract is what it is "+
			"canonical about (§10)", interfacePage)
	}
	if built != declared {
		t.Errorf("the image's HEALTHCHECK is\n\t%s\nand %s declares\n\t%s\nThe directive and its "+
			"four parameters are the declared surface: moving one is a surface change, and the "+
			"page is what SemVer is measured over (§9, §10, §12).", built, interfacePage, declared)
	}
}

// healthcheckIn is the HEALTHCHECK directive out of a Dockerfile or out of a
// page quoting one, with its continuations joined and its spacing collapsed, so
// that two spellings of one directive compare equal and two directives do not.
func healthcheckIn(t *testing.T, source string) string {
	t.Helper()

	joined := strings.ReplaceAll(source, "\\\n", " ")
	for _, line := range strings.Split(joined, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "HEALTHCHECK ") {
			continue
		}
		return strings.Join(strings.Fields(line), " ")
	}
	return ""
}

// Both images the build stands on are pinned by digest rather than by tag.
//
// A tag is a mutable pointer — alpine repoints a release tag on every patch,
// git included — so a tag pin does not deliver "the git version moves only when
// we move it", which is the premise an immutable image tag and a build
// attestation both stand on (§12). Dependabot is what moves these, and a base
// CVE bump is a patch release.
func TestTheImageIsBuiltOnDigestsRatherThanTags(t *testing.T) {
	t.Parallel()

	for _, line := range strings.Split(read(t, "Dockerfile"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		image := baseImageIn(line)
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("the Dockerfile builds on %q, which is a tag rather than a digest: a tag is a "+
				"mutable pointer, and a base whose bytes can change under a pinned obsync version "+
				"is the thing digest-pinning exists to stop (§12)", image)
		}
	}
}

// CI derives the git matrix's upper point by asking the base image what git it
// carries, and the base image is named in the Dockerfile. It must not be named
// in the workflow as well: two copies of it is the drift the git floor's own
// one-file rule exists to make impossible, one layer out.
func TestCIReadsTheBaseImageFromTheDockerfileRatherThanRepeatingIt(t *testing.T) {
	t.Parallel()

	workflow := read(t, ciWorkflow)
	if !strings.Contains(workflow, "Dockerfile") {
		t.Errorf("%s does not read the base image from the Dockerfile, so the git the matrix tests "+
			"the product on and the git the image ships are two answers rather than one (§12)",
			ciWorkflow)
	}
	base := baseImageIn(lastFrom(t))
	name, _, _ := strings.Cut(base, "@")
	if strings.Contains(workflow, name) {
		t.Errorf("%s spells the base image %q itself; it is meant to read the Dockerfile's own "+
			"FROM, so that a Dependabot bump moves one line and the matrix follows it (§12)",
			ciWorkflow, name)
	}
}

// CI runs the seam-2 tests by name, so a seam-2 test whose name the workflow's
// own `-run` does not match is one that runs nowhere: skipped in every ordinary
// loop because it asks for SEAM2, and never selected in the one job that sets
// it. That is a silent gap rather than a red build, which is the direction this
// project treats as expensive, and the selection is a regular expression in a
// YAML file that nothing else reads.
//
// So the pattern is taken from the workflow and matched against every test in
// this file that asks for seam 2.
func TestCIRunsEverySeam2TestItsPatternCanSelect(t *testing.T) {
	t.Parallel()

	pattern := seam2Selection(t)
	selects, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("%s selects the seam-2 tests with %q, which is not a regular expression: %v",
			ciWorkflow, pattern, err)
	}

	for _, name := range seam2Tests(t) {
		if !selects.MatchString(name) {
			t.Errorf("%s is a seam-2 test and %s selects seam-2 tests with %q, which does not "+
				"match it — so it is skipped everywhere SEAM2 is unset and never chosen where it "+
				"is set, and the thing it checks about the image is checked nowhere (§12)",
				name, ciWorkflow, pattern)
		}
	}
}

// seam2Selection is the `-run` pattern the workflow hands `go test`.
func seam2Selection(t *testing.T) string {
	t.Helper()

	_, after, found := strings.Cut(read(t, ciWorkflow), "-run '")
	pattern, _, closed := strings.Cut(after, "'")
	if !found || !closed {
		t.Fatalf("%s no longer selects the seam-2 tests with a quoted -run pattern, so this check "+
			"cannot tell whether the job runs them all", ciWorkflow)
	}
	return pattern
}

// seam2Tests is every test in this file that asks for seam 2, by name.
func seam2Tests(t *testing.T) []string {
	t.Helper()

	var asked []string
	// Split on the declaration rather than parsed: what is wanted is which
	// function bodies mention the switch, and a body is everything up to the
	// next top-level `func`.
	for _, function := range strings.Split(read(t, "image_test.go"), "\nfunc ") {
		name, body, isFunction := strings.Cut(function, "(t *testing.T) {")
		if !isFunction || !strings.HasPrefix(name, "Test") || !strings.Contains(body, "needsSeam2(t)") {
			continue
		}
		asked = append(asked, name)
	}
	if len(asked) == 0 {
		t.Fatalf("no test in image_test.go asks for seam 2, which cannot be right while this file " +
			"is the seam-2 suite")
	}
	return asked
}

// lastFrom is the Dockerfile's final FROM line — the base the image ships on,
// as against the builder it is compiled in.
func lastFrom(t *testing.T) string {
	t.Helper()

	last := ""
	for _, line := range strings.Split(read(t, "Dockerfile"), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.EqualFold(fields[0], "FROM") {
			last = line
		}
	}
	if last == "" {
		t.Fatalf("the Dockerfile has no FROM line")
	}
	return last
}

// baseImageIn is the image a FROM line names: its first argument that is not a
// flag, which is what makes `FROM --platform=$BUILDPLATFORM image AS builder`
// read the same way as `FROM image`.
func baseImageIn(line string) string {
	for _, field := range strings.Fields(line)[1:] {
		if !strings.HasPrefix(field, "--") {
			return field
		}
	}
	return ""
}

// read is one of this repository's own files, read in process. That is
// load-bearing rather than incidental, for the reason gomod_test.go states:
// `go test` keys its result cache on the files a test itself opens, so reading
// them here makes a changed file re-run these.
func read(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

// A git below the git floor stops obsync and does not stop the container.
//
// Gate 7 is one of the nine, so it is a full freeze: obsync touches nothing,
// says the fact and the remedy, parks alive, and keeps re-checking — because a
// container that exited on this would crash-loop, and a crash loop buries the
// one message that matters (§7). It is unhealthy while it stands, which is what
// `docker ps` shows the operator (§9).
//
// This is the seam that can see it. The floor obsync refuses below is embedded
// from one file, so the way to put a git below it in front of obsync is to
// raise the floor above the git the image carries — the shipped Dockerfile,
// built from this repository, with that one file's contents changed and nothing
// else. Everything the gate reads is real: git's own version, obsync's own
// comparison, and the image's own build.
func TestTheImageFreezesRatherThanCrashingOnAGitBelowTheFloor(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	// Above every git that exists, so the image's own is below it whatever the
	// base image bumps to.
	const unreachableFloor = "99.0.0"

	image := buildAtFloor(t, unreachableFloor)
	volume := seam2Volume(t, "floor", obsyncUID)
	container := startObsync(t, image, volume, nil)

	waitFor(t, "obsync to refuse the git it is driving", func() bool {
		stdout, stderr, _ := dockerRun(t, "logs", container)
		return strings.Contains(stdout+stderr, "git is older than obsync's floor")
	})

	said, saidToo, _ := dockerRun(t, "logs", container)
	if log := said + saidToo; !strings.Contains(log, unreachableFloor) {
		t.Errorf("obsync refused its git without naming the floor it refused below:\n%s", log)
	}

	// Parked alive, which is what every refusal in this design does.
	if running := strings.TrimSpace(docker(t, "inspect", "--format", "{{.State.Running}}", container)); running != "true" {
		t.Errorf("the container is not running after a gate refused (State.Running=%q); a full "+
			"freeze parks alive and keeps re-checking, and obsync never exits on one (§7)", running)
	}

	// And unhealthy while it stands: a full freeze is the first row of §9's
	// closed unhealthy list, and the healthcheck is how `docker ps` says so.
	if _, _, code := dockerRun(t, "exec", container, "obsync", "healthcheck"); code != 1 {
		t.Errorf("obsync healthcheck exited %d under a full freeze, want 1 — any full freeze needs "+
			"a human, and that is what Docker acts on (§9)", code)
	}
}

// buildAtFloor builds the shipped Dockerfile against this repository with the
// git floor raised, and returns the tag.
//
// The Dockerfile is the real one with a single line inserted, rather than a
// second Dockerfile that would drift from it: everything the image is — the
// pinned base, the static build, the entrypoint, the healthcheck — has to be
// the shipped thing for the gate's refusal to mean anything about the shipped
// image. Changing the file *inside the build* is also what proves the binary's
// floor comes from this repository's own `internal/git/GIT_FLOOR` rather than
// from something the build had lying around.
func buildAtFloor(t *testing.T, floor string) string {
	t.Helper()

	return derivedImage(t, seam2FloorTag, "a raised git floor", func(dockerfile string) string {
		const copiesTheSource = "COPY . .\n"
		if !strings.Contains(dockerfile, copiesTheSource) {
			t.Fatalf("the Dockerfile no longer copies the source with %q, so this test cannot "+
				"raise the floor inside the build it is testing", strings.TrimSpace(copiesTheSource))
		}
		return strings.Replace(dockerfile, copiesTheSource,
			copiesTheSource+"RUN printf '"+floor+"\\n' > internal/git/GIT_FLOOR\n", 1)
	})
}

// derivedImage builds the shipped Dockerfile with one change applied to it, and
// returns the tag.
//
// The shipped file with a line in it rather than a second Dockerfile that would
// drift from it: everything the image is — the pinned base, the static build,
// the entrypoint, the healthcheck, the default USER — has to be the shipped
// thing for what the derived image does to mean anything about the shipped one.
//
// Serialized, because the tags are fixed so that a re-run reuses the layer
// cache, and two parallel tests asking for the same one would otherwise race on
// it. A second build of an unchanged Dockerfile is a cache hit rather than a
// cost.
var derivedBuilds sync.Mutex

func derivedImage(t *testing.T, tag, purpose string, change func(string) string) string {
	t.Helper()

	derivedBuilds.Lock()
	defer derivedBuilds.Unlock()

	path := filepath.Join(t.TempDir(), "Dockerfile.derived")
	if err := os.WriteFile(path, []byte(change(read(t, "Dockerfile"))), 0o600); err != nil {
		t.Fatalf("writing the Dockerfile for %s: %v", purpose, err)
	}
	if err := buildTaggedImage(tag, path); err != nil {
		t.Fatalf("building the image with %s: %v", purpose, err)
	}
	return tag
}

// asRoot is a command run as root at the end of the shipped Dockerfile, with
// the shipped image's own default USER put back afterwards — so the only thing
// the derived image differs by is the command itself.
func asRoot(dockerfile, command string) string {
	return dockerfile + "\nUSER 0:0\nRUN " + command + "\n" + shippedUser(dockerfile) + "\n"
}

// shippedUser is the Dockerfile's final USER line, and shippedHome the value of
// its final ENV HOME. Read rather than repeated, so that a derived image cannot
// quietly restore a different identity from the one the image ships with, and
// so that "the key goes in HOME" is one fact rather than two.
func shippedUser(dockerfile string) string { return lastDirective(dockerfile, "USER ") }

func shippedHome(t *testing.T) string {
	t.Helper()

	home := strings.TrimPrefix(lastDirective(read(t, "Dockerfile"), "ENV HOME="), "ENV HOME=")
	if home == "" {
		t.Fatalf("the Dockerfile sets no HOME, and an arbitrary UID has no passwd entry to take " +
			"one from — so there is nowhere documented for an ssh key to be mounted (§8)")
	}
	return home
}

// lastDirective is the last line of a Dockerfile starting with a prefix, with
// its spacing collapsed, or "".
func lastDirective(dockerfile, prefix string) string {
	last := ""
	for _, line := range strings.Split(dockerfile, "\n") {
		if collapsed := strings.Join(strings.Fields(line), " "); strings.HasPrefix(collapsed, prefix) {
			last = collapsed
		}
	}
	return last
}

// The credential file is read every time git asks for a credential, so an
// operator rotating a token does not restart anything.
//
// That is a property of the *assembly* rather than of a function: the secret's
// journey is the file the operator mounted, one invocation's memory, and git's
// stdin — and the invocation is a process the image starts, as the arbitrary
// UID, out of a path git resolves. There is no daemon and no cache, which is
// the point rather than an omission: a cache exists specifically to not re-read
// (§8). Seam 1 drives the whole path against a remote that really authenticates;
// what is checked here is that the thing git runs inside this image answers
// from the file that is on disk at the moment it is asked.
//
// The rotation is an unlink and a write rather than an overwrite, because that
// is the shape a real rotation has — and a helper holding an open descriptor or
// a remembered inode would pass an overwrite and fail this.
func TestTheImageReadsTheCredentialFileEveryTimeGitAsksForIt(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := seam2Image(t)
	volume := seam2Volume(t, "credential", obsyncUID)
	container := startObsync(t, image, volume, map[string]string{
		"OBSYNC_TOKEN_FILE": "/data/token",
		"OBSYNC_USERNAME":   "oauth2",
	})

	if got := credentialFromHelper(t, container); got != "username=oauth2\npassword=the-first-token\n" {
		t.Fatalf("the helper answered %q, want the credential the mounted file holds", got)
	}

	docker(t, "exec", container, "sh", "-c", `rm /data/token && printf 'the-rotated-token\n' > /data/token`)

	if got := credentialFromHelper(t, container); got != "username=oauth2\npassword=the-rotated-token\n" {
		t.Errorf("after the credential file was rotated the helper answered %q, want the rotated "+
			"credential: the file is re-read every time git asks, which is what makes a rotation "+
			"heal with no restart (§8)", got)
	}
}

// credentialFromHelper asks the image's credential helper for a credential the
// way git does — a request on stdin, an answer on stdout — inside the container
// that is already running the loop.
func credentialFromHelper(t *testing.T, container string) string {
	t.Helper()

	// git writes the request as one key per line and ends it with a blank one.
	cmd := exec.Command("docker", "exec", "--interactive", container,
		"obsync", "credential-helper", "get")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=owner/vault.git\n\n")
	var out, said bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &said
	if err := cmd.Run(); err != nil {
		t.Fatalf("asking the image's credential helper: %v: %s", err, said.String())
	}
	// git relays a helper's stderr to its own, and obsync logs what git says,
	// so a helper that wrote there is one whose output could reach a log — and
	// the credential helper's output is never logged, at any level (§9).
	if said.Len() != 0 {
		t.Errorf("the credential helper wrote %q to stderr; it writes to stdout and to nothing "+
			"else, because git relays a helper's stderr into a log (§9)", said.String())
	}
	return out.String()
}

// referenceCompose is the reference stack an operator copies (§11).
const referenceCompose = "compose.yaml"

// The reference compose is normative rather than exemplary, so it is checked
// rather than reviewed.
//
// Each assertion below is a decision an operator inherits by copying and would
// otherwise have to know to make: the UID pair that replaces a PUID/PGID knob,
// the write-coalescing line that keeps obsync's read path honest, a stop grace
// period longer than the deadline obsync's own shutdown runs on, the vault
// mount landing on obsync's default vault path, the token mounted read-only at
// the path obsync was told to read, and an image pin that moves with a release
// and never with a rebuild.
//
// It is asserted against the *parsed* file rather than against its text,
// because what an operator inherits is what Compose makes of it — and because
// a test that greps a YAML file is one that breaks on an indent and passes on a
// value in the wrong service.
func TestTheReferenceComposeIsWhatItPromises(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	stack := composeConfig(t, referenceCompose)

	ignis, ok := stack.Services["ignis"]
	if !ok {
		t.Fatalf("%s has no ignis service; the reference stack is ignis plus obsync (§11)", referenceCompose)
	}
	if coalescing := ignis.Environment["WRITE_COALESCE_MS"]; coalescing != "0" {
		t.Errorf("the ignis service sets WRITE_COALESCE_MS=%q, want \"0\": above zero obsync reads "+
			"stale content and can overwrite a remote edit ignis is holding buffered, and there is "+
			"no check obsync can write for it — this line is the whole of the answer (§11)",
			coalescing)
	}

	obsync, ok := stack.Services["obsync"]
	if !ok {
		t.Fatalf("%s has no obsync service", referenceCompose)
	}
	if obsync.User != "1000:1000" {
		t.Errorf("the obsync service runs as %q, want \"1000:1000\" — ignis's own default pair. "+
			"This line is what replaces a PUID/PGID knob, and obsync writing the vault as another "+
			"UID is what makes the two fight over ownership (§8)", obsync.User)
	}

	// Docker's default is 10s and obsync's own shutdown deadline is about 30s,
	// so anything at or under 30s SIGKILLs obsync mid-run — which is how a
	// shutdown manufactures a half-applied tree (§1).
	grace, err := time.ParseDuration(obsync.StopGracePeriod)
	if err != nil {
		t.Fatalf("the obsync service's stop_grace_period is %q, which is not a duration: %v",
			obsync.StopGracePeriod, err)
	}
	if grace <= 30*time.Second {
		t.Errorf("the obsync service's stop_grace_period is %s, and obsync's own shutdown deadline "+
			"is 30s: at or under it Docker kills obsync in the middle of the run it was finishing "+
			"(§1, §10)", grace)
	}

	// The pin is a floating major: `1`, or pre-1.0 the `0.x` that stands in for
	// one. Never `latest`, which is a name two images can share, and never a
	// patch, which never moves and leaves a vault sidecar on a stale base.
	image := obsync.Image
	repository, tag, tagged := strings.Cut(image, ":")
	switch {
	case !tagged || tag == "" || tag == "latest":
		t.Errorf("the obsync service pins %q; the reference compose pins the floating major, and "+
			"`latest` is never quoted in obsync's documentation (§12)", image)
	case !strings.HasPrefix(repository, "ghcr.io/"):
		t.Errorf("the obsync service pulls from %q; obsync publishes to GHCR only (§12)", repository)
	case strings.Count(tag, ".") > 1:
		t.Errorf("the obsync service pins %q, which is a patch version: it never moves, and obsync's "+
			"value is running unattended for months on a base that does (§12)", tag)
	}

	// And it resolves — once there is something to resolve. obsync publishes
	// nothing until the release pipeline cuts the first tag (#43), so a pin
	// nothing answers for is recorded here rather than failed, and the day a
	// tag exists this becomes the check that the file an operator copies names
	// an image they can actually pull. It is the reason the pin's *form* is
	// asserted above rather than left to this.
	if _, _, code := dockerRun(t, "manifest", "inspect", image); code != 0 {
		t.Logf("%s pins %s, which no registry answers for yet: obsync has published no release, "+
			"and the first pushed tag is #43's. The pin's form is checked above; that it resolves "+
			"is checkable from the first release onwards.", referenceCompose, image)
	}

	// The vault mount lands on obsync's default vault path, which is what makes
	// OBSYNC_VAULT_PATH absent from the file rather than set to its default.
	vault := mountAt(obsync, "/vault")
	if vault == nil {
		t.Fatalf("the obsync service mounts nothing at /vault, which is obsync's default vault " +
			"path — singular, and deliberately not ignis's /vaults (§8)")
	}
	if base := path.Base(path.Dir(vault.Source)); base != "vaults" {
		t.Errorf("the obsync service mounts %q at /vault, want one vault folder out of ignis's "+
			"vault root — /vaults/<name> is where ignis keeps them, one per vault (§11)", vault.Source)
	}

	// The token is mounted where obsync was told to read it. Two lines that
	// have to agree, in one file an operator copies, is precisely the mistake
	// worth failing a build over.
	credential := obsync.Environment["OBSYNC_TOKEN_FILE"]
	if credential == "" {
		t.Fatalf("the obsync service sets no OBSYNC_TOKEN_FILE, and the reference remote is an " +
			"https one, which is exactly when it is required (§8)")
	}
	token := mountAt(obsync, credential)
	switch {
	case token == nil:
		t.Errorf("the obsync service reads its credential from %q and mounts nothing there (§8)",
			credential)
	case !token.ReadOnly:
		t.Errorf("the credential is mounted writable at %q; obsync only ever reads it (§8)", credential)
	}
}

// composeStack is as much of Compose's own canonical output as this suite
// asserts on. It is read from `docker compose config`, which is Compose's
// answer to what the file means — not a second YAML parser, which obsync has no
// dependency for and does not want one.
type composeStack struct {
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	Image           string            `json:"image"`
	User            string            `json:"user"`
	StopGracePeriod string            `json:"stop_grace_period"`
	Environment     map[string]string `json:"environment"`
	Volumes         []composeMount    `json:"volumes"`
}

type composeMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

func composeConfig(t *testing.T, files ...string) composeStack {
	t.Helper()

	args := []string{"compose"}
	for _, file := range files {
		// Read in process for the reason buildTaggedImage reads the Dockerfile:
		// Compose parses these on the other side of a socket, so `go test`
		// would otherwise cache a pass across an edit to the very file under
		// test.
		if _, err := os.ReadFile(file); err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		args = append(args, "--file", file)
	}
	canonical := docker(t, append(args, "config", "--format", "json")...)

	var stack composeStack
	if err := json.Unmarshal([]byte(canonical), &stack); err != nil {
		t.Fatalf("reading Compose's own view of %v: %v", files, err)
	}
	return stack
}

// mountAt is the mount a service puts at a container path, or nil.
func mountAt(service composeService, target string) *composeMount {
	for _, mount := range service.Volumes {
		if mount.Target == target {
			return &mount
		}
	}
	return nil
}

// The reference compose is run, not just parsed: obsync comes up under it and
// reports healthy.
//
// Two lines are replaced and nothing else — the image, which is the one built
// from this checkout rather than the tag the release pipeline publishes, and
// the remote, which is the same bare repository over file:// every other test
// here uses. Everything the file decides stays exactly as an operator would
// copy it: the UID pair, the stop grace period, the restart policy, the vault
// mount landing on obsync's default vault path.
func TestTheReferenceComposeBringsObsyncUpHealthy(t *testing.T) {
	needsSeam2(t)
	t.Parallel()

	image := seam2Image(t)
	// Owned by the pair the reference compose runs obsync as, which is
	// ignis's own default and not the arbitrary UID the rest of this suite
	// uses.
	volume := seam2Volume(t, "compose", "1000:1000")
	project := fmt.Sprintf("obsync-seam2-%d", os.Getpid())

	override := filepath.Join(t.TempDir(), "compose.override.yaml")
	if err := os.WriteFile(override, []byte(fmt.Sprintf(`
services:
  obsync:
    image: %s
    environment: !override
      OBSYNC_REPO: file:///data/remote.git
      OBSYNC_VAULT_PATH: /data/vault
    volumes: !override
      - %s:/data
volumes:
  %s:
    external: true
`, image, volume, volume)), 0o600); err != nil {
		t.Fatalf("writing the override that points the reference compose at a throwaway vault: %v", err)
	}

	compose := []string{"compose", "--project-name", project,
		"--file", referenceCompose, "--file", override}
	t.Cleanup(func() {
		_, _, _ = dockerRun(t, append(append([]string{}, compose...), "down", "--timeout", "5")...)
	})

	docker(t, append(append([]string{}, compose...), "up", "--detach", "obsync")...)
	container := strings.TrimSpace(docker(t, append(append([]string{}, compose...), "ps", "--quiet", "obsync")...))
	if container == "" {
		t.Fatalf("the reference compose started no obsync container")
	}
	t.Cleanup(func() {
		if t.Failed() {
			stdout, stderr, _ := dockerRun(t, "logs", container)
			t.Logf("obsync's own log:\n%s%s", stdout, stderr)
		}
	})

	waitFor(t, "obsync to come up healthy under the reference compose", func() bool {
		_, _, code := dockerRun(t, "exec", container, "obsync", "healthcheck")
		return code == 0
	})
	assertRuntimeHealth(t, container)

	// The stack the operator copies is still the stack that ran: the override
	// replaced the image and the remote, and Compose resolved everything else
	// out of the reference file.
	stack := composeConfig(t, referenceCompose, override)
	if user := stack.Services["obsync"].User; user != "1000:1000" {
		t.Errorf("the running stack resolved user %q rather than the reference compose's own", user)
	}
}
