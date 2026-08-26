// Package git drives git as a subprocess, which is the only way obsync ever
// touches a repository.
//
// There is no library binding on any path, including reads (§1). Every decision
// in this design is expressed in git plumbing — merge-tree --write-tree,
// commit-tree, reset --keep, an exclude file, a git add pathspec — and the
// image has to carry a git anyway, so a binding buys nothing and loses
// fidelity. For the same reason nothing here is an interface: git is not a
// dependency obsync injects, and a fake one would test obsync's beliefs about
// git rather than git.
//
// The subprocess boundary is therefore the whole risk surface, and the rules on
// it are absolute: machine formats and NUL separation everywhere, the
// environment pinned per invocation, obsync's own configuration in a
// per-process file that the vault's own .git/config outranks, every git in its
// own process group, and a deadline on network commands only.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andyroberts2/obsync/internal/clock"
	"github.com/andyroberts2/obsync/internal/config"
)

// networkDeadline converts a hung network git into a countable failure (§1).
// It is the only deadline in obsync: a local git is never timed out, because a
// hung local command means the disk or the kernel is in trouble and killing a
// reset --keep halfway manufactures the one unrecoverable state in this design.
const networkDeadline = 120 * time.Second

// shutdownDeadline is how long obsync has to exit after a SIGTERM (§1). The
// run in flight finishes rather than being interrupted, and the one thing in it
// that can be waiting on the outside world is a network git, so that is what
// this cuts short — a local git is never timed out, at shutdown or at any other
// moment, because killing a reset --keep halfway manufactures the one
// unrecoverable state in this design. The reference compose therefore documents
// a stop_grace_period longer than Docker's 10s default.
const shutdownDeadline = 30 * time.Second

// killGrace is how long a signalled git has to finish dying before the process
// group is SIGKILLed. Long enough for git to unwind and drop its lock files —
// leaving an index.lock behind costs the next run — and short enough that it
// cannot eat the ~30s obsync has to exit in after a SIGTERM (§1).
const killGrace = 5 * time.Second

// Repo is the vault's git repository, and the only thing that runs git against
// it. One process holds one, because obsync syncs one vault (§8).
type Repo struct {
	// vault is the working tree every command runs in. obsync never passes
	// --git-dir or --work-tree: the repo is wherever the vault's own .git is,
	// which is what makes a vault that is a submodule or a worktree behave as
	// git says it should rather than as obsync assumed.
	vault string

	// configDir holds the private git configuration, and is removed by Close.
	// It sits outside the vault deliberately: bootstrap has to configure git
	// before there is a .git to write into (#26), and anything obsync wrote
	// inside the vault would be an owned path it would then have to declare.
	configDir  string
	configPath string

	log   *slog.Logger
	clock clock.Clock
}

// Attach opens the vault's repository and writes obsync's private git
// configuration, returning a Repo that runs every git under it.
//
// It does not decide whether the vault is one obsync may sync — the gates do
// that, per run, and they are #32. What it does is fail early and by name when
// the vault path is not a directory obsync can see at all, because every other
// failure below it would otherwise present as a chdir error from git.
func Attach(vaultPath string, identity config.CommitIdentity, log *slog.Logger, clk clock.Clock) (*Repo, error) {
	info, err := os.Stat(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("the vault at %q cannot be read: %w", vaultPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("the vault at %q is not a directory", vaultPath)
	}

	dir, err := os.MkdirTemp("", "obsync-git-config")
	if err != nil {
		return nil, fmt.Errorf("creating obsync's private git configuration: %w", err)
	}

	repo := &Repo{
		vault:      vaultPath,
		configDir:  dir,
		configPath: filepath.Join(dir, "config"),
		log:        log,
		clock:      clk,
	}
	if err := repo.writeConfig(identity); err != nil {
		_ = repo.Close()
		return nil, err
	}
	return repo, nil
}

// Close removes the private git configuration. A Repo is unusable afterwards.
func (r *Repo) Close() error {
	return os.RemoveAll(r.configDir)
}

// writeConfig writes the per-process GIT_CONFIG_GLOBAL of §1.
//
// It is written by git rather than by hand: the config format has its own
// escaping, an author name is a value obsync did not choose, and the tool that
// owns the format is already a subprocess away.
//
// What is set is a closed list, and what is left alone is as much of the
// decision as what is not:
//
//   - the commit identity, which is where provenance lives (§2);
//   - core.askPass, forced, so an interactive prompt can never hang the loop —
//     git runs it through a shell and true produces an empty credential, which
//     fails fast instead of waiting on a terminal that is not there;
//   - fetch.fsckObjects, the one integrity check proportional to what arrived
//     rather than to the size of the repo (§7);
//   - gc.autoDetach off, which forbids a detached background repack (§7).
//
// Deliberately absent: any merge strategy, so a real conflict is a real
// conflict resolved by §4's rule rather than a silent -X ours; gc.auto, which
// keeps its default; and credential.helper, which is the credential path's own
// slice (#36).
func (r *Repo) writeConfig(identity config.CommitIdentity) error {
	for _, setting := range [][2]string{
		{"user.name", identity.Name},
		{"user.email", identity.Email},
		{"core.askPass", "true"},
		{"fetch.fsckObjects", "true"},
		{"gc.autoDetach", "false"},
	} {
		if _, err := r.run(invocation{
			dir:  r.configDir,
			args: []string{"config", "--file", r.configPath, "--replace-all", setting[0], setting[1]},
		}); err != nil {
			return fmt.Errorf("writing obsync's private git configuration: %w", err)
		}
	}
	return nil
}

// HeadBranch is the branch HEAD is on. It is what the tracked branch resolves
// to when obsync attaches to a vault that is already a repo — the branch the
// human is already on, never the remote's idea of a default (§3).
//
// A detached HEAD has no answer here; it is gate 3, and a full freeze (#32).
func (r *Repo) HeadBranch() (string, error) {
	out, err := r.run(invocation{dir: r.vault, args: []string{"symbolic-ref", "--quiet", "--short", "HEAD"}})
	if err != nil {
		return "", fmt.Errorf("the vault's HEAD is not on a branch: %w", err)
	}
	branch := strings.TrimSuffix(string(out), "\n")
	if branch == "" {
		return "", errors.New("the vault's HEAD names no branch")
	}
	return branch, nil
}

// Changed is every path git reports as changed in the vault — modified,
// deleted, staged by a human, or never seen before.
//
// The watcher never contributes to this list, and that split is what keeps
// obsync correct against dropped inotify events, an exhausted watch budget and
// the third writer it cannot see: the watcher wakes the loop, and git says what
// changed (§2).
func (r *Repo) Changed() ([]string, error) {
	// -uall rather than the default: an untracked directory is otherwise
	// reported as one entry, and obsync stages paths rather than directories.
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"status", "--porcelain=v2", "-z", "-uall"},
	})
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

// Stage puts the given paths in the index, including the ones that are there to
// be removed.
//
// The paths arrive as NUL-separated literal pathspecs on stdin, which is the
// only form that is correct for a vault: a note title may contain a space, a
// newline or a glob character, and any of the three would otherwise be read as
// something other than the name of the file it is.
func (r *Repo) Stage(paths []string) error {
	var pathspecs bytes.Buffer
	for _, path := range paths {
		// :(literal) is what stops Notes/[draft] plan.md from being a
		// character class. It cannot be omitted for "ordinary" paths, because
		// obsync has no say in what a human names a note.
		pathspecs.WriteString(":(literal)")
		pathspecs.WriteString(path)
		pathspecs.WriteByte(0)
	}
	_, err := r.run(invocation{
		dir:   r.vault,
		stdin: pathspecs.Bytes(),
		args:  []string{"add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"},
	})
	return err
}

// Staged is what the index holds that HEAD does not: the change one commit
// would carry, which is what the commit message is written from.
//
// Rename detection is deliberately off. A rename is a delete plus an add, which
// is what obsync says in the message and what the human did on disk; git
// detects it at read time anyway, where the reader can ask for the threshold
// they want.
func (r *Repo) Staged() ([]Change, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"diff-index", "--cached", "--name-status", "-z", "HEAD"},
	})
	if err != nil {
		return nil, err
	}
	return parseNameStatus(out)
}

// Commit records the index as one commit, with message as its message.
//
// --cleanup=whitespace is passed rather than left to default so that the
// message obsync composed is the message it writes: the vault's own .git/config
// outranks obsync's private one, and commit.cleanup there would otherwise be
// free to rewrite it. Measured, no configured mode currently changes a line of
// it — every line obsync writes starts with a verb or a marker, and none of
// them is a comment character — so this is what keeps that true when the format
// grows a line rather than something a test can see today.
func (r *Repo) Commit(message string) error {
	_, err := r.run(invocation{
		dir:   r.vault,
		stdin: []byte(message),
		args:  []string{"commit", "--quiet", "--cleanup=whitespace", "-F", "-"},
	})
	return err
}

// HasUnpushedCommits reports whether the branch holds commits the remote is not
// known to have. It is answered from the remote-tracking ref, so it costs no
// network and is only as fresh as the last fetch or push — which is exactly
// enough to decide whether there is anything to push, and no substitute for
// classification, which fetches first and is #27's.
func (r *Repo) HasUnpushedCommits(branch string) (bool, error) {
	remoteRef := "refs/remotes/" + config.RemoteName + "/" + branch

	// for-each-ref rather than rev-parse: a ref that does not exist is not a
	// failure here, it is a branch obsync has never pushed, and for-each-ref
	// says so by printing nothing and exiting 0.
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"for-each-ref", "--format=%(objectname)", remoteRef},
	})
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return true, nil
	}

	out, err = r.run(invocation{
		dir:  r.vault,
		args: []string{"rev-list", "--count", remoteRef + "..refs/heads/" + branch},
	})
	if err != nil {
		return false, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("git rev-list --count answered %q, which is not a count: %w", out, err)
	}
	return count > 0, nil
}

// Push sends the tracked branch to the remote, and is the one network command
// in this build.
//
// The refspec is written out in full and both sides are named: obsync pushes
// one branch in each direction (§3) and never sets an upstream, because -u
// writes the vault's .git/config, which belongs to the human. There is no
// --force here and there is none anywhere, not even --force-with-lease: every
// write to the remote is a fast-forward or it does not happen.
//
// --porcelain is passed for the enum in its output, which is how a rejection is
// eventually told from a lost race (§7) — that reading is #35's, and until it
// exists a non-zero exit is simply a failed run.
func (r *Repo) Push(ctx context.Context, branch string) error {
	ref := "refs/heads/" + branch
	_, err := r.run(invocation{
		dir:      r.vault,
		args:     []string{"push", "--porcelain", config.RemoteName, ref + ":" + ref},
		deadline: networkDeadline,
		shutdown: ctx.Done(),
	})
	return err
}

// invocation is one git, described completely. Every subcommand obsync runs is a
// plain string literal at the call site and never assembled from a table or a
// variable, so that the never-list stays greppable: a grep for a subcommand
// obsync promises never to run is a proof rather than a hint.
type invocation struct {
	dir   string
	args  []string
	stdin []byte
	// deadline is set only on a network command, at networkDeadline. A local
	// command leaves it zero and is never timed out, which is §1's asymmetry
	// expressed as the absence of a timer rather than as a rule to remember.
	deadline time.Duration
	// shutdown is closed when obsync has been told to stop, and is set on the
	// same commands the deadline is and for the same reason: it is the network
	// that can hang, and a local command is left alone.
	shutdown <-chan struct{}
}

// run runs one git to completion and returns its stdout.
//
// Every invocation is pinned here rather than at startup: LC_ALL=C so git's
// machine output is the one obsync was written against, GIT_TERMINAL_PROMPT=0
// so nothing can wait on a terminal, GIT_OPTIONAL_LOCKS=0 so a status never
// fights a human's git for the index lock, GIT_CONFIG_NOSYSTEM=1 and a private
// GIT_CONFIG_GLOBAL so the only configuration in play is obsync's and the
// vault's own.
func (r *Repo) run(inv invocation) ([]byte, error) {
	started := r.clock.Now()

	cmd := exec.Command("git", inv.args...)
	cmd.Dir = inv.dir
	cmd.Env = r.env()
	// Its own process group, so that killing it kills git-remote-https and ssh
	// with it rather than leaving them holding the connection (§1).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if inv.stdin != nil {
		cmd.Stdin = bytes.NewReader(inv.stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("running git %s: %w", strings.Join(inv.args, " "), err)
	}

	// obsync waits on every git it spawns. There is no init process in the
	// image and there is not meant to be one (§1).
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	// A zero deadline is a nil channel, and a receive on a nil channel blocks
	// forever — so a local command is not timed out by construction rather
	// than by a branch someone could add an exception to. The shutdown channel
	// is nil on the same commands for the same reason.
	var expiry, shutdownExpiry <-chan time.Time
	var stopping <-chan struct{}
	if inv.deadline > 0 {
		expiry = r.clock.After(inv.deadline)
		stopping = inv.shutdown
	}

	var err error
	var timedOut error
waiting:
	for {
		select {
		case err = <-waited:
			break waiting
		case <-expiry:
			timedOut = ErrNetworkDeadline
			r.killGroup(cmd, waited)
			break waiting
		case <-stopping:
			// SIGTERM. The run finishes rather than being interrupted, but
			// obsync has ~30s to exit, so the clock on this git starts again
			// and shorter (§1).
			stopping = nil
			shutdownExpiry = r.clock.After(shutdownDeadline)
		case <-shutdownExpiry:
			timedOut = ErrShutdownDeadline
			r.killGroup(cmd, waited)
			break waiting
		}
	}

	// DEBUG carries the full argv deliberately: beliefs about git plumbing are
	// this design's whole risk surface, and a bug report carrying the argv is
	// one a maintainer replays by hand. It is safe because the credential
	// never reaches a URL or an argv — and the credential helper's own output
	// is never logged, at any level (§9).
	r.log.Debug("git",
		"argv", fmt.Sprintf("%q", inv.args),
		"exit", cmd.ProcessState.ExitCode(),
		"duration", r.clock.Now().Sub(started),
	)

	if timedOut != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(inv.args, " "), timedOut)
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("running git %s: %w", strings.Join(inv.args, " "), err)
		}
		return nil, &CommandError{
			Args:     append([]string{}, inv.args...),
			ExitCode: exit.ExitCode(),
			Stderr:   stderr.String(),
		}
	}
	return stdout.Bytes(), nil
}

// killGroup signals the process group and waits for it to die, escalating to
// SIGKILL after killGrace. The negative pid is the whole point: SIGTERM to the
// group reaches the transport helper and any hook it started, and killing only
// the git obsync started would leave them behind.
func (r *Repo) killGroup(cmd *exec.Cmd, waited <-chan error) {
	group := -cmd.Process.Pid
	_ = syscall.Kill(group, syscall.SIGTERM)
	select {
	case <-waited:
	case <-r.clock.After(killGrace):
		_ = syscall.Kill(group, syscall.SIGKILL)
		<-waited
	}
}

// env is the environment every git runs with.
//
// Inherited GIT_* variables are dropped rather than passed through: git's
// behaviour has to come from obsync's argv and obsync's configuration, and a
// GIT_DIR or a GIT_ASKPASS the container happened to be started with would
// quietly make a run mean something else. Everything else survives, because ssh
// reaches a key through HOME and that is how a key arrives (§8).
func (r *Repo) env() []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+5)
	for _, entry := range inherited {
		if strings.HasPrefix(entry, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+r.configPath,
	)
}

// ErrNetworkDeadline is a network git that ran past obsync's 120s deadline and
// was killed. It is a countable failure rather than a verdict about the remote:
// nothing was returned, so nothing was decided.
var ErrNetworkDeadline = errors.New("the network git ran past obsync's 120s deadline and was killed")

// ErrShutdownDeadline is a network git that was still running when the ~30s
// obsync has to exit in ran out. It is not a verdict about the remote and not a
// failure a human is needed for: obsync was told to stop, and what it had
// already committed is in the vault waiting for the next start.
var ErrShutdownDeadline = errors.New("obsync was stopping and the network git had not finished within its 30s")

// CommandError is a git that ran and failed, carrying what obsync needs to tell
// a human: the argv it ran, the status it exited with, and git's own words.
//
// Those words may name a failure and never escalate one — nothing in obsync
// branches on this string, because behaviour that hangs on prose written for
// humans moves when git rewords a message (§7).
type CommandError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	said := firstLine(e.Stderr)
	if said == "" {
		return fmt.Sprintf("git %s exited %d", strings.Join(e.Args, " "), e.ExitCode)
	}
	return fmt.Sprintf("git %s exited %d: %s", strings.Join(e.Args, " "), e.ExitCode, said)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
