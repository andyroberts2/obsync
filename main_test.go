package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stampedVersion is what the test build is linked with. No source file carries
// this string, so reading it back out of `obsync status` is proof the link-time
// stamp is wired end to end rather than defaulted.
const stampedVersion = "0.0.0-test+stamp"

// obsyncBin is the binary under test, built once by TestMain. The subcommands
// are tested through the real process boundary — their stdout and exit status
// are the observable behaviour (§10) — and building is the only way to see a
// value that exists solely in a linked binary.
var obsyncBin string

func TestMain(m *testing.M) {
	// A credential-helper invocation re-enters obsync rather than the suite.
	// Seam 1 drives the sync loop in this process, so the obsync a git it
	// starts finds at its own path is this test binary — and this is what
	// makes that invocation the real subcommand instead of a second copy of
	// the suite. It is obsync's own code either way: the same run() the built
	// binary calls, dispatched before m.Run parses the flags go test passes.
	if len(os.Args) > 1 && os.Args[1] == "credential-helper" {
		os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
	}

	dir, err := os.MkdirTemp("", "obsync-build")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating a build directory: %v\n", err)
		os.Exit(1)
	}
	obsyncBin = filepath.Join(dir, "obsync")

	build := exec.Command("go", "build", "-ldflags", "-X main.version="+stampedVersion, "-o", obsyncBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building obsync: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestStatusReportsTheBuildVersion(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runObsync(t, nil, "status")

	if exitCode != 0 {
		t.Errorf("obsync status exited %d, want 0 — status exits 0 always (§10)", exitCode)
	}
	if !strings.Contains(stdout, stampedVersion) {
		t.Errorf("obsync status printed %q, want it to name the build version %q", stdout, stampedVersion)
	}
	if stderr != "" {
		t.Errorf("obsync status wrote %q to stderr, want silence — a run that did what it was asked "+
			"says nothing, because an empty log is a designed signal (§9)", stderr)
	}
}

// §10's subcommands are a closed list of four, and this build recognises all
// four: healthcheck says it is not implemented rather than being refused as a
// typo, and the other three do what §10 says they do. Without a row per case, a
// subcommand dropped from the declared surface still exits non-zero and the
// suite stays green — measured, by dropping healthcheck.
func TestEveryDeclaredSubcommandIsRecognised(t *testing.T) {
	t.Parallel()

	for _, declared := range []struct {
		name string
		args []string
		// exitCode is what §10 promises for an empty environment block: 0 for
		// status, which always succeeds; 1 for healthcheck, which this build
		// has not implemented; 1 for the sync loop, which refuses a block with
		// no OBSYNC_REPO in it (§8); and 1 for credential-helper, which is
		// git's and is being invoked here without the operation git always
		// names (the protocol itself is credential_test.go's).
		exitCode int
		// quietStdout marks the subcommands §10 leaves no stdout to: the sync
		// loop, whose output is logfmt on stderr, and healthcheck, which is
		// silent. credential-helper's stdout is git's, and status's is tested
		// above.
		quietStdout bool
	}{
		{name: "the sync loop", args: nil, exitCode: 1, quietStdout: true},
		{name: "healthcheck", args: []string{"healthcheck"}, exitCode: 1, quietStdout: true},
		{name: "status", args: []string{"status"}, exitCode: 0},
		{name: "credential-helper", args: []string{"credential-helper"}, exitCode: 1},
	} {
		t.Run(declared.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, exitCode := runObsync(t, nil, declared.args...)

			if exitCode != declared.exitCode {
				t.Errorf("obsync %s exited %d, want %d (§10)", declared.name, exitCode, declared.exitCode)
			}
			if strings.Contains(stderr, "unknown subcommand") {
				t.Errorf("obsync %s wrote %q to stderr, want a subcommand §10 declares to be "+
					"recognised rather than refused as one that does not exist", declared.name, stderr)
			}
			if declared.quietStdout && stdout != "" {
				t.Errorf("obsync %s wrote %q to stdout, want stdout left to the subcommands that "+
					"print there (§9)", declared.name, stdout)
			}
		})
	}
}

// §10's default subcommand runs until SIGTERM, and that is the whole of what
// makes the rest of the config surface observable: "an unknown variable never
// exits" says nothing about a process that exits either way. So this is the
// row the subcommand table above cannot carry — the one subcommand whose
// contract is that it does not finish.
func TestTheSyncLoopRunsUntilSIGTERM(t *testing.T) {
	t.Parallel()

	loop := startLoop(t, "OBSYNC_REPO=ssh://git@git.lan/owner/vault.git")
	loop.awaitLine(startupLine)

	if !loop.running() {
		t.Error("obsync exited on its own after resolving a configuration it accepted, want it " +
			"parked until SIGTERM (§10) — a refused obsync parks alive and a satisfied one has " +
			"even less reason to leave")
	}
	if exitCode := loop.stopAndWait(); exitCode != 0 {
		t.Errorf("obsync exited %d on SIGTERM, want 0 — anything else is obsync being killed rather "+
			"than stopping (§1)", exitCode)
	}
	if stdout := loop.stdout(); stdout != "" {
		t.Errorf("the sync loop wrote %q to stdout, want logfmt on stderr and stdout left to the "+
			"subcommands (§9)", stdout)
	}
}

func TestUnknownSubcommandIsRefusedAndNamed(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runObsync(t, nil, "sync-now")

	if exitCode == 0 {
		t.Error("obsync sync-now exited 0, want non-zero for a subcommand that does not exist")
	}
	if !strings.Contains(stderr, "sync-now") {
		t.Errorf("obsync sync-now wrote %q to stderr, want it to name the subcommand it refused", stderr)
	}
	if stdout != "" {
		t.Errorf("obsync sync-now wrote %q to stdout, want stdout left to the subcommands' own output (§9)", stdout)
	}
}

// runObsync runs the built binary to completion and returns what a human or
// Docker would see.
//
// The environment block is given in full rather than inherited: it is half of
// what this seam observes (§8), so a stray OBSYNC_* variable in the test
// runner's own environment must not be able to reach obsync.
//
// testing.TB rather than *testing.T for the reason vaultEnv is: the seam-1
// harness reaches the subcommands through here, over the vault it built, which
// is the only way §9's two readers of the status file are observed at all.
func runObsync(t testing.TB, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	// The deadline is a bound on failure, not a wait for obsync: everything
	// this helper runs either does its work and exits or refuses and exits, so
	// an invocation that parked instead is a failure to report rather than a
	// suite to hang.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, obsyncBin, args...)
	cmd.Env = append([]string{}, env...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatalf("obsync %v had not exited after 30s; it wrote %q", args, errBuf.String())
	case errors.As(err, &exit):
		exitCode = exit.ExitCode()
	default:
		t.Fatalf("running obsync %v: %v", args, err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// A process is obsync running as §10's default subcommand, which runs until
// SIGTERM. It is the half of this seam that does not exit on its own, so the
// harness reads its stderr as it is written and stops it with a signal.
//
// It is named for what it is rather than for what it runs: the sync loop it
// turns is a type of its own, driven in-process by the seam-1 harness beside
// this one, because a clock and a watcher cannot be injected across a process
// boundary.
//
// It reaps obsync itself, in the same goroutine that reads its stderr, because
// that is the only way an exit is observable before the test asks for one: an
// exited child that has not been Waited for is a zombie, and a zombie takes a
// signal without complaint, so a signal-0 liveness check reports a dead obsync
// as running.
type process struct {
	t     *testing.T
	cmd   *exec.Cmd
	stop  context.CancelFunc
	lines <-chan string
	// exited is closed once obsync's stderr has ended and it has been reaped;
	// waitErr is what Wait returned and is read only after exited is closed.
	exited  <-chan struct{}
	waitErr error

	out      *strings.Builder
	seen     []string
	exitCode int
	stopped  bool
}

// parkGrace is how long running() waits for an exit before concluding obsync
// parked. obsync writes its last startup line and then either parks or returns
// immediately after it, so a second is far more slack than an exit needs to
// show up in. It can only ever under-detect — a parked obsync passes however
// loaded the machine is — so a slow runner makes the check weaker, never
// flaky.
const parkGrace = time.Second

// pendingLines is the stderr backlog the reader goroutine may hold while the
// test is not consuming: enough that obsync's whole startup — the unknown and
// http warnings, the startup line, the not-implemented warning — never blocks
// the goroutine before it reaches Wait, which is what lets running() see an
// exit the test has not read up to yet.
const pendingLines = 64

// startLoop starts obsync with the given environment block and nothing else in
// its environment.
func startLoop(t *testing.T, env ...string) *process {
	t.Helper()
	return startLoopFrom(t, obsyncBin, env...)
}

// startLoopFrom is startLoop with the binary named, for the one test that cares
// where obsync is installed rather than what it was configured with.
func startLoopFrom(t *testing.T, binary string, env ...string) *process {
	t.Helper()

	ctx, stop := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append([]string{}, env...)
	// obsync's exit path is SIGTERM (§1), so that is what stopping it means
	// here; WaitDelay is the backstop for a build that ignored the signal.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second

	var out strings.Builder
	cmd.Stdout = &out

	stderr, err := cmd.StderrPipe()
	if err != nil {
		stop()
		t.Fatalf("opening obsync's stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		stop()
		t.Fatalf("starting obsync: %v", err)
	}

	lines := make(chan string, pendingLines)
	exited := make(chan struct{})
	l := &process{t: t, cmd: cmd, stop: stop, lines: lines, exited: exited, out: &out}
	go func() {
		defer close(exited)
		scanner := bufio.NewScanner(stderr)
		// A knob's value can be far longer than a line of prose, and the
		// startup line carries all nine of them, so the default 64KB token
		// limit is raised rather than left to turn an over-long line into a
		// stderr that merely appears to have ended.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		// Wait only once the pipe has ended, which is what os/exec requires,
		// and here rather than in stopAndWait so that obsync exiting on its
		// own is a fact the harness holds before anyone asks for it.
		l.waitErr = cmd.Wait()
	}()

	t.Cleanup(func() { l.stopAndWait() })
	return l
}

// awaitLine returns obsync's first stderr line containing want.
//
// The deadline is a bound on failure, not a wait for obsync: a loop that never
// says what it was going to say fails here with everything it did say, rather
// than hanging until the suite's own timeout.
func (l *process) awaitLine(want string) string {
	l.t.Helper()

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case line, ok := <-l.lines:
			if !ok {
				l.t.Fatalf("obsync's stderr ended without a line containing %q; it wrote:\n%s",
					want, strings.Join(l.seen, "\n"))
			}
			l.seen = append(l.seen, line)
			if strings.Contains(line, want) {
				return line
			}
		case <-deadline.C:
			l.t.Fatalf("obsync wrote no line containing %q within 30s; it wrote:\n%s",
				want, strings.Join(l.seen, "\n"))
		}
	}
}

// running reports whether obsync is still parked rather than having exited.
//
// The grace is a bound on failure rather than a wait for obsync — see
// parkGrace — and the question is asked of a reaped process rather than of a
// signal, because a signal cannot tell a parked obsync from a zombie one.
func (l *process) running() bool {
	l.t.Helper()

	grace := time.NewTimer(parkGrace)
	defer grace.Stop()
	select {
	case <-l.exited:
		return false
	case <-grace.C:
		return true
	}
}

// stopAndWait sends SIGTERM, drains what obsync wrote on its way out, and
// returns its exit status. Calling it twice is harmless: the cleanup this
// harness registers is the second call in a test that stopped the loop itself.
func (l *process) stopAndWait() int {
	l.t.Helper()

	if l.stopped {
		return l.exitCode
	}
	l.stopped = true
	l.stop()
	for line := range l.lines {
		l.seen = append(l.seen, line)
	}
	<-l.exited

	// A loop that handled SIGTERM and exited cleanly comes back as the
	// cancellation that sent the signal, so the process's own status is read
	// off ProcessState either way. It is -1 when obsync was killed by the
	// signal rather than handling it, which is the distinction being tested.
	var exit *exec.ExitError
	switch err := l.waitErr; {
	case err == nil, errors.Is(err, context.Canceled), errors.As(err, &exit):
		l.exitCode = l.cmd.ProcessState.ExitCode()
	default:
		l.t.Fatalf("waiting for obsync: %v", err)
	}
	return l.exitCode
}

// stderr returns everything obsync wrote, and is meaningful once it has
// stopped: an assertion that a line is *absent* needs the whole of the output
// rather than the part of it that has arrived.
func (l *process) stderr() string {
	l.t.Helper()

	l.stopAndWait()
	return strings.Join(l.seen, "\n")
}

// stdout returns everything obsync wrote to stdout, which for the sync loop is
// nothing at all: its output is logfmt on stderr, and stdout belongs to the
// subcommands (§9).
func (l *process) stdout() string {
	l.t.Helper()

	l.stopAndWait()
	return l.out.String()
}
