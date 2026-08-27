package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/vault"
)

// What obsync tracks (§5), at seam 1: a real vault, a real bare remote, and
// real git deciding every precedence question rather than obsync's beliefs
// about it. Every assertion below is about which bytes reached the remote,
// which files are still on the disk, and what obsync said — never about which
// git ran.

// The floor goes into the repo's own exclude file rather than the vault's
// .gitignore, so that it cannot conflict, cannot be clobbered by an external
// push, and cannot be disabled by editing a note. It is an owned path, so it is
// rewritten wholesale at every startup — a human who edited it finds obsync's
// floor back, and their own rules go where their own rules go.
func TestTheIgnoreFloorIsWrittenToTheReposExcludeFileAtEveryStartup(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.wake()

	if got := floorIn(env.excludeFile()); !slices.Equal(got, vault.IgnoreFloor) {
		t.Errorf("the repo's exclude file carries %v, want the ignore floor %v (§5)",
			got, vault.IgnoreFloor)
	}

	// Idempotent, and wholesale: whatever the file held at startup, what it
	// holds afterwards is the floor and the marker that says whose file it is.
	before := env.excludeFile()
	if err := os.WriteFile(filepath.Join(env.vault, ".git", "info", "exclude"),
		[]byte("# someone edited this\nNotes/\n"), 0o644); err != nil {
		t.Fatalf("editing the exclude file: %v", err)
	}
	env.restart()
	env.wake()

	if got := env.excludeFile(); got != before {
		t.Errorf("after a restart the exclude file holds:\n%s\nwant the same floor it held "+
			"before:\n%s\nobsync rewrites it wholesale at every startup (§5, §10)", got, before)
	}
	if strings.Contains(env.excludeFile(), "Notes/") {
		t.Error("the exclude file still carries an entry obsync did not put there; it is an owned " +
			"path, rewritten wholesale rather than merged, and the vault's own .gitignore is where " +
			"a human's rules live (§10)")
	}

	// It arrives through a staging directory, and a temporary file's 0600 is
	// the one thing that default gets wrong for a destination that is not one:
	// git creates this file 0644, and a floor only obsync's own UID can read
	// is a floor that applies to obsync and to nobody else running git in the
	// same vault.
	info, err := os.Stat(filepath.Join(env.vault, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("reading the repo's exclude file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("the repo's exclude file is mode %v, want 0644 — what git itself creates it as; "+
			"obsync rewrites this file's contents and never narrows who may read it (§10)", got)
	}
}

// The floor exists so that a fresh clone of the vault repo is the same vault
// rather than a folder of markdown *and* a week of somebody else's window
// layout. Everything named here is a floor entry in a shape a real vault
// produces it in.
func TestTheWorkspaceChurnAndCruftTheFloorCoversNeverReachTheRemote(t *testing.T) {
	t.Parallel()

	covered := []string{
		".obsidian/workspace.json",
		".obsidian/workspace-mobile.json",
		".obsidian/workspaces.json",
		".obsidian/plugins/dataview/data.json",
		".trash/a note I deleted.md",
		".DS_Store",
		"Attachments/.DS_Store",
		"Thumbs.db",
		".vscode/settings.json",
		".idea/workspace.xml",
		".obsidian-git-data",
		"obsync-attention.md",
	}

	env := newVault(t)
	for _, path := range covered {
		env.writeNote(path, "churn\n")
	}
	env.writeNote("Daily/2026-08-24.md", "the note I actually wrote\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I actually wrote\n" {
		t.Errorf("the remote holds %q at the note's path, want the note; the floor takes cruft out "+
			"of a commit and leaves everything else in (§5)", got)
	}
	for _, path := range covered {
		if env.remoteHolds(path) {
			t.Errorf("the remote holds %q, which obsync's ignore floor covers (§5)", path)
		}
	}
}

// The other half of the same rule, and the reason the floor is eleven entries
// rather than ".obsidian/": a clone of the vault repo has to arrive with the
// same theme, hotkeys and plugin set, which is what makes it the same vault.
func TestEverythingElseUnderObsidianIsTracked(t *testing.T) {
	t.Parallel()

	tracked := []string{
		".obsidian/app.json",
		".obsidian/appearance.json",
		".obsidian/core-plugins.json",
		".obsidian/community-plugins.json",
		".obsidian/plugins/dataview/main.js",
		".obsidian/plugins/dataview/manifest.json",
		".obsidian/plugins/dataview/styles.css",
		".obsidian/themes/Minimal/theme.css",
	}

	env := newVault(t)
	for _, path := range tracked {
		env.writeNote(path, "the vault's own configuration\n")
	}

	env.wake()

	for _, path := range tracked {
		if !env.remoteHolds(path) {
			t.Errorf("the remote holds no %q; everything under .obsidian/ that is not workspace "+
				"state or plugin settings is tracked, so a fresh clone is the same vault (§5)", path)
		}
	}
}

// The floor is a default, not a rule. Precedence is git's — .gitignore first,
// then the repo's exclude file — so a vault that re-includes a floor entry gets
// it tracked, and this asserts against real git rather than against obsync's
// belief about the order.
//
// A tool that cannot be overruled becomes a tool people work around.
func TestAVaultGitignoreOverridesTheIgnoreFloor(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote(".gitignore", "!.obsidian/workspace.json\n")
	env.writeNote(".obsidian/workspace.json", "the layout I want on every machine\n")

	env.wake()

	if got := env.remoteFile(".obsidian/workspace.json"); got != "the layout I want on every machine\n" {
		t.Errorf("the remote holds %q at the workspace file's path, want the bytes the vault holds: "+
			"the vault's own .gitignore outranks obsync's floor (§5)", got)
	}
}

// The one exception, and the one place obsync cannot be overruled.
//
// The vault's .gitignore re-includes plugin settings, so git reports the file
// as changed and the floor has already lost the argument — the only thing left
// standing between an API key and a repo that may be public is the pathspec
// exclusion on the `git add` itself, which no .gitignore can negate.
//
// Overriding a noisy default is a preference. Committing a credential is
// unrecoverable.
func TestPluginDataIsRefusedByTheAddEvenWhenTheVaultsGitignoreAllowsIt(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote(".gitignore", "!.obsidian/plugins/*/data.json\n")
	env.writeNote(".obsidian/plugins/dataview/data.json", `{"apiKey":"sk-not-in-my-repo"}`+"\n")
	env.writeNote(".obsidian/plugins/dataview/main.js", "the plugin itself\n")

	env.wake()

	if !env.remoteHolds(".obsidian/plugins/dataview/main.js") {
		t.Error("the remote holds no .obsidian/plugins/dataview/main.js; the plugin itself is " +
			"tracked, and only its settings are refused (§5)")
	}
	if env.remoteHolds(".obsidian/plugins/dataview/data.json") {
		t.Error("the remote holds .obsidian/plugins/dataview/data.json, which is where community " +
			"plugins keep API keys: it is excluded on the git add itself, which no .gitignore can " +
			"negate (§5)")
	}
}

// The refused-path list is closed and it is a promise on the declared surface.
// Every entry is named here in a shape a vault could really hold it in, at the
// root and further down, because these entries match by name at any depth
// exactly as the floor's bare-name entries do.
//
// A refusal skips the path. The note written in the same breath still reaches
// the remote, which is the whole difference between a refusal and a freeze.
func TestACredentialShapedFileIsNeverCommittedAndTheRestOfTheVaultKeepsSyncing(t *testing.T) {
	t.Parallel()

	refused := []string{
		".env",
		"Notes/.env.production",
		"id_rsa",
		"Keys/id_dsa",
		"id_ecdsa",
		"id_ed25519",
		"Attachments/server.pem",
		"signing.key",
		"Certs/bundle.p12",
		"Certs/bundle.pfx",
		".netrc",
		".npmrc",
		".pypirc",
		"credentials",
		"Work/aws/credentials",
	}

	env := newVault(t)
	for _, path := range refused {
		env.writeNote(path, "a secret that must never leave this machine\n")
	}
	env.writeNote("Daily/2026-08-24.md", "the note I wrote beside them\n")

	env.wake()

	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I wrote beside them\n" {
		t.Errorf("the remote holds %q at the note's path, want the note: a refusal skips the path "+
			"and never stops the loop (§5)", got)
	}
	for _, path := range refused {
		if env.remoteHolds(path) {
			t.Errorf("the remote holds %q, which is on obsync's refused-path list (§5)", path)
		}
	}
}

// Name-matching only — no content scanning, ever. This is a note-taking vault:
// people write *about* keys, paste example blocks, and keep security runbooks,
// and a refusal is silent by omission, so a false positive is the expensive
// error. Nobody names a note id_rsa.
func TestANoteWrittenAboutAPrivateKeyIsCommittedLikeAnyOtherNote(t *testing.T) {
	t.Parallel()

	const runbook = "# Rotating the deploy key\n\n" +
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n" +
		"-----END OPENSSH PRIVATE KEY-----\n"

	env := newVault(t)
	env.writeNote("Notes/Rotating the deploy key.md", runbook)

	env.wake()

	if got := env.remoteFile("Notes/Rotating the deploy key.md"); got != runbook {
		t.Errorf("the remote holds %q, want the runbook: obsync matches names and never scans "+
			"content, because a note about a key is a note (§5)", got)
	}
}

// The size ceiling is the one configured value in this area, and it is a fact
// about the remote rather than a taste. The boundary is asserted in both
// directions, because "over the ceiling" is the whole of the rule and a file
// exactly at it is not over it.
func TestAFileOverTheSizeCeilingIsRefusedAndTheRestOfTheVaultKeepsSyncing(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeAttachment("Attachments/the video I dragged in.mp4", 4096)
	env.writeAttachment("Attachments/exactly at the ceiling.png", 1024)
	env.writeNote("Daily/2026-08-24.md", "and everything else still syncs\n")

	env.wake()

	if !env.remoteHolds("Daily/2026-08-24.md") {
		t.Error("the remote holds no Daily/2026-08-24.md: one oversized attachment must not stop " +
			"the notes reaching the remote, because a stopped sidecar is the failure that gets a " +
			"sync tool uninstalled (§5)")
	}
	if !env.remoteHolds("Attachments/exactly at the ceiling.png") {
		t.Error("the remote holds no Attachments/exactly at the ceiling.png: the ceiling refuses a " +
			"file *over* it, and a file exactly at it is not over it (§5)")
	}
	if env.remoteHolds("Attachments/the video I dragged in.mp4") {
		t.Error("the remote holds Attachments/the video I dragged in.mp4, which is over the " +
			"configured size ceiling (§5)")
	}
}

// A refusal never freezes the loop, and it says so once.
//
// The path is refused on every run for as long as it sits in the vault — git
// reports it as changed each time — so a WARN per run would be one an hour, for
// ever, about a state that has not changed. Once per path on transition; the
// standing signal is the attention note (#38).
func TestARefusedPathIsWarnedAboutOncePerPathAndNeverStopsTheLoop(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Attachments/leaked.pem", "a certificate\n")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	// obsync kept syncing everything else while the refused path sat there,
	// which is the property that matters more than the log line.
	env.writeNote("Daily/2026-08-24.md", "written while a path was refused\n")
	env.advance(70 * time.Second)

	if !env.stillTurning() {
		t.Fatal("obsync's sync loop stopped over a refused path; a refusal skips the path and " +
			"never freezes the loop (§5)")
	}
	if got, ok := env.remoteContentYet("Daily/2026-08-24.md"); !ok || got != "written while a path was refused\n" {
		t.Errorf("the remote holds %q at the note's path (present: %t), want the note written "+
			"while a path was refused (§5)", got, ok)
	}
	if env.remoteHoldsYet("Attachments/leaked.pem") {
		t.Error("the remote holds Attachments/leaked.pem, so nothing was refused and the count " +
			"below is counting something else (§5)")
	}
	if got := strings.Count(env.saidSoFar(), "Attachments/leaked.pem"); got != 1 {
		t.Errorf("obsync named the refused path %d times across four runs, want once — a WARN "+
			"fires once per path on transition, and the standing signal is the attention note "+
			"(§5, §9). It said:\n%s", got, env.saidSoFar())
	}
}

// "Commit if dirty" is "commit if the committable set is non-empty". A tree
// holding nothing but refused paths is quiet: no commit, no push, and no
// repeated warning.
func TestATreeHoldingNothingButRefusedPathsProducesNoCommit(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	before := env.commitsSoFar(env.remote)
	env.writeNote("Attachments/id_rsa", "a key someone dropped in the vault\n")

	env.wake()

	if got := env.commitsOn(env.remote); got != before {
		t.Errorf("the remote is %s commits along, was %s: a tree holding nothing but refused paths "+
			"is quiet and produces no commit (§5)", got, before)
	}
	if got := env.commitsOn(env.vault); got != before {
		t.Errorf("the vault is %s commits along, was %s: the committable set was empty, so there "+
			"was nothing to commit (§5)", got, before)
	}
}

// A vault whose history already carries workspace state churns forever
// regardless of the floor, because ignore rules only ever affect untracked
// paths. obsync takes that subset out of the index once, in one loudly-messaged
// commit, and leaves every byte on disk.
func TestTheChurnSubsetIsUntrackedOnceAndTheFilesStayOnDisk(t *testing.T) {
	t.Parallel()

	churn := []string{
		".obsidian/workspace.json",
		".obsidian/workspaces.json",
		".trash/a note I deleted.md",
		"Attachments/.DS_Store",
	}

	env := newVault(t)
	env.vaultAlreadyTracks(append(slices.Clone(churn), ".obsidian/appearance.json")...)

	env.turn()
	env.awaitIdle()
	// The one-shot runs at the top of the local half of a run *after* one whose
	// network half left the vault and the remote in step — not at bootstrap.
	env.advance(70 * time.Second)

	for _, path := range churn {
		if env.remoteHoldsYet(path) {
			t.Errorf("the remote still holds %q, which obsync's ignore floor covers: an entry "+
				"already in a vault's history churns forever until it leaves the index (§5)", path)
		}
		if !env.vaultHoldsYet(path) {
			t.Errorf("the vault no longer holds %q on disk; obsync untracks the churn subset with "+
				"`git rm --cached` and never deletes a file you own (§5)", path)
		}
	}
	if !env.remoteHoldsYet(".obsidian/appearance.json") {
		t.Error("the remote no longer holds .obsidian/appearance.json, which is not churn: the " +
			"subset obsync untracks is the ignore floor and nothing else (§5)")
	}

	// Once, ever. A second one of these commits would untrack nothing and say
	// so loudly to every clone that pulled it.
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if got := untrackingCommits(env.remoteSubjects()); got != 1 {
		t.Errorf("the remote carries %d commits that stop tracking the churn subset across four "+
			"runs, want exactly one (§5). obsync said:\n%s", got, env.saidSoFar())
	}
	if got := env.remoteMessage(); !strings.Contains(got, "Every byte is still on disk") {
		t.Errorf("the untracking commit says:\n%s\nwant a message loud enough for someone reading "+
			"git log a year later, including that nothing left the disk (§5)", got)
	}
}

// One-shot means once, and the reason it has to is a human who disagrees.
//
// Someone who puts their workspace file deliberately back in the index has said
// what they want, and obsync taking it out again on the next tick is a fight
// they cannot win by doing the thing they just did. The supported way to say it
// permanently is their own .gitignore, which outranks the floor everywhere
// including here; this is what happens meanwhile.
func TestTheOneShotDoesNotFightAHumanWhoPutsTheFileBack(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks(".obsidian/workspace.json")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)

	if env.vaultTracks(".obsidian/workspace.json") {
		t.Fatalf("the vault still tracks .obsidian/workspace.json, so the one-shot did not run and "+
			"nothing below is being asserted. obsync said:\n%s", env.saidSoFar())
	}

	// The human's own git, in their own vault, saying what they want.
	env.mustGit(env.vault, "add", "-f", "--", ":(literal).obsidian/workspace.json")
	env.mustGit(env.vault, env.asAHuman("commit", "--quiet", "-m", "I want my layout synced")...)

	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if !env.vaultTracks(".obsidian/workspace.json") {
		t.Errorf("obsync untracked .obsidian/workspace.json a second time, after a human put it "+
			"back: the untracking is a one-shot, not a rule obsync enforces every tick (§5). It "+
			"said:\n%s", env.saidSoFar())
	}
	if got := untrackingCommits(env.remoteSubjects()); got != 1 {
		t.Errorf("the remote carries %d commits that stop tracking the churn subset, want exactly "+
			"one (§5)", got)
	}
}

// The one-shot waits for a run whose network half left the vault and the remote
// in step, and this is why: untracking against a stale tip risks doing it twice,
// and while the remote is unreachable obsync should not be making structural
// commits it cannot push.
func TestTheChurnSubsetIsNotUntrackedWhileTheRemoteCannotBeReached(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks(".obsidian/workspace.json")
	away := env.remote + ".unplugged"
	if err := os.Rename(env.remote, away); err != nil {
		t.Fatalf("unplugging the remote: %v", err)
	}

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if !env.vaultTracks(".obsidian/workspace.json") {
		t.Error("obsync untracked the churn subset while the remote was unreachable; that is a " +
			"structural commit it cannot push, against a tip it has not confirmed (§5)")
	}

	if err := os.Rename(away, env.remote); err != nil {
		t.Fatalf("plugging the remote back in: %v", err)
	}
	// One run to reach the remote and one to act on having reached it.
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if env.vaultTracks(".obsidian/workspace.json") {
		t.Errorf("the vault still tracks .obsidian/workspace.json after the remote came back; the "+
			"one-shot is deferred by an unreachable remote, not cancelled by one (§5). obsync "+
			"said:\n%s", env.saidSoFar())
	}
	if env.remoteHoldsYet(".obsidian/workspace.json") {
		t.Error("the remote still holds .obsidian/workspace.json after obsync reached it again (§5)")
	}
}

// The one-shot is bounded by two answers, and this is the other one: git says
// what is ignored — over the vault's own .gitignore first and obsync's exclude
// file second — and obsync says what is its to untrack.
//
// So a human who re-included their workspace file because they want it synced
// is not overruled by the untracking either. The floor being a default rather
// than a rule has to hold for the act that makes it stick, or it holds for
// nothing.
func TestTheChurnSubsetLeavesAlonePathsTheVaultsGitignoreReincludes(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote(".gitignore", "!.obsidian/workspace.json\n")
	env.vaultAlreadyTracks(".obsidian/workspace.json", ".trash/a note I deleted.md")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if env.vaultTracks(".trash/a note I deleted.md") {
		t.Error("the vault still tracks .trash/a note I deleted.md, so the one-shot did not run " +
			"and the assertions below prove nothing (§5)")
	}
	// The commit is what says which paths obsync decided were its to take out,
	// and it is the only place that answer survives: a path untracked and then
	// re-added by the next run's ordinary commit ends up tracked either way,
	// having churned the history of every clone on the way.
	want := "Stop tracking .trash/a note I deleted.md, which obsync's ignore floor covers"
	if got := untrackingSubject(env.remoteSubjects()); got != want {
		t.Errorf("obsync's untracking commit says %q, want %q: the vault's own .gitignore "+
			"re-includes the workspace file, and the floor being a default rather than a rule has "+
			"to hold for the act that makes it stick (§5)", got, want)
	}
	if !env.vaultTracks(".obsidian/workspace.json") {
		t.Error("the vault no longer tracks .obsidian/workspace.json, which its own .gitignore " +
			"re-includes (§5)")
	}
	if !env.remoteHoldsYet(".obsidian/workspace.json") {
		t.Error("the remote no longer holds .obsidian/workspace.json, which the vault's own " +
			".gitignore re-includes (§5)")
	}
}

// Already-tracked plugin settings are the one floor entry obsync leaves alone.
// Untracking them would delete deliberately-synced settings from every other
// clone, and it would not unleak a key the remote's history already holds — so
// the rule is only ever the other half: never be the thing that commits one for
// the first time.
func TestAnAlreadyTrackedPluginDataFileIsLeftAloneAndSaidSoAtStartup(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks(".obsidian/plugins/dataview/data.json", ".obsidian/workspace.json")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if !env.remoteHoldsYet(".obsidian/plugins/dataview/data.json") {
		t.Error("the remote no longer holds .obsidian/plugins/dataview/data.json: untracking it " +
			"would delete deliberately-synced plugin settings from every other clone, and would " +
			"not unleak a key the remote's history already holds (§5)")
	}
	if env.vaultTracks(".obsidian/workspace.json") {
		t.Error("the vault still tracks .obsidian/workspace.json, so the one-shot did not run and " +
			"the assertion above proves nothing (§5)")
	}
	if got := env.saidSoFar(); !strings.Contains(got, ".obsidian/plugins/dataview/data.json") {
		t.Errorf("obsync said nothing about the plugin data file this vault already tracks; it "+
			"logs loudly at startup when it finds one (§5, §9). It said:\n%s", got)
	}
}

// The vault's .gitignore is content, and it is the user's. It is also the one
// mechanism that outranks obsync's floor, so obsync writing it would be obsync
// editing the only thing that can overrule obsync.
func TestObsyncNeverWritesTheVaultsGitignore(t *testing.T) {
	t.Parallel()

	const theirs = "# mine\nArchive/\n!.obsidian/workspace.json\n"

	env := newVault(t)
	env.writeNote(".gitignore", theirs)
	env.writeNote("Daily/2026-08-24.md", "a run that does plenty of work\n")

	env.wake()

	if got := env.vaultFile(".gitignore"); got != theirs {
		t.Errorf("the vault's .gitignore holds %q, want the bytes the human wrote: obsync never "+
			"writes that file, and it is what outranks obsync's own floor (§5)", got)
	}
}

// The other half of the same promise: a vault that had no .gitignore does not
// acquire one. obsync's floor lives in the repo's exclude file, which is never
// committed and cannot be clobbered by a push.
func TestAVaultWithNoGitignoreNeverGainsOne(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote("Daily/2026-08-24.md", "a run that does plenty of work\n")

	env.wake()

	if env.vaultHoldsYet(".gitignore") {
		t.Error("obsync created a .gitignore in the vault; the ignore floor goes in the repo's " +
			"exclude file, which is never committed, cannot conflict and cannot be clobbered by " +
			"an external push (§5)")
	}
	if env.remoteHolds(".git/info/exclude") {
		t.Error("the remote holds .git/info/exclude; the floor is never committed (§5)")
	}
}

// floorIn is the exclude file's entries: everything that is not the marker
// comment obsync writes them under.
func floorIn(exclude string) []string {
	var entries []string
	for _, line := range strings.Split(exclude, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			entries = append(entries, line)
		}
	}
	return entries
}

// untrackingCommits counts the churn-subset one-shot's commits among a branch's
// subjects, and untrackingSubject is the one of them when there is exactly one.
// They match on what the subject promises rather than on the whole of it,
// because the count of files in it is not always what is being asserted.
func untrackingCommits(subjects []string) int {
	return len(untrackingSubjects(subjects))
}

func untrackingSubject(subjects []string) string {
	found := untrackingSubjects(subjects)
	if len(found) != 1 {
		return "no single untracking commit, found " + strconv.Itoa(len(found))
	}
	return found[0]
}

func untrackingSubjects(subjects []string) []string {
	var found []string
	for _, subject := range subjects {
		if strings.HasPrefix(subject, "Stop tracking ") {
			found = append(found, subject)
		}
	}
	return found
}

// The recipe every git user knows for "stop tracking this": ignore it, then
// take it out of the index. What it leaves behind is a staged deletion of a
// path git now ignores — and git refuses an `add` that names an ignored path,
// refusing the *whole* add rather than that one pathspec.
//
// So naming it on the add obsync builds from what status reported takes out
// every other path in the same run, and the record survives until something
// commits it — which is now never. That is obsync stopping for good over a
// thing a human is told to do, which is the failure a sync tool gets
// uninstalled over (§5, §7).
//
// obsync's own churn one-shot reaches the same state whenever its `git rm
// --cached` lands and the commit after it does not.
func TestAPathAHumanUntrackedAndIgnoredNeverStopsTheVaultSyncing(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Archive/old note.md")

	// The human's own git, in their own vault, saying what they want.
	env.writeNote(".gitignore", "Archive/\n")
	env.mustGit(env.vault, "rm", "--cached", "--quiet", "--", ":(literal)Archive/old note.md")

	env.writeNote("Daily/2026-08-24.md", "the note I wrote afterwards\n")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if got, ok := env.remoteContentYet("Daily/2026-08-24.md"); !ok || got != "the note I wrote afterwards\n" {
		t.Errorf("the remote holds %q at the note's path (present: %t), want the note written "+
			"after a human untracked an ignored path: one such path must not take the vault's "+
			"notes out of the commit with it (§5). obsync said:\n%s", got, ok, env.saidSoFar())
	}
	if env.remoteHoldsYet("Archive/old note.md") {
		t.Error("the remote still holds Archive/old note.md, which a human took out of the index " +
			"deliberately; obsync commits what the index holds (§5)")
	}
	if !env.vaultHoldsYet("Archive/old note.md") {
		t.Error("the vault no longer holds Archive/old note.md on disk; nothing here deletes a " +
			"file a human owns (§5)")
	}
	if !env.stillTurning() {
		t.Fatal("obsync's sync loop stopped over a path a human untracked (§7)")
	}
}

// The same act with nothing else in the vault to commit, which is the other
// half of it: what the add is given is narrower than what the commit carries,
// so a run whose whole change is one a human already put in the index makes the
// commit anyway rather than going quiet over an empty `git add`.
func TestAChangeAHumanAlreadyStagedIsCommittedWithNothingLeftToAdd(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks("Archive/old note.md")

	// The human's own git: ignore it, commit that, then take it out of the
	// index and leave obsync to make the commit.
	env.writeNote(".gitignore", "Archive/\n")
	env.mustGit(env.vault, "add", "--", ":(literal).gitignore")
	env.mustGit(env.vault, env.asAHuman("commit", "--quiet", "-m", "ignore the archive")...)
	env.mustGit(env.vault, "rm", "--cached", "--quiet", "--", ":(literal)Archive/old note.md")

	env.wake()

	if env.vaultTracks("Archive/old note.md") {
		t.Errorf("the vault still tracks Archive/old note.md; obsync commits what the index holds, "+
			"and the index no longer held it (§2). obsync said:\n%s", env.saidSoFar())
	}
	if env.remoteHolds("Archive/old note.md") {
		t.Error("the remote still holds Archive/old note.md, so obsync never committed the change " +
			"a human had already staged (§2)")
	}
	if !env.vaultHoldsYet("Archive/old note.md") {
		t.Error("the vault no longer holds Archive/old note.md on disk; nothing here deletes a " +
			"file a human owns (§5)")
	}
}

// The one-shot's own way into the same state, and the reason it is worth a row
// of its own: the `git rm --cached` lands and the commit that was meant to
// carry it does not.
//
// What is left is exactly what a human's `git rm --cached` leaves — a staged
// deletion of a path git now ignores — except that obsync made it, once, in the
// vault it is responsible for. It has to be able to finish the job on the next
// run rather than stopping there for good (§5, §7).
func TestTheUntrackingIsFinishedByTheNextRunWhenItsCommitWasRefused(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks(".obsidian/workspace.json")
	env.installVaultHook("pre-commit", "#!/bin/sh\nexit 1\n")

	env.turn()
	env.awaitIdle()
	// One run to reach the remote, and the one after it is the one-shot: the
	// untracking lands in the index and the hook refuses the commit.
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	env.removeVaultHook("pre-commit")

	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if env.vaultTracks(".obsidian/workspace.json") {
		t.Errorf("the vault still tracks .obsidian/workspace.json; a one-shot whose commit was "+
			"refused has to be finished by a later run rather than left half done (§5). obsync "+
			"said:\n%s", env.saidSoFar())
	}
	if env.remoteHoldsYet(".obsidian/workspace.json") {
		t.Errorf("the remote still holds .obsidian/workspace.json, so obsync never got the "+
			"untracking it had already staged into a commit (§5). obsync said:\n%s", env.saidSoFar())
	}
	if !env.vaultHoldsYet(".obsidian/workspace.json") {
		t.Error("the vault no longer holds .obsidian/workspace.json on disk; obsync untracks and " +
			"never deletes (§5)")
	}
	if !env.stillTurning() {
		t.Fatal("obsync's sync loop stopped after a refused commit (§7)")
	}
}

// `git add` is not the only way a path reaches the index, and `git commit`
// records the index rather than what obsync staged.
//
// A human's own `git add -A` in their own vault is muscle memory, and every
// plugin that drives git for them does it — so a refused path can be sitting in
// the index before obsync ever looks. §5 admits no exception: the list never
// enters a commit, whatever the vault's state. The note staged beside it is
// committed like any other, because a refusal skips the path and stops nothing.
func TestARefusedPathAHumanStagedIsTakenBackOutRatherThanCommitted(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.writeNote(".env", "AWS_SECRET_ACCESS_KEY=hunter2\n")
	env.writeNote("Attachments/deploy.pem", "-----BEGIN PRIVATE KEY-----\n")
	env.writeNote("Daily/2026-08-24.md", "the note I wrote beside them\n")
	env.mustGit(env.vault, "add", "-A")

	env.wake()

	if env.remoteHolds(".env") {
		t.Errorf("the remote holds .env, which a human staged and obsync then committed and "+
			"pushed: a refused path never enters a commit, whatever the vault's state, and "+
			"obsync said in the same run that it was not committing it (§5). It said:\n%s",
			env.saidSoFar())
	}
	if env.remoteHolds("Attachments/deploy.pem") {
		t.Error("the remote holds Attachments/deploy.pem, which a human staged (§5)")
	}
	if got := env.remoteFile("Daily/2026-08-24.md"); got != "the note I wrote beside them\n" {
		t.Errorf("the remote holds %q at the note's path, want the note: a refusal skips the path "+
			"and never stops the rest of the vault syncing (§5)", got)
	}

	// Index-only, always: the bytes are exactly where the human left them.
	if got := env.vaultFile(".env"); got != "AWS_SECRET_ACCESS_KEY=hunter2\n" {
		t.Errorf("the vault holds %q at .env, want the bytes the human wrote: obsync takes a "+
			"refused path out of the index and never off the disk (§5)", got)
	}
}

// The same promise for a path that is over the size ceiling rather than named
// on the list, because the refusal layer is one layer and a 200MB attachment
// somebody staged is a push that the remote rejects and a run that keeps
// rejecting.
func TestAnOversizedFileAHumanStagedIsTakenBackOutRatherThanCommitted(t *testing.T) {
	t.Parallel()

	env := newVaultWith(t, "OBSYNC_SIZE_CEILING=1KB")
	env.writeAttachment("Attachments/the video I dragged in.mp4", 4096)
	env.writeNote("Daily/2026-08-24.md", "and everything else still syncs\n")
	env.mustGit(env.vault, "add", "-A")

	env.wake()

	if env.remoteHolds("Attachments/the video I dragged in.mp4") {
		t.Errorf("the remote holds an attachment over the configured size ceiling, which a human "+
			"staged (§5). obsync said:\n%s", env.saidSoFar())
	}
	if !env.remoteHolds("Daily/2026-08-24.md") {
		t.Error("the remote holds no Daily/2026-08-24.md: one oversized attachment must not stop " +
			"the notes reaching the remote (§5)")
	}
	if !env.vaultHoldsYet("Attachments/the video I dragged in.mp4") {
		t.Error("the vault no longer holds the attachment on disk; obsync unstages and never " +
			"deletes (§5)")
	}
}

// The untracking one-shot's commit records the index like every other commit,
// so it passes the refusal layer like every other commit. It is the one commit
// obsync makes that is not built from the committable set, which is exactly why
// it is the one worth checking.
func TestTheUntrackingCommitDoesNotCarryARefusedPathAHumanStaged(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	env.vaultAlreadyTracks(".obsidian/workspace.json")
	env.writeNote(".env", "AWS_SECRET_ACCESS_KEY=hunter2\n")
	env.mustGit(env.vault, "add", "-A")

	env.turn()
	env.awaitIdle()
	env.advance(70 * time.Second)
	env.advance(70 * time.Second)

	if env.vaultTracks(".obsidian/workspace.json") {
		t.Fatalf("the vault still tracks .obsidian/workspace.json, so the one-shot did not run and "+
			"nothing below is being asserted. obsync said:\n%s", env.saidSoFar())
	}
	if env.remoteHoldsYet(".env") {
		t.Errorf("the remote holds .env, carried out by the one commit obsync makes that is not "+
			"built from the committable set (§5). obsync said:\n%s", env.saidSoFar())
	}
	if got := env.vaultFile(".env"); got != "AWS_SECRET_ACCESS_KEY=hunter2\n" {
		t.Errorf("the vault holds %q at .env, want the bytes the human wrote (§5)", got)
	}
}
