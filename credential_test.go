package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
)

// The credential path (#36), driven at both of its ends: the subcommand git
// invokes, and a whole sync run against a remote that demands a credential.
//
// The remote here is reached over http:// rather than file://, and that is the
// only thing this file does differently from the rest of seam 1. It is not a
// concession — file:// authenticates with nothing, so it is precisely the route
// that cannot see any of this. What sits behind the URL is the same real
// `git init --bare` remote every other test uses, served by real
// git-http-backend, so the suite still runs offline, on a fork's PR, with no
// credential of anyone's.

// TestTheCredentialHelperSpeaksGitsCredentialProtocol drives the protocol the
// way git drives it, through the process boundary.
//
// Measured against both points of the git matrix before it was claimed: git
// appends the operation to the configured command, so the argv is
// `credential-helper get`, and it writes the request as key=value lines and
// closes stdin without a trailing blank line.
func TestTheCredentialHelperSpeaksGitsCredentialProtocol(t *testing.T) {
	t.Parallel()

	// The request git writes for a push to an https remote. useHttpPath is off
	// by default, so the path is not part of it.
	const request = "protocol=https\nhost=git.example.com\n"

	for _, c := range []struct {
		name string
		// operation is the word git appends: get, store or erase.
		operation string
		// credential is what the file holds; absent leaves the file out of the
		// environment entirely, which is an ssh:// or file:// deployment.
		credential string
		absent     bool
		// unreadable points the variable at something no credential can come
		// out of, in the shape the case names.
		unreadable func(t *testing.T) string
		username   string
		stdout     string
		exitCode   int
	}{{
		name:       "a credential file is read and answered",
		operation:  "get",
		credential: "ghp_the_operators_token\n",
		stdout:     "username=obsync\npassword=ghp_the_operators_token\n",
	}, {
		// A token file written by an operator's editor ends with a newline,
		// and one written by `printf` does not. Both name the same token.
		name:       "a credential file with no trailing newline",
		operation:  "get",
		credential: "ghp_the_operators_token",
		stdout:     "username=obsync\npassword=ghp_the_operators_token\n",
	}, {
		// GitLab needs oauth2 and Gitea the real login name, which is the
		// whole reason the username is a knob (§8).
		name:       "the username is the one the operator configured",
		operation:  "get",
		credential: "glpat_the_operators_token\n",
		username:   "oauth2",
		stdout:     "username=oauth2\npassword=glpat_the_operators_token\n",
	}, {
		// ssh:// and file:// take no credential file, and git may still ask.
		// Answering nothing is what a helper with nothing to say does.
		name:      "no credential file configured",
		operation: "get",
		absent:    true,
	}, {
		name:       "a credential file that is not there",
		operation:  "get",
		unreadable: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
		exitCode:   1,
	}, {
		// The mount point rather than the file inside it, which is how a
		// compose file gets this wrong.
		name:       "a credential file that is a directory",
		operation:  "get",
		unreadable: func(t *testing.T) string { return t.TempDir() },
		exitCode:   1,
	}, {
		name:       "an empty credential file",
		operation:  "get",
		credential: "\n",
		exitCode:   1,
	}, {
		// git's protocol is one key per line, so a value carrying a newline
		// would arrive as a second key. Refusing is the only safe reading.
		name:       "a credential file holding more than one line",
		operation:  "get",
		credential: "ghp_the_operators_token\nquit=1\n",
		exitCode:   1,
	}, {
		// store is git offering the credential back for safekeeping. obsync
		// keeps nothing: never a .git-credentials file, never a ~/.netrc (§8).
		name:       "store keeps nothing and says nothing",
		operation:  "store",
		credential: "ghp_the_operators_token\n",
	}, {
		name:       "erase has nothing to erase",
		operation:  "erase",
		credential: "ghp_the_operators_token\n",
	}, {
		// git defines three operations and tells helpers to ignore any other,
		// so that it can add one without breaking them.
		name:       "an operation git has not invented yet is ignored",
		operation:  "approve",
		credential: "ghp_the_operators_token\n",
	}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			env := []string{}
			switch {
			case c.unreadable != nil:
				env = append(env, "OBSYNC_TOKEN_FILE="+c.unreadable(t))
			case !c.absent:
				env = append(env, "OBSYNC_TOKEN_FILE="+writeCredential(t, c.credential))
			}
			if c.username != "" {
				env = append(env, "OBSYNC_USERNAME="+c.username)
			}

			stdout, stderr, exitCode := runHelper(t, env, request, c.operation)

			if stdout != c.stdout {
				t.Errorf("obsync credential-helper %s wrote %q to stdout, want %q",
					c.operation, stdout, c.stdout)
			}
			if exitCode != c.exitCode {
				t.Errorf("obsync credential-helper %s exited %d, want %d",
					c.operation, exitCode, c.exitCode)
			}
			// git relays a helper's stderr to its own, and obsync logs git's
			// stderr — so a helper that writes there is a helper whose output
			// can reach a log, at any level (§9). It writes nowhere but
			// stdout, whatever happens.
			if stderr != "" {
				t.Errorf("obsync credential-helper %s wrote %q to stderr, want nothing anywhere but "+
					"stdout — the credential helper's own output is never logged, at any level (§9)",
					c.operation, stderr)
			}
		})
	}
}

// obsync's whole credential mechanism is a subcommand and a file the operator
// mounted. There is no daemon, no socket, and nothing written down: never a
// `.git-credentials` file and never a `~/.netrc` (§8).
func TestTheCredentialHelperWritesNothingItWasHandedAnywhere(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	env := []string{"HOME=" + home, "OBSYNC_TOKEN_FILE=" + writeCredential(t, "ghp_the_operators_token\n")}

	// store carries the credential itself, which is what git's own `store`
	// helper writes to ~/.git-credentials and what a `netrc` helper would put
	// in ~/.netrc.
	for _, operation := range []string{"get", "store", "erase"} {
		if _, _, exitCode := runHelper(t,
			env,
			"protocol=https\nhost=git.example.com\nusername=obsync\npassword=ghp_the_operators_token\n",
			operation,
		); exitCode != 0 {
			t.Errorf("obsync credential-helper %s exited %d, want 0", operation, exitCode)
		}
	}

	left, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("reading the home directory back: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, len(left))
		for i, entry := range left {
			names[i] = entry.Name()
		}
		t.Errorf("the credential helper left %v in an empty home directory, want nothing — never a "+
			".git-credentials file, never a ~/.netrc (§8)", names)
	}
}

// The whole path, end to end: an operator mounts a token file, obsync
// authenticates to an https remote, and the vault's note arrives.
func TestAVaultPushesToARemoteThatDemandsACredential(t *testing.T) {
	t.Parallel()

	const credential = "ghp_the_operators_token"
	env, remote := newAuthenticatedVault(t, writeCredential(t, credential+"\n"), credential)
	env.writeNote("Daily/2026-08-24.md", "the note I wrote in my browser\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I wrote in my browser\n" {
		t.Errorf("the remote holds %q at the vault's path, want the bytes the vault holds", got)
	}
	if got, want := remote.handed(), "obsync:"+credential; !slices.Contains(got, want) {
		t.Errorf("the remote was handed %v, want %q — the username beside the credential is the "+
			"one the config surface resolved (§8)", got, want)
	}
}

// An operator rotating an expired token writes a file, and obsync heals. The
// credential is re-read every time git asks for it, per invocation, by
// construction — which is the reason the secret is a file at all, since a live
// process cannot see a changed environment variable (§8).
//
// The loop is never stopped between the two runs, so nothing here is a restart:
// the assertions in the middle read the two repositories directly rather than
// through the helpers that stop the loop first.
func TestReplacingTheCredentialFileHealsAFailedPushWithNoRestart(t *testing.T) {
	t.Parallel()

	const rotated = "ghp_the_new_token"
	credentialFile := writeCredential(t, "ghp_the_expired_token\n")
	env, remote := newAuthenticatedVault(t, credentialFile, rotated)
	env.writeNote("Daily/2026-08-24.md", "written while the token was expired\n")

	env.wake()

	if _, code := env.git(env.remote, "cat-file", "-e", "refs/heads/main:Daily/2026-08-24.md"); code == 0 {
		t.Error("the remote holds the note although the credential was refused, want a push that " +
			"did not happen")
	}
	// A bad credential is a network-half failure, so the local half is
	// untouched: the vault keeps being captured and the commit is there to
	// push the moment the token is right (§7).
	if got, want := strings.TrimSpace(env.mustGit(env.vault, "rev-list", "--count", "refs/heads/main")), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a credential the remote refuses is a "+
			"network-half failure and the local half keeps committing (§2, §7)", got, want)
	}

	if err := os.WriteFile(credentialFile, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatalf("rotating the credential file: %v", err)
	}
	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "written while the token was expired\n" {
		t.Errorf("the remote holds %q, want the note the expired token could not push", got)
	}
	if got, want := remote.handed(), "obsync:"+rotated; !slices.Contains(got, want) {
		t.Errorf("the remote was handed %v, want %q — the file is re-read every time git asks, so "+
			"a rotated credential heals with no restart (§8)", got, want)
	}
}

// The token never reaches a URL, an argv, a log line, `git remote -v`, or a
// file obsync wrote — which is what makes DEBUG logging safe to turn on, and
// DEBUG is what this runs at, where every git invocation is echoed with its
// full argv (§9).
func TestTheCredentialReachesNoLogNoURLAndNoFileObsyncWrote(t *testing.T) {
	t.Parallel()

	const credential = "ghp_a_token_that_must_never_appear"
	env, _ := newAuthenticatedVault(t, writeCredential(t, credential+"\n"), credential, "OBSYNC_LOG_LEVEL=debug")
	env.writeNote("Daily/2026-08-24.md", "pushed with a credential\n")

	env.wake()

	// Read while obsync still holds it: the private git configuration is
	// removed when the loop closes, and every assertion helper below stops the
	// loop before it looks at anything.
	privateConfig := privateGitConfig(t)

	// The credential worked, so what follows is about a token that was really
	// spent rather than one that was never asked for.
	if got := env.remoteFile("Daily/2026-08-24.md"); got != "pushed with a credential\n" {
		t.Fatalf("the remote holds %q, want the note the credential was spent on", got)
	}

	said := env.said()
	// Without this the rest is vacuous: a log obsync never wrote holds no
	// credential either.
	if !strings.Contains(said, "level=DEBUG") || !strings.Contains(said, "argv=") {
		t.Fatalf("obsync said %q, want the DEBUG log that carries every git invocation with its "+
			"full argv (§9)", said)
	}
	if strings.Contains(said, credential) {
		t.Errorf("obsync's own log carries the credential; it said %q", said)
	}

	// What an operator runs when they want to know where their vault goes.
	if out := env.mustGit(env.vault, "remote", "-v"); strings.Contains(out, credential) {
		t.Errorf("git remote -v reports %q, want a remote with no credential in it (§8)", out)
	}
	for _, path := range filesUnder(t, env.vault) {
		if strings.Contains(readFile(t, path), credential) {
			t.Errorf("%s holds the credential, want it nowhere obsync wrote (§8)", path)
		}
	}
	// The private git config is the one file obsync writes outside the vault
	// in this build, and it is where a helper configured the lazy way — a
	// credential handed to `git credential approve`, or a URL with the token
	// in it — would put the secret.
	for path, content := range privateConfig {
		if strings.Contains(content, credential) {
			t.Errorf("obsync's private git configuration at %s holds the credential, want the "+
				"credential handed to git only when git asks for it (§8)", path)
		}
	}
}

// obsync's own path is quoted where git is told how to run it, because git runs
// a `!` credential.helper value through a shell and a path is not a shell word.
//
// This is also the one test where the credential helper git invokes is the
// shipped binary rather than obsync's code inside the suite: the loop runs as
// its own process, started from an installation directory a shell would
// otherwise read as three words and a quote.
func TestTheShippedBinaryAuthenticatesFromAPathThatIsNotAShellWord(t *testing.T) {
	t.Parallel()

	const credential = "ghp_the_operators_token"
	base := t.TempDir()
	installed := installObsync(t, filepath.Join(base, "an odd 'install' dir"))

	remotePath := filepath.Join(base, "remote.git")
	mustGit(t, base, "init", "--bare", "--quiet", "-b", "main", remotePath)
	remote := serveRemote(t, base, remotePath, credential)

	vault := filepath.Join(base, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "Daily"), 0o755); err != nil {
		t.Fatalf("creating the vault: %v", err)
	}
	mustGit(t, vault, "init", "--quiet", "-b", "main")
	mustGit(t, vault, "remote", "add", config.RemoteName, remote.url)
	writeFile(t, filepath.Join(vault, ".obsidian", "app.json"), "{}\n")
	mustGit(t, vault, "add", "-A")
	mustGit(t, vault, append(append([]string{}, humanIdentity...), "commit", "--quiet", "-m", "the vault before obsync")...)
	mustGit(t, vault, "push", "--quiet", "file://"+remotePath, "refs/heads/main:refs/heads/main")

	writeFile(t, filepath.Join(vault, "Daily", "2026-08-24.md"), "pushed by the installed obsync\n")

	loop := startLoopFrom(t, installed,
		"PATH="+os.Getenv("PATH"),
		"HOME="+base,
		"OBSYNC_REPO="+remote.url,
		"OBSYNC_VAULT_PATH="+vault,
		"OBSYNC_TOKEN_FILE="+writeCredential(t, credential+"\n"),
	)
	loop.awaitLine("msg=pushed")
	loop.stopAndWait()

	if got := mustGit(t, remotePath, "cat-file", "blob", "refs/heads/main:Daily/2026-08-24.md"); got != "pushed by the installed obsync\n" {
		t.Errorf("the remote holds %q, want the note the installed obsync pushed", got)
	}
	if got, want := remote.handed(), "obsync:"+credential; !slices.Contains(got, want) {
		t.Errorf("the remote was handed %v, want %q", got, want)
	}
}

// installObsync puts a copy of the built binary where a test wants it, which is
// how a test says anything about where obsync is installed.
func installObsync(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	built, err := os.ReadFile(obsyncBin)
	if err != nil {
		t.Fatalf("reading the built obsync: %v", err)
	}
	installed := filepath.Join(dir, "obsync")
	if err := os.WriteFile(installed, built, 0o755); err != nil {
		t.Fatalf("installing obsync at %s: %v", installed, err)
	}
	return installed
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the folder for %q: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
}

// mustGit is runGit for a test that has no vault environment to hang it off.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, code := runGit(t, dir, args...)
	if code != 0 {
		t.Fatalf("git %s in %s exited %d: %s", strings.Join(args, " "), dir, code, out)
	}
	return out
}

// newAuthenticatedVault is newVault with the remote reached over http and
// demanding the credential accepted, which is the deployment the credential
// path exists for.
func newAuthenticatedVault(t *testing.T, credentialFile, accepted string, extra ...string) (*vaultEnv, *authenticatedRemote) {
	t.Helper()

	var remote *authenticatedRemote
	env := newVaultReachedBy(t, func(e *vaultEnv) (string, []string) {
		remote = serveRemote(t, filepath.Dir(e.remote), e.remote, accepted)
		return remote.url, append([]string{"OBSYNC_TOKEN_FILE=" + credentialFile}, extra...)
	})
	return env, remote
}

// authenticatedRemote is the bare remote behind an http endpoint that demands
// a Basic credential, and the record of every credential it was handed.
type authenticatedRemote struct {
	url string

	mu   sync.Mutex
	seen []string
}

func (r *authenticatedRemote) record(username, credential string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, username+":"+credential)
}

// handed is every `username:credential` pair the remote was offered, which is
// the only place a test can see what obsync's helper actually answered.
func (r *authenticatedRemote) handed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.seen...)
}

// serveRemote puts real git-http-backend in front of the bare remote, behind
// the Basic auth an operator's remote asks for.
func serveRemote(t *testing.T, projectRoot, remotePath, accepted string) *authenticatedRemote {
	t.Helper()

	// A bare repo takes a push over http only when it says so. It is the
	// remote's setting and the remote's business; obsync neither reads nor
	// writes it.
	if out, code := runGit(t, remotePath, "config", "http.receivepack", "true"); code != 0 {
		t.Fatalf("configuring the remote to take a push over http: %s", out)
	}

	// Named rather than assumed: a git built without http support has no
	// git-http-backend, and the failure that follows would otherwise present
	// as an unexplained 500 from a server the test itself started.
	backendPath := filepath.Join(gitExecPath(t), "git-http-backend")
	if _, err := os.Stat(backendPath); err != nil {
		t.Fatalf("this git ships no git-http-backend at %s, so it cannot serve the remote the "+
			"credential path needs: %v", backendPath, err)
	}

	backend := &cgi.Handler{
		Path: backendPath,
		Env:  []string{"GIT_PROJECT_ROOT=" + projectRoot, "GIT_HTTP_EXPORT_ALL=1"},
	}

	remote := &authenticatedRemote{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, credential, ok := r.BasicAuth()
		if ok {
			remote.record(username, credential)
		}
		if !ok || credential != accepted {
			w.Header().Set("WWW-Authenticate", `Basic realm="obsync"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	remote.url = server.URL + "/" + filepath.Base(remotePath)
	return remote
}

func gitExecPath(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("asking git where its own commands live: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runHelper runs obsync's credential-helper subcommand the way git runs it: the
// operation as the one argument, the request on stdin, and the answer on
// stdout.
func runHelper(t *testing.T, env []string, request string, operation string) (stdout, stderr string, exitCode int) {
	t.Helper()

	// A bound on failure rather than a wait: a helper git is waiting on must
	// answer or fail, and one that does neither is what an interactive prompt
	// would be (§8).
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, obsyncBin, "credential-helper", operation)
	cmd.Env = append([]string{}, env...)
	cmd.Stdin = strings.NewReader(request)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatalf("obsync credential-helper %s had not answered after 30s", operation)
	case errors.As(err, &exit):
		exitCode = exit.ExitCode()
	default:
		t.Fatalf("running obsync credential-helper %s: %v", operation, err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// writeCredential writes a credential file an operator would have mounted.
func writeCredential(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the credential file: %v", err)
	}
	return path
}

// filesUnder is every regular file in a tree, which for the vault includes
// everything under .git — the config, the index, the refs and the objects.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()

	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return paths
}

// privateGitConfig is every file in every private git configuration obsync
// currently has open, read now. Other tests' obsyncs are swept up with this
// one's, which costs nothing: the credential must be in none of them.
func privateGitConfig(t *testing.T) map[string]string {
	t.Helper()

	dirs, err := filepath.Glob(filepath.Join(os.TempDir(), "obsync-git-config*"))
	if err != nil {
		t.Fatalf("looking for obsync's private git configuration: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("obsync wrote no private git configuration, want the one every git it runs reads (§1)")
	}

	files := make(map[string]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A parallel test's obsync closing its own configuration while
			// this one reads is not this test's business.
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			files[path] = string(content)
		}
	}
	if len(files) == 0 {
		t.Fatal("obsync's private git configuration holds no file, want the one every git it runs reads (§1)")
	}
	return files
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}
