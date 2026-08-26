package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// startupLine is the message on the one INFO line that names every resolved
// knob (§8). Waiting for it is also how a test knows obsync got all the way
// through resolving its configuration.
const startupLine = "resolved configuration"

// The config surface is reached through seam 1, obsync's process boundary: an
// environment block goes in, and what comes back is an exit status and logfmt
// on stderr — exactly what an operator's compose file and `docker logs` see.
//
// The line §8 draws, and the one these tests are mostly about: if a check
// needs a syscall on the vault or a network round trip it is a gate and obsync
// parks alive; if it can be decided from the environment block alone it is a
// config error and obsync exits 1.

func TestAMissingRepoIsAConfigErrorAndExits(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runObsync(t, nil)

	if exitCode != 1 {
		t.Errorf("obsync exited %d with no OBSYNC_REPO, want 1 — a container handed nonsense it can "+
			"judge from the environment alone fails visibly rather than parking quietly (§8)", exitCode)
	}
	if !strings.Contains(stderr, "OBSYNC_REPO") {
		t.Errorf("obsync wrote %q to stderr, want it to name the one required variable", stderr)
	}
	if stdout != "" {
		t.Errorf("obsync wrote %q to stdout, want logfmt on stderr and stdout left to the subcommands (§9)", stdout)
	}
}

func TestTheStartupLineEchoesEveryResolvedKnob(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.example.com/owner/vault.git")
	line := loop.awaitLine(startupLine)

	// Every knob, defaulted or set, so an operator can diff what obsync says
	// it is running against what the declared surface says exists (§8).
	for _, want := range []string{
		"repo=ssh://git@git.example.com/owner/vault.git",
		"vault_path=/vault",
		"username=obsync",
		"size_ceiling=95MB",
		"author_name=obsync",
		"author_email=obsync@obsync.invalid",
		"log_level=info",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup line is %q, want it to carry %q", line, want)
		}
	}
}

// The accepted repo forms are a closed list (§8), so this is a row per form,
// and the normalisation each row asserts is gate 5's comparison: what a URL
// reduces to once everything that varies without changing where bytes go is
// discarded. scp-style and ssh:// appear as two rows with one expected value,
// which is what "treats them as equal" means.
func TestTheAcceptedRepoFormsNormaliseToHostAndPath(t *testing.T) {
	t.Parallel()

	for _, form := range []struct {
		name   string
		repo   string
		remote string
	}{
		{"https", "https://github.com/owner/vault.git", "github.com/owner/vault"},
		{"http", "http://git.lan/owner/vault", "git.lan/owner/vault"},
		{"ssh", "ssh://git@github.com/owner/vault.git", "github.com/owner/vault"},
		{"scp-style", "git@github.com:owner/vault.git", "github.com/owner/vault"},
		{"file", "file:///srv/git/vault.git", "/srv/git/vault"},
		{"embedded credentials", "https://someone:secret@github.com/owner/vault", "github.com/owner/vault"},
		{"a default port", "ssh://git@github.com:22/owner/vault", "github.com/owner/vault"},
		{"host case and a trailing slash", "https://GitHub.com/owner/vault/", "github.com/owner/vault"},
		{"a port that is not the default", "https://git.lan:8443/owner/vault", "git.lan:8443/owner/vault"},
	} {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()

			loop := startLoop(t, "OBSYNC_REPO="+form.repo, "OBSYNC_TOKEN_FILE="+credentialFile(t, "a-token"))
			line := loop.awaitLine(startupLine)

			if want := "remote=" + form.remote + " "; !strings.Contains(line, want) {
				t.Errorf("obsync resolved %s to a remote the startup line reports as %q, want %q",
					form.repo, line, want)
			}
		})
	}
}

// Anything outside those forms exits 1: it is nonsense obsync can judge from
// the environment block alone, and a container handed nonsense should fail
// visibly rather than park (§8).
func TestAnUnacceptableRepoIsAConfigErrorAndExits(t *testing.T) {
	t.Parallel()

	for _, refused := range []struct {
		name string
		repo string
	}{
		{"the git:// scheme", "git://github.com/owner/vault.git"},
		{"an absolute path, which is file://'s job", "/srv/git/vault.git"},
		{"a relative path", "vault.git"},
		{"a URL that does not parse", "https://git.lan:not-a-port/owner/vault"},
		{"a URL naming no repository", "https://github.com/"},
		{"scp-style naming no repository", "git@github.com:"},
		{"a URL naming no host", "https:///owner/vault"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, exitCode := runObsync(t, []string{"OBSYNC_REPO=" + refused.repo})

			if exitCode != 1 {
				t.Errorf("obsync exited %d for %q, want 1", exitCode, refused.repo)
			}
			if !strings.Contains(stderr, "OBSYNC_REPO") || !strings.Contains(stderr, "accepts") {
				t.Errorf("obsync wrote %q for %q, want it to name the variable and the accepted forms", stderr, refused.repo)
			}
		})
	}
}

// §8 puts the token in a file so that a rotation is a write rather than a
// redeploy, and keeps it out of every URL, log line and argv. An operator can
// still embed one in the repo URL, and the startup line is the one place
// obsync would otherwise read it straight back out.
func TestNoCredentialInTheRepoURLReachesTheLog(t *testing.T) {
	t.Parallel()

	for _, embedded := range []struct {
		name string
		env  []string
		// survives is what the echo may still carry: for ssh, the login name,
		// which is not a secret and is half of what an operator is diffing.
		survives string
	}{
		{
			name: "https, where the userinfo is the credential",
			env: []string{"OBSYNC_REPO=https://obsync:hunter2@github.com/owner/vault.git",
				"OBSYNC_TOKEN_FILE=" + credentialFile(t, "a-token")},
		},
		{
			// ssh has no use for a password — git hands the URL to ssh, which
			// takes a key — so one here is a secret in a URL under a scheme
			// that cannot even spend it, and the echo is where it would surface.
			name:     "ssh, where only the login name is not a secret",
			env:      []string{"OBSYNC_REPO=ssh://git:hunter2@git.lan/owner/vault.git"},
			survives: "git@git.lan",
		},
	} {
		t.Run(embedded.name, func(t *testing.T) {
			t.Parallel()

			loop := startLoop(t, embedded.env...)
			line := loop.awaitLine(startupLine)

			if stderr := loop.stderr(); strings.Contains(stderr, "hunter2") {
				t.Errorf("obsync wrote %q, want the credential absent entirely — not redacted, not its "+
					"first four characters: absent (§8)", stderr)
			}
			if embedded.survives != "" && !strings.Contains(line, embedded.survives) {
				t.Errorf("the startup line is %q, want it to still carry %q — the echo is what an "+
					"operator diffs against their compose file", line, embedded.survives)
			}
		})
	}
}

// credentialFile writes a token file and returns its path. Its contents are
// never obsync's business until git asks for a credential, which is what makes
// a rotated token heal itself with no restart (§8).
func credentialFile(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("writing a credential file: %v", err)
	}
	return path
}

// Every one of the nine, set to something other than its default: the surface
// is closed, so a name obsync does not recognise here is a knob that has gone
// missing, and a WARN or an ERROR on a valid block is obsync refusing its own
// declared surface.
func TestEveryVariableOnTheConfigSurfaceIsRecognisedAndEchoed(t *testing.T) {
	t.Parallel()

	loop := startLoop(t,
		"OBSYNC_REPO=ssh://git@git.lan/owner/vault.git",
		"OBSYNC_VAULT_PATH=/vaults/notes",
		"OBSYNC_BRANCH=vault-live",
		"OBSYNC_TOKEN_FILE="+credentialFile(t, "a-token"),
		"OBSYNC_USERNAME=oauth2",
		"OBSYNC_SIZE_CEILING=40MB",
		"OBSYNC_AUTHOR_NAME=vault-bot",
		"OBSYNC_AUTHOR_EMAIL=vault-bot@example.invalid",
		"OBSYNC_LOG_LEVEL=debug",
	)
	line := loop.awaitLine(startupLine)

	for _, want := range []string{
		"repo=ssh://git@git.lan/owner/vault.git",
		"remote=git.lan/owner/vault",
		"vault_path=/vaults/notes",
		"branch=vault-live",
		"username=oauth2",
		"size_ceiling=40MB",
		"author_name=vault-bot",
		"author_email=vault-bot@example.invalid",
		"log_level=debug",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup line is %q, want it to carry %q", line, want)
		}
	}
	// The refusal obsync would report is its config error, named rather than
	// matched by level: this vault path does not exist, so the sync loop has
	// its own ERROR to log about it, and that one is a gate doing its job
	// rather than a variable being refused.
	if stderr := loop.stderr(); strings.Contains(stderr, "unknown variable") || strings.Contains(stderr, "obsync cannot start") {
		t.Errorf("obsync wrote %q for a block holding nothing but its own nine variables, want none "+
			"of them refused or unrecognised", stderr)
	}
}

// A typo is warned about and never fatal: a variable obsync ignored silently
// leaves its operator certain they set something they did not, and exiting
// would make a newer compose file bring down an older image (§8).
func TestAnUnknownVariableWarnsAndNeverExits(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.lan/owner/vault.git", "OBSYNC_SIZE_CEILINGG=500MB")

	warning := loop.awaitLine("OBSYNC_SIZE_CEILINGG")
	if !strings.Contains(warning, "level=WARN") {
		t.Errorf("obsync reported the unknown variable as %q, want a WARN (§9)", warning)
	}
	loop.awaitLine(startupLine)
	if !loop.running() {
		t.Error("obsync exited over an unknown variable, want it to carry on with the rest of the block (§8)")
	}
	if exitCode := loop.stopAndWait(); exitCode != 0 {
		t.Errorf("obsync exited %d on SIGTERM, want 0", exitCode)
	}
}

// The remote name is not a knob and there is no plan for it to become one
// (§3), so OBSYNC_REMOTE is a name obsync does not have rather than one it
// half-supports.
func TestTheRemoteNameIsNotConfigurable(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.lan/owner/vault.git", "OBSYNC_REMOTE=upstream")
	loop.awaitLine("OBSYNC_REMOTE")
	loop.awaitLine(startupLine)

	if stderr := loop.stderr(); strings.Contains(stderr, "upstream") {
		t.Errorf("obsync wrote %q, want OBSYNC_REMOTE named as unknown and its value never adopted", stderr)
	}
}

// Plain http:// earns a loud WARN and never a refusal: this audience
// self-hosts, and refusing would reject a real LAN deployment to protect
// against a threat model its operator has already accepted (§8).
func TestAPlainHTTPRemoteWarnsAndIsNeverRefused(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=http://git.lan/owner/vault.git", "OBSYNC_TOKEN_FILE="+credentialFile(t, "a-token"))

	warning := loop.awaitLine("http://")
	if !strings.Contains(warning, "level=WARN") {
		t.Errorf("obsync reported the plain http:// remote as %q, want a WARN (§9)", warning)
	}
	loop.awaitLine(startupLine)
	if !loop.running() {
		t.Error("obsync exited over a plain http:// remote, want it synced anyway (§8)")
	}
}

// §9's levels are a closed list of four, and ERROR passes all of them: a
// deployment that cannot start is loud however quiet its operator asked obsync
// to be.
func TestEveryLogLevelIsAcceptedAndNoneOfThemSilencesAnError(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			t.Parallel()

			_, stderr, exitCode := runObsync(t, []string{"OBSYNC_LOG_LEVEL=" + level})

			if exitCode != 1 {
				t.Errorf("obsync exited %d with no OBSYNC_REPO, want 1", exitCode)
			}
			if strings.Contains(stderr, "not a log level") {
				t.Errorf("obsync refused the log level %q: %s", level, stderr)
			}
			if !strings.Contains(stderr, "level=ERROR") || !strings.Contains(stderr, "OBSYNC_REPO") {
				t.Errorf("obsync wrote %q at log level %q, want the config error reported at ERROR", stderr, level)
			}
		})
	}
}

func TestTheLogLevelFiltersWhatObsyncSays(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.lan/owner/vault.git",
		"OBSYNC_SIZE_CEILINGG=500MB", "OBSYNC_LOG_LEVEL=warn")
	loop.awaitLine("OBSYNC_SIZE_CEILINGG")

	if stderr := loop.stderr(); strings.Contains(stderr, startupLine) {
		t.Errorf("obsync wrote %q at log level warn, want the startup line to be the INFO it is (§9)", stderr)
	}
}

func TestAnUnknownLogLevelIsAConfigErrorAndExits(t *testing.T) {
	t.Parallel()

	_, stderr, exitCode := runObsync(t, []string{"OBSYNC_REPO=ssh://git@git.lan/owner/vault.git", "OBSYNC_LOG_LEVEL=loud"})

	if exitCode != 1 {
		t.Errorf("obsync exited %d for an unknown log level, want 1 (§8)", exitCode)
	}
	if !strings.Contains(stderr, "OBSYNC_LOG_LEVEL") {
		t.Errorf("obsync wrote %q, want it to name the variable it could not read", stderr)
	}
}

// The credential file is required iff the repo URL is http(s):// — the two
// schemes that authenticate with a credential. ssh takes a key and file://
// takes nothing, which is why SSH needs no knobs at all (§8).
func TestTheCredentialFileIsRequiredForAnHTTPRemoteAndForNoOther(t *testing.T) {
	t.Parallel()

	for _, scheme := range []struct {
		name     string
		repo     string
		required bool
	}{
		{"https", "https://github.com/owner/vault.git", true},
		{"http", "http://git.lan/owner/vault.git", true},
		{"ssh", "ssh://git@github.com/owner/vault.git", false},
		{"scp-style", "git@github.com:owner/vault.git", false},
		{"file", "file:///srv/git/vault.git", false},
	} {
		t.Run(scheme.name, func(t *testing.T) {
			t.Parallel()

			if !scheme.required {
				loop := startLoop(t, "OBSYNC_REPO="+scheme.repo)
				loop.awaitLine(startupLine)
				if !loop.running() {
					t.Errorf("obsync exited for %s with no credential file, want it to run without one", scheme.repo)
				}
				return
			}

			_, stderr, exitCode := runObsync(t, []string{"OBSYNC_REPO=" + scheme.repo})
			if exitCode != 1 {
				t.Errorf("obsync exited %d for %s with no credential file, want 1", exitCode, scheme.repo)
			}
			if !strings.Contains(stderr, "OBSYNC_TOKEN_FILE") {
				t.Errorf("obsync wrote %q, want it to name the variable it needs", stderr)
			}
		})
	}
}

// Unreadable at startup is a config error. Unreadable later is not — that is
// the self-healing bad-credential tier, and it is what makes a rotated token
// recover with no restart (§7, §8).
func TestACredentialFileThatCannotBeReadIsAConfigErrorAndExits(t *testing.T) {
	t.Parallel()

	for _, broken := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"a path that is not there", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }},
		{"a directory", func(t *testing.T) string { return t.TempDir() }},
		// A named pipe is not a plausible credential file; it is here because
		// it is the shape whose open blocks rather than fails. obsync installs
		// its SIGTERM handler before it resolves anything, so a startup that
		// blocks is a container that never starts, says nothing, and takes a
		// SIGKILL to stop.
		{"a named pipe, whose open blocks rather than failing", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "pipe")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatalf("making a named pipe: %v", err)
			}
			return path
		}},
	} {
		t.Run(broken.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, exitCode := runObsync(t, []string{
				"OBSYNC_REPO=https://github.com/owner/vault.git",
				"OBSYNC_TOKEN_FILE=" + broken.path(t),
			})

			if exitCode != 1 {
				t.Errorf("obsync exited %d for %s as a credential file, want 1 (§8)", exitCode, broken.name)
			}
			if !strings.Contains(stderr, "OBSYNC_TOKEN_FILE") {
				t.Errorf("obsync wrote %q, want it to name the variable it could not read", stderr)
			}
		})
	}
}

// The other side of the table above, and the reason it has to be stated rather
// than left implied: Docker Swarm and Kubernetes both mount a secret as a
// symlink into a directory of them, so the shape obsync is most likely to be
// handed in production is not a plain regular file. A shape check tightened to
// look at the link rather than what it points at would refuse every one of
// those deployments at startup, by exiting 1 (§8).
func TestACredentialFileMountedAsASymlinkIsAnOrdinaryCredentialFile(t *testing.T) {
	t.Parallel()

	link := filepath.Join(t.TempDir(), "token")
	if err := os.Symlink(credentialFile(t, "a-token"), link); err != nil {
		t.Fatalf("mounting the credential file as a symlink: %v", err)
	}

	loop := startLoop(t, "OBSYNC_REPO=https://github.com/owner/vault.git", "OBSYNC_TOKEN_FILE="+link)
	loop.awaitLine(startupLine)
	if !loop.running() {
		t.Errorf("obsync did not start with a credential file mounted as a symlink, which is what "+
			"a Docker or Kubernetes secret is; it said:\n%s", loop.stderr())
	}
	loop.stopAndWait()
}

// A size takes a human suffix and never raw bytes (§8), and the startup line
// echoes it back in the same form so that what an operator set and what obsync
// resolved are comparable at a glance.
func TestASizeCeilingTakesAHumanSuffix(t *testing.T) {
	t.Parallel()

	for _, size := range []struct{ set, echoed string }{
		{"40MB", "40MB"},
		{"1GB", "1GB"},
		{"500KB", "500KB"},
		{"95mb", "95MB"},
		{"1048576B", "1MB"},
	} {
		t.Run(size.set, func(t *testing.T) {
			t.Parallel()

			loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.lan/owner/vault.git", "OBSYNC_SIZE_CEILING="+size.set)
			line := loop.awaitLine(startupLine)

			if want := "size_ceiling=" + size.echoed + " "; !strings.Contains(line, want) {
				t.Errorf("obsync resolved a size ceiling of %q to a line reading %q, want %q", size.set, line, want)
			}
		})
	}
}

// Every problem in the block, not just the first. Each one an operator finds
// separately costs them a redeploy, and a container handed three wrong values
// should be repairable in one pass (§8).
func TestEveryProblemInABlockIsReportedRatherThanTheFirst(t *testing.T) {
	t.Parallel()

	_, stderr, exitCode := runObsync(t, []string{
		"OBSYNC_REPO=https://github.com/owner/vault.git", // http(s), and no credential file with it
		"OBSYNC_SIZE_CEILING=104857600",
		"OBSYNC_LOG_LEVEL=loud",
	})

	if exitCode != 1 {
		t.Errorf("obsync exited %d for a block holding three config errors, want 1", exitCode)
	}
	for _, want := range []string{"OBSYNC_TOKEN_FILE", "OBSYNC_SIZE_CEILING", "OBSYNC_LOG_LEVEL"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("obsync wrote %q, want it to name %s too — all three problems are in the block "+
				"and finding them one redeploy at a time is what reporting the first costs", stderr, want)
		}
	}
}

func TestASizeCeilingObsyncCannotReadIsAConfigErrorAndExits(t *testing.T) {
	t.Parallel()

	for _, unreadable := range []string{"104857600", "95 MB", "-1MB", "0MB", "95TB", "MB"} {
		t.Run(unreadable, func(t *testing.T) {
			t.Parallel()

			_, stderr, exitCode := runObsync(t, []string{
				"OBSYNC_REPO=ssh://git@git.lan/owner/vault.git",
				"OBSYNC_SIZE_CEILING=" + unreadable,
			})

			if exitCode != 1 {
				t.Errorf("obsync exited %d for a size ceiling of %q, want 1 (§8)", exitCode, unreadable)
			}
			if !strings.Contains(stderr, "OBSYNC_SIZE_CEILING") {
				t.Errorf("obsync wrote %q, want it to name the variable it could not read", stderr)
			}
		})
	}
}
