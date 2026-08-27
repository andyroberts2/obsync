package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
)

// The tracer bullet: point obsync at a vault whose origin is a local bare repo,
// wake it once, and the vault's change arrives in the remote (#24).
func TestOneWakeUpCommitsTheVaultAndPushesIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "the note I wrote in my browser\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I wrote in my browser\n" {
		t.Errorf("the remote holds %q at the vault's path, want the bytes the vault holds", got)
	}
}

// A vault is a hostile place to parse loosely: note titles carry spaces,
// unicode, glob and pathspec-magic characters, and — legally — a newline.
//
// Each is a path something obsync could plausibly have done would get wrong: a
// line-split or a quoted status format loses the newline one, and a pathspec
// without :(literal) refuses the colon one outright — measured, git calls it a
// pathspec that "did not match any files" and exits 128, which would fail every
// run for as long as that note existed.
func TestPathsAVaultReallyHoldsCommitAndPush(t *testing.T) {
	t.Parallel()

	hostile := []string{
		"Notes/space and ünïcode.md",
		"Notes/[draft] plan.md",
		":colon note.md",
		"Notes/#tag index.md",
		"Notes/two\nlines.md",
		// A leading dash is the one that would survive every parsing rule
		// above and still break: it is a path only because it arrives on
		// stdin, and an argv-shaped pathspec would read it as an option.
		"-dash note.md",
		"Notes/*star.md",
		"Notes/trailing space .md",
	}

	env := newVault(t)
	for i, path := range hostile {
		env.writeNote(path, fmt.Sprintf("note %d\n", i))
	}

	env.wake()

	for i, path := range hostile {
		if got := env.remoteFile(path); got != fmt.Sprintf("note %d\n", i) {
			t.Errorf("the remote holds %q at %q, want the bytes the vault holds", got, path)
		}
	}
	for _, path := range hostile {
		if body := env.remoteMessage(); !strings.Contains(body, "+ "+path) {
			t.Errorf("the commit message is %q, want it to list %q", body, path)
		}
	}
}

// §2's commit messages, which are the difference between a git log that says
// something and a wall of identical "vault backup" lines. The subject names the
// file when exactly one path changed and counts them otherwise, and the verb
// varies with the operation.
func TestTheCommitMessageSaysWhatTheRunChanged(t *testing.T) {
	t.Parallel()

	for _, message := range []struct {
		name string
		// baseline is committed by a first wake-up; change is what the second
		// one finds, and what the message under test describes.
		baseline func(env *vaultEnv)
		change   func(env *vaultEnv)
		subject  string
		body     []string
	}{
		{
			name:     "one note edited",
			baseline: func(env *vaultEnv) { env.writeNote("Daily/2026-08-24.md", "morning\n") },
			change:   func(env *vaultEnv) { env.writeNote("Daily/2026-08-24.md", "afternoon\n") },
			subject:  "Update Daily/2026-08-24.md",
		},
		{
			name:     "one note written",
			baseline: func(env *vaultEnv) {},
			change:   func(env *vaultEnv) { env.writeNote("Notes/Zettel/Bicameral mind.md", "new\n") },
			subject:  "Import Notes/Zettel/Bicameral mind.md",
		},
		{
			name: "four notes deleted on purpose",
			baseline: func(env *vaultEnv) {
				for i := range 4 {
					env.writeNote(fmt.Sprintf("Archive/old %d.md", i), "old\n")
				}
			},
			change: func(env *vaultEnv) {
				for i := range 4 {
					env.deleteNote(fmt.Sprintf("Archive/old %d.md", i))
				}
			},
			subject: "Delete 4 notes",
			body: []string{
				"- Archive/old 0.md",
				"- Archive/old 3.md",
			},
		},
		{
			name: "a note renamed and its attachment pasted",
			baseline: func(env *vaultEnv) {
				env.writeNote("Daily/2026-08-24.md", "morning\n")
				env.writeNote("Notes/Index.md", "index\n")
				env.writeNote("Archive/Old plan.md", "plan\n")
			},
			change: func(env *vaultEnv) {
				env.writeNote("Notes/Zettel/Bicameral mind.md", "new\n")
				env.writeNote("Daily/2026-08-24.md", "afternoon\n")
				env.deleteNote("Archive/Old plan.md")
			},
			subject: "Update 3 notes",
			body: []string{
				"+ Notes/Zettel/Bicameral mind.md",
				"~ Daily/2026-08-24.md",
				"- Archive/Old plan.md",
			},
		},
	} {
		t.Run(message.name, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			message.baseline(env)
			env.wake()
			message.change(env)
			env.wake()

			if got := env.remoteSubject(); got != message.subject {
				t.Errorf("the commit subject is %q, want %q (§2)", got, message.subject)
			}
			for _, line := range message.body {
				if body := env.remoteMessage(); !strings.Contains(body, line) {
					t.Errorf("the commit message is %q, want it to carry %q (§2)", body, line)
				}
			}
		})
	}
}

// A bulk import is where the message rules earn their keep: the count is
// grouped so a human can read it, and the body stops at fifty paths and counts
// the rest rather than putting a screenful between every pair of commits (§2).
func TestABulkImportIsCountedAndItsBodyIsCapped(t *testing.T) {
	t.Parallel()

	const notes = 1001

	env := newVault(t)
	for i := range notes {
		env.writeNote(fmt.Sprintf("Import/note %04d.md", i), "imported\n")
	}

	env.wake()

	if got, want := env.remoteSubject(), "Import 1,001 notes"; got != want {
		t.Errorf("the commit subject is %q, want %q (§2)", got, want)
	}

	body := strings.Split(env.remoteMessage(), "\n")[2:]
	if got, want := len(body), bodyLines+1; got != want {
		t.Errorf("the commit body is %d lines, want %d — fifty paths and the count of the rest (§2)", got, want)
	}
	if got, want := body[len(body)-1], "… and 951 more"; got != want {
		t.Errorf("the commit body ends %q, want %q (§2)", got, want)
	}
}

// bodyLines is the cap §2 puts on a commit body, restated here rather than
// imported: a test that reads the constant it is checking asserts nothing.
const bodyLines = 50

// The cap is a boundary, and a boundary is where a cap is wrong: a body of
// exactly fifty paths is complete and says nothing about a remainder, and the
// fifty-first path is the first one counted rather than listed (§2).
func TestTheBodyCapListsFiftyPathsAndCountsTheFiftyFirst(t *testing.T) {
	t.Parallel()

	for _, boundary := range []struct {
		name  string
		notes int
		// lines is how long the body is: fifty listed paths, plus the count
		// of the rest when there is a rest.
		lines int
		// lastLine is the body's final line, which is the last path listed
		// when nothing was left over and the count when something was.
		lastLine string
	}{
		{
			name:     "exactly fifty paths are all listed",
			notes:    bodyLines,
			lines:    bodyLines,
			lastLine: fmt.Sprintf("+ Import/note %04d.md", bodyLines-1),
		},
		{
			name:     "the fifty-first path is counted, not listed",
			notes:    bodyLines + 1,
			lines:    bodyLines + 1,
			lastLine: "… and 1 more",
		},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			t.Parallel()

			env := newVault(t)
			for i := range boundary.notes {
				env.writeNote(fmt.Sprintf("Import/note %04d.md", i), "imported\n")
			}

			env.wake()

			body := strings.Split(env.remoteMessage(), "\n")[2:]
			if got, want := len(body), boundary.lines; got != want {
				t.Errorf("a commit body over %d paths is %d lines, want %d (§2)", boundary.notes, got, want)
			}
			if got := body[len(body)-1]; got != boundary.lastLine {
				t.Errorf("the commit body ends %q, want %q (§2)", got, boundary.lastLine)
			}
		})
	}
}

// A human who renames a note with git rather than in their editor leaves the
// rename sitting in the index, and git then reports it as a single status
// record carrying two paths — a shape nothing else in a vault produces, and
// the only one where the path is not the ninth field.
//
// Measured: a miscounted field there does not read a wrong path, it reads
// "R100 Notes/New name.md", which is a pathspec matching no file, so `git add`
// exits 128 and every run fails for as long as the rename sits there. Renaming
// a note is not a rare thing to do to a vault.
func TestAHumanStagedRenameCommitsAsADeleteAndAnAdd(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Notes/Old name.md", "the same bytes\n")
	env.wake()

	env.mustGit(env.vault, "mv", "Notes/Old name.md", "Notes/New name.md")

	env.wake()

	if got := env.remoteFile("Notes/New name.md"); got != "the same bytes\n" {
		t.Errorf("the remote holds %q at the renamed path, want the bytes the vault holds", got)
	}
	if env.remoteHolds("Notes/Old name.md") {
		t.Error("the remote still holds the note at its old path, want the rename's delete carried too")
	}
	if got, want := env.remoteSubject(), "Update 2 notes"; got != want {
		t.Errorf("the commit subject is %q, want %q — a rename is a delete plus an add (§2)", got, want)
	}
	for _, line := range []string{"+ Notes/New name.md", "- Notes/Old name.md"} {
		if body := env.remoteMessage(); !strings.Contains(body, line) {
			t.Errorf("the commit message is %q, want it to carry %q (§2)", body, line)
		}
	}
}

// Provenance lives in the commit identity, which is why the name rather than
// the address is the part that carries meaning — it is what filtering history
// by author matches on (§8).
func TestACommitCarriesObsyncsOwnIdentity(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "written by a human, committed by obsync\n")

	env.wake()

	if got, want := env.remoteAuthor(), "obsync <obsync@obsync.invalid>"; got != want {
		t.Errorf("the commit was authored by %s, want %s (§8)", got, want)
	}
}

// The vault's own .git/config outranks obsync's private global, and that is a
// deliberate escape hatch rather than an accident of precedence (§1). It is
// also the only way to see from outside that obsync's configuration is a
// GIT_CONFIG_GLOBAL at all: a -c on the command line would win here, and does
// not.
func TestTheVaultsOwnConfigOutranksObsyncsPrivateOne(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.mustGit(env.vault, "config", "user.name", "A Human")
	env.mustGit(env.vault, "config", "user.email", "human@example.invalid")
	env.writeNote("Daily/2026-08-24.md", "mine\n")

	env.wake()

	if got, want := env.remoteAuthor(), "A Human <human@example.invalid>"; got != want {
		t.Errorf("the commit was authored by %s, want %s — the vault's own config outranks "+
			"obsync's private global (§1)", got, want)
	}
}

// The repo's config holds the human's identity and their remote, and obsync
// only ever reads it — that read is gate 5 (§8). Everything obsync writes lives
// in namespaces it declared, which is what makes removing it a deletion rather
// than an untangling (§10).
func TestObsyncNeverWritesTheVaultsGitConfig(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	configPath := filepath.Join(env.vault, ".git", "config")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the vault's .git/config: %v", err)
	}
	env.writeNote("Daily/2026-08-24.md", "mine\n")

	env.wake()
	env.stop()

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the vault's .git/config: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("obsync rewrote the vault's .git/config.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Healthy is quiet: docker logs --since 1h is empty exactly when nothing is
// wrong, so a log line on a run that changed nothing is a defect rather than
// reassurance (§9).
func TestARunThatChangedNothingSaysNothingAndCommitsNothing(t *testing.T) {
	t.Parallel()

	env := newVault(t)

	env.wake()

	if said := env.said(); said != "" {
		t.Errorf("obsync said %q on a run that changed nothing, want silence (§9)", said)
	}
	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a clean tree produces no commit", got, want)
	}
}

// A human runs git in their own vault, and obsync has to be right about the
// index rather than about the tree: a change staged and then taken back leaves
// a dirty-looking vault whose commit would be empty. Committing it anyway puts
// a commit in a human's history that says nothing happened, and git refuses to
// write one, which would make it a failed run as well.
func TestAChangeThatCancelsItselfOutProducesNoCommitAndNoFailure(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "second thoughts\n")
	env.mustGit(env.vault, "add", "-A")
	env.deleteNote("Daily/2026-08-24.md")

	env.wake()

	if got, want := env.commitsOn(env.vault), "1"; got != want {
		t.Errorf("the vault holds %s commits, want %s — the index matches HEAD, so there is nothing "+
			"to commit", got, want)
	}
	if said := env.said(); said != "" {
		t.Errorf("obsync said %q about a vault whose changes cancelled out, want silence (§9)", said)
	}
}

// A second wake-up on a tree obsync has already captured is a self-triggered
// wake finding a clean tree, which is exactly what obsync's own writes are
// meant to cost: one git status and nothing else (§4).
func TestASecondWakeUpOverTheSameChangeCommitsItOnce(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "once\n")

	env.wake()
	env.wake()

	if got, want := env.commitsOn(env.remote), "2"; got != want {
		t.Errorf("the remote holds %s commits, want %s — one sync run produces at most one commit (§2)", got, want)
	}
}

// A vault obsync attached to rather than cloned has no remote-tracking ref
// until obsync pushes one, so "is there anything to push" cannot be answered by
// comparing against a ref that is not there. A branch obsync has never pushed
// has everything to push.
func TestAVaultThatHasNeverPushedPushesWhatItCommitted(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.mustGit(env.vault, "update-ref", "-d", "refs/remotes/"+config.RemoteName+"/main")
	env.writeNote("Daily/2026-08-24.md", "the first push\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the first push\n" {
		t.Errorf("the remote holds %q, want the note a vault with no remote-tracking ref committed", got)
	}
}

// The local half cannot fail for network reasons, so a remote that will not
// take the push costs obsync nothing but latency: the vault keeps being
// captured, and the commit is there to push when the remote is fixed (§2).
func TestAPushTheRemoteDeclinesStillLeavesTheCommitInTheVault(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "captured anyway\n")

	env.wake()

	if got, want := env.commitsOn(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — the local half keeps committing (§2)", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits, want %s — the push was declined", got, want)
	}
	if said := env.said(); !strings.Contains(said, "level=ERROR") {
		t.Errorf("obsync said %q about a push the remote declined, want it reported (§9)", said)
	}
}

// obsync never exits on a sync failure, so the run after a failed one is an
// ordinary run: the commit is still there, still unpushed, and goes as soon as
// the remote takes it (§2).
//
// The run that takes it is the next tick past the 60s the failed push bought
// itself, because only the network half backs off and the tick is what retries
// it — a rate limit on the loop obsync already turns rather than a schedule of
// its own.
func TestTheNextRunPushesWhatTheLastOneCouldNot(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.installHook("pre-receive", "#!/bin/sh\nexit 1\n")
	env.writeNote("Daily/2026-08-24.md", "eventually\n")

	env.turn()
	env.awaitIdle()
	env.removeHook("pre-receive")
	env.advance(70 * time.Second)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "eventually\n" {
		t.Errorf("the remote holds %q, want the note the earlier run could not push", got)
	}
}

// Timeouts are asymmetric, and the asymmetry is the whole rule: a network git
// is timed out at 120s, and a local one is never timed out at all, because
// killing a local command halfway is how this design manufactures the one state
// it cannot recover from (§1).
//
// Measured: a run that commits, reconciles and pushes drives eighteen git
// commands — five of them writing the private git config, of which one is the
// credential isolation's forced askpass — and takes out exactly two deadlines,
// one for each of the two that talk to the remote. The credential helper is not
// among them: it is pinned per invocation rather than written down
// (internal/git/isolation.go).
//
// The loop waits on the same clock for its own cadence, and those waits are not
// timeouts: nothing is killed when one expires. So the assertion is both halves
// — one git was timed out, and every other wait obsync took out was it waiting
// for its next run.
func TestOnlyTheNetworkGitIsEverTimedOut(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "one commit, one push\n")

	env.wake()
	env.stop()

	if got, want := env.clock.networkDeadlinesTaken(), 2; got != want {
		t.Errorf("obsync timed out %d gits in a run that committed, reconciled and pushed, want "+
			"%d — the fetch and the push, and no local command (§1)", got, want)
	}
	for _, waited := range env.clock.waitsTaken() {
		if waited == networkDeadline {
			continue
		}
		if waited < tick-tickJitter || waited > tick+tickJitter {
			t.Errorf("obsync waited %s, which is neither the network deadline nor a tick; the only "+
				"git obsync times out is the network one (§1)", waited)
		}
	}
}

// A hung network git is converted into a countable failure at 120s, and the
// kill signals the process group rather than the process: git-remote-https and
// ssh — here a hook holding the connection open — die with it (§1).
func TestAHungNetworkGitIsKilledAtTheDeadlineWithItsWholeProcessGroup(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	// The hook backgrounds its sleep and reports that child's pid, so what the
	// test then looks for is a process obsync never started and could only have
	// reached through the group.
	env.installHook("pre-receive", "#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n")
	env.writeNote("Daily/2026-08-24.md", "never arrives\n")

	// Turning in the background, because this test has to act while the run is
	// still in flight: the push is hung, and nothing else can move until the
	// clock does.
	env.turn()
	held := awaitPid(t, pidFile)
	env.clock.awaitDeadline(t)
	env.clock.advanceToNextDeadline(t)
	env.stop()

	if got, want := env.clock.elapsed(), 120*time.Second; got != want {
		t.Errorf("obsync gave the network git %s, want %s (§1)", got, want)
	}
	assertProcessGone(t, held)
	if got, want := env.commitsOn(env.vault), "2"; got != want {
		t.Errorf("the vault holds %s commits, want %s — a killed push does not undo a commit", got, want)
	}
	if got, want := env.commitsOn(env.remote), "1"; got != want {
		t.Errorf("the remote holds %s commits, want %s — nothing arrived", got, want)
	}
}

// awaitPid reads the pid a hook wrote once it exists. The wait is for a file to
// appear, which is the one clock a test cannot fake; it is bounded so that a
// hook that never ran fails here rather than hanging the suite.
func awaitPid(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the pre-receive hook wrote no pid to %s within 30s", path)
	return 0
}

// assertProcessGone reports whether a process obsync killed is really gone. A
// process that has been killed but not yet reaped is a zombie, which is dead
// for every purpose this test has, so both count.
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return
		}
		// The state letter is the field after the comm field, which is itself
		// parenthesised and may contain spaces.
		if _, after, found := strings.Cut(string(stat), ") "); found && strings.HasPrefix(after, "Z") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("the process the hook started (pid %d) was still running 30s after obsync killed the "+
		"push, want the whole process group signalled (§1)", pid)
}

// The watcher's only role is to wake the loop sooner than the next tick would;
// it never says what changed, and every run asks git that instead (§2). This is
// the loop turning as it does in production: a wake-up arrives on the channel,
// the vault goes quiet, one run happens, and the channel is not looked at again
// until it is over.
func TestAWatcherWakeUpDrivesASyncRun(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.turn()
	env.awaitIdle()
	env.writeNote("Daily/2026-08-24.md", "woken by the watcher\n")

	env.watcherWake()
	env.advance(quietWindow)

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "woken by the watcher\n" {
		t.Errorf("the remote holds %q, want the note the wake-up was about", got)
	}
}
