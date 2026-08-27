package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andyroberts2/obsync/internal/git"
)

// Sizing (#44), the one thread #21 left open as a measurement rather than a
// decision: three costs known in kind and unknown in size, none of which could
// be settled before there was code to run.
//
// The two that grow with the vault are benchmarked here rather than reasoned
// about, and the numbers they produced are recorded in
// docs/research/sizing.md, which is what the README's sizing section stands on.
// This file is the method: a reader who does not believe a number in that
// report re-runs the benchmark that produced it, on their own hardware and at
// either point of the git matrix.
//
//	go test -run '^$' -bench BenchmarkStatusCostPerRun       -benchtime=100x -count=3
//	go test -run '^$' -bench BenchmarkMergeCostPerDivergence -benchtime=10x  -count=2
//
// -benchtime in iterations rather than seconds because a vault of fifty
// thousand notes costs more to build than to measure, and the question is what
// one run costs, not how many runs fit in a second.
//
// These are benchmarks and they assert nothing. The one assertion in this file
// is the third cost, which is not a benchmark at all: disk doubling is a fact
// about one merge rather than a rate, so it is a test.
//
// Seam 1 throughout, and through the same harness every test here uses: a real
// vault directory, a real bare remote over file://, real git underneath, and
// the injected clock. Nothing about the measurement is simulated except time,
// and time is what the fake clock removes — the settle interval is serviced
// instantly, so what these numbers carry is obsync's own work rather than the
// one second §6 spends on purpose.

// vaultSizes is what "representative" means for an Obsidian vault, and each
// point is there for a reason rather than as a power of ten.
//
//	1,000  — a vault a year old. The size almost every deployment is.
//	10,000 — a vault someone has kept for a decade, or imported into.
//	50,000 — past what this audience has, deliberately: a cost that is still
//	         invisible here is invisible, and one that is not is a number the
//	         sizing section has to say out loud.
var vaultSizes = []int{1_000, 10_000, 50_000}

// BenchmarkStatusCostPerRun is the first cost: every sync run asks git what
// changed, at up to one run per tick, and the answer is O(the vault) rather
// than O(what moved).
//
// It measures Changed rather than a whole run, because a whole run also fetches
// and the fetch here crosses a file:// remote on the same disk, which is not a
// cost any deployment has. What is left is the command every run pays whatever
// else it does.
func BenchmarkStatusCostPerRun(b *testing.B) {
	for _, notes := range vaultSizes {
		b.Run(fmt.Sprintf("%d-notes", notes), func(b *testing.B) {
			_, repo := vaultOf(b, notes)

			for b.Loop() {
				if _, err := repo.Changed(); err != nil {
					b.Fatalf("asking git what changed: %v", err)
				}
			}
		})
	}
}

// BenchmarkMergeCostPerDivergence is the second cost: the merge is computed out
// of tree on every divergence (§4), and a divergence is the designed-for case
// rather than an anomaly, so whatever it costs is a cost the vault pays
// routinely.
//
// Each iteration is one real divergence with one conflicted path — both sides
// editing the same daily note, which is the shape that reaches every expensive
// part of §4: merge-tree over both trees, obsync's substitution, a conflict
// copy, commit-tree, and reset --keep applying it to the working tree. Building
// that divergence is outside the timer; what is timed is Reconcile, which is
// the fetch, the classification and the merge.
func BenchmarkMergeCostPerDivergence(b *testing.B) {
	for _, notes := range vaultSizes {
		b.Run(fmt.Sprintf("%d-notes", notes), func(b *testing.B) {
			env, repo := vaultOf(b, notes)

			round := 0
			for b.Loop() {
				b.StopTimer()
				round++
				bothSidesEdit(env, round)
				b.StartTimer()

				got, err := repo.Reconcile(context.Background())
				if err != nil {
					b.Fatalf("reconciling round %d: %v", round, err)
				}
				if got.State != git.Diverged {
					b.Fatalf("round %d stood %q, want %q: this benchmark measures the merge, so a "+
						"round that did not diverge is measuring something else", round, got.State, git.Diverged)
				}
			}
		})
	}
}

// vaultOf is a vault of n notes, committed and pushed, with obsync attached to
// it — the state every sync run after the first one starts from.
func vaultOf(b *testing.B, notes int) (*vaultEnv, *git.Repo) {
	b.Helper()

	env := buildAttachedVault(b, nil)
	for i := range notes {
		// A hundred folders, because a vault is a tree rather than a
		// directory, and git's own untracked scan is per-directory.
		env.writeNote(fmt.Sprintf("Notes/%02d/note-%06d.md", i%100, i), noteBody(i))
	}
	env.mustGit(env.vault, "add", "-A")
	env.mustGit(env.vault, env.asAHuman("commit", "--quiet", "-m", "the vault before obsync")...)
	env.pushVaultTo("main")

	// A second of real time, and then one status through the harness's own git
	// — whose environment does not carry obsync's GIT_OPTIONAL_LOCKS=0 — so
	// that git writes the refreshed index back.
	//
	// Not a tidy-up. A vault of a thousand notes is written and committed
	// inside one second, and git at both matrix points compares an index
	// entry's mtime against the index's own timestamp at one-second
	// granularity: an entry whose mtime is not strictly older is *racily
	// clean*, so git re-reads the file rather than trusting the stat. git
	// normally clears that by writing the refreshed index back, and
	// GIT_OPTIONAL_LOCKS=0 (§1) is exactly the pin that forbids obsync doing
	// so — so a vault built in a burst pays a whole-vault re-read on every
	// single run, for ever. Measured at a thousand notes: 9.9ms an op against
	// 5.3ms, which is a thousand notes costing what ten thousand cost.
	//
	// A real vault is never in that state for long, because only the paths
	// written in the same second as the last index write are racy and obsync
	// writes the index on every run that commits. So the second is waited out
	// and the index refreshed once, which puts the vault in the state it
	// actually syncs in. Recorded in docs/research/sizing.md rather than left
	// here to be rediscovered.
	//
	// This is the one sleep in this file, and it is the sanctioned kind: it
	// ages a file against the clock obsync cannot fake, rather than waiting
	// for obsync to do something.
	time.Sleep(1100 * time.Millisecond)
	env.mustGit(env.vault, "status", "--porcelain=v2", "-z", "-uall")

	repo, err := git.Bootstrap(context.Background(), env.cfg, env.logger, env.clock)
	if err != nil {
		b.Fatalf("attaching obsync to a vault of %d notes: %v", notes, err)
	}
	b.Cleanup(func() {
		if err := repo.Close(); err != nil {
			b.Errorf("closing the repo: %v", err)
		}
	})
	return env, repo
}

// noteBody is a note the size a note actually is. The measurement is about how
// many paths git walks rather than how many bytes it hashes, but a vault of
// empty files would be a vault of empty files.
func noteBody(i int) string {
	var body strings.Builder
	fmt.Fprintf(&body, "# Note %d\n\n", i)
	for line := range 20 {
		fmt.Fprintf(&body, "Line %d of a note that is about the length a note is.\n", line)
	}
	return body.String()
}

// bothSidesEdit is one divergence: the same daily note written on the laptop
// and in the vault, which is user story 10 and the row of §4's table every
// other row is a variation on.
func bothSidesEdit(env *vaultEnv, round int) {
	path := fmt.Sprintf("Daily/2026-08-%02d.md", round%28+1)

	env.remoteCommit(path, fmt.Sprintf("written on the laptop, round %d\n", round))
	env.writeNote(path, fmt.Sprintf("written in the vault, round %d\n", round))
	env.mustGit(env.vault, "add", "-A")
	env.mustGit(env.vault, env.asAHuman("commit", "--quiet", "-m", "written in the vault")...)
}

// attachmentSize is large enough that doubling it is a fact about the volume
// rather than a rounding error, and small enough that the suite stays a suite.
const attachmentSize = 4 << 20

// The third cost, and the reason it is a test rather than a benchmark: keeping
// both sides of a conflicted attachment costs the *remote* nothing and doubles
// the file on the *vault volume*, and that asymmetry is a claim with an exact
// mechanism rather than a number that varies with the machine.
//
// The mechanism is the one #19 measured from the remote's side: the copy is the
// losing version, which is a blob the fetch already brought down, so committing
// it at a second path adds a tree entry and no bytes. The object store holds
// those bytes once. The working tree holds them twice, because a working tree
// is files.
//
// docs/research/sizing.md states the bound this puts on disk headroom; this is
// what stops that statement going quietly false.
func TestKeepingBothSidesOfAnAttachmentDoublesItInTheVaultAndNotInTheObjectStore(t *testing.T) {
	t.Parallel()

	env := newVault(t)
	attachment := "Attachments/scan.png"
	env.vaultAlreadyTracks(attachment)

	onTheLaptop, inTheVault := randomBytes(t), randomBytes(t)
	env.remoteCommit(attachment, string(onTheLaptop))
	env.writeNote(attachment, string(inTheVault))

	env.wake()

	beside := "Attachments/scan" + conflictAt + ".png"
	if got := env.vaultFile(attachment); got != string(inTheVault) {
		t.Errorf("the vault holds %d bytes at the canonical path, want the %d it was written with",
			len(got), len(inTheVault))
	}
	if got := env.vaultFile(beside); got != string(onTheLaptop) {
		t.Errorf("the conflict copy holds %d bytes, want the laptop's %d, byte for byte",
			len(got), len(onTheLaptop))
	}

	// The whole of the vault-side cost: two files of the attachment's size
	// where the vault previously carried one. Read off the volume rather than
	// asserted from the two reads above, because the bytes on disk are what a
	// volume is sized for.
	if got, want := workingTreeBytes(t, env.vault), int64(2*attachmentSize); got < want {
		t.Errorf("the vault's working tree carries %d bytes, want at least %d — keeping both sides "+
			"of a conflicted attachment writes the file twice, which is the deployment fact "+
			"docs/research/sizing.md is written from", got, want)
	}

	// And the whole of the reason it costs the remote nothing: the copy is not
	// a second blob, it is the blob the fetch already brought down, named at a
	// second path. One object, two entries. The merge commit's second parent is
	// where the fetch left the remote's tip, so it is where the losing bytes
	// are asked for — the remote-tracking ref has since moved on to the merge
	// obsync pushed, where that path holds the vault's version.
	if got, want := env.blobAt("refs/heads/main:"+beside), env.blobAt("refs/heads/main^2:"+attachment); got != want {
		t.Errorf("the conflict copy points at blob %s and the remote's losing version is %s; they "+
			"are meant to be one object, which is why the copy costs the object store nothing and "+
			"is exempt from the size ceiling at any size (§4)", got, want)
	}
}

// randomBytes is an attachment's worth of bytes that do not compress, so that
// what the volume carries is what the file says it is.
func randomBytes(t *testing.T) []byte {
	t.Helper()

	made := make([]byte, attachmentSize)
	if _, err := rand.Read(made); err != nil {
		t.Fatalf("making an attachment: %v", err)
	}
	return made
}

// workingTreeBytes is what the vault costs the volume, .git excluded: the
// object store is the other half of the disk question and it is answered by
// oid identity above rather than by size.
func workingTreeBytes(t *testing.T, vault string) int64 {
	t.Helper()

	var total int64
	err := filepath.WalkDir(vault, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("measuring the vault: %v", err)
	}
	return total
}

// blobAt is the object name git holds at a revision and path, which is how a
// test says "these are the same bytes" without reading the attachment twice.
func (e *vaultEnv) blobAt(revision string) string {
	e.t.Helper()

	return strings.TrimSpace(e.mustGit(e.vault, "rev-parse", revision))
}
