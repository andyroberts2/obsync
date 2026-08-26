package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	stdout, _, exitCode := runObsync(t, "status")

	if exitCode != 0 {
		t.Errorf("obsync status exited %d, want 0 — status exits 0 always (§10)", exitCode)
	}
	if !strings.Contains(stdout, stampedVersion) {
		t.Errorf("obsync status printed %q, want it to name the build version %q", stdout, stampedVersion)
	}
}

func TestUnknownSubcommandIsRefusedAndNamed(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runObsync(t, "sync-now")

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

// runObsync runs the built binary and returns what a human or Docker would see.
func runObsync(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command(obsyncBin, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		exitCode = exit.ExitCode()
	default:
		t.Fatalf("running obsync %v: %v", args, err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}
