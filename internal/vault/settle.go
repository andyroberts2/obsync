package vault

import (
	"os"
	"path/filepath"
	"time"

	"github.com/andyroberts2/obsync/internal/clock"
)

// settleInterval is the gap between the settle guard's two samples (§6).
//
// One second, because it is more than 3× ignis's own 300ms stability
// threshold, and because it costs 1s against a 10s quiet window. It covers the
// whole torn window by construction rather than by estimate: a non-atomic
// writeFile is open(O_TRUNC) → write → close, and every one of those steps
// bumps mtime, including a slow multi-second write on a FUSE mount.
//
// It is not configurable, and that is the rule rather than an omission. The
// quiet window is taste and the max-wait cap may waive it; this is about valid
// bytes, nothing waives it, and a knob here would be a waiver with extra steps
// (§8).
const settleInterval = time.Second

// sample is the whole of what the settle guard reads of one path: whether it is
// there, how big it is, and when it last changed.
//
// Two samples, never `now - mtime`. Vaults sit on NFS, SMB and rclone mounts
// where mtime comes from the server's clock, and a path whose mtime is skewed
// into the future would read as unsettled *for ever* under a freshness test —
// which, with nothing able to waive this guard, is a note that silently never
// commits. Two samples are purely relative, and are therefore a conclusive fact
// rather than a judgement.
//
// No zero-byte and no suspicious-shrink heuristics either: stability already
// subsumes them, and emptying a note is a legitimate thing a human does.
type sample struct {
	present  bool
	size     int64
	modified time.Time
}

// sampleOf reads one path. A path that is not there samples as absent, which is
// what a deletion looks like and is settled as soon as it stops moving, exactly
// like everything else.
//
// Every Lstat error samples as absent, not only ErrNotExist, and that is safe
// here for a reason worth stating: obsync runs as the vault's own UID (§8), so
// a path git has just reported and obsync cannot stat is one something has
// since moved rather than one obsync may not read — git would not have listed
// a path it could not reach either. The errors that are left, an EIO on a
// flaky mount among them, reach git next and fail the command loudly rather
// than committing bytes nobody read. Treating them as unsettled instead would
// hand the write side a path it could abandon on, silently, for ever.
func sampleOf(root, relative string) sample {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return sample{}
	}
	return sample{present: true, size: info.Size(), modified: info.ModTime()}
}

func (s sample) same(other sample) bool {
	return s.present == other.present && s.size == other.size && s.modified.Equal(other.modified)
}

// acrossTheSettleInterval samples every path, spends the settle interval, and
// samples them again — one wait for the run rather than one per path, because
// the judgement is per-path and the wait is not: a vault where a bulk import
// changed a thousand notes would otherwise take seventeen minutes to look at
// them.
//
// There is no poll-until-stable loop here, deliberately: it can spin unboundedly
// on a hot file inside a serialized loop, and hot files already have an answer
// — they are excluded, and the next run looks again.
//
// The wait is Sleep rather than After because obsync is not waiting *for*
// anything: nothing that could arrive in the meantime would change what it does
// next (internal/clock).
func acrossTheSettleInterval(clk clock.Clock, root string, paths []string) (before, after map[string]sample) {
	before = make(map[string]sample, len(paths))
	after = make(map[string]sample, len(paths))
	if len(paths) == 0 {
		// No candidates, no wait. A quiet vault costs nothing.
		return before, after
	}

	for _, relative := range paths {
		before[relative] = sampleOf(root, relative)
	}
	clk.Sleep(settleInterval)
	for _, relative := range paths {
		after[relative] = sampleOf(root, relative)
	}
	return before, after
}

// Unsettled is the settle guard applied to a set of paths obsync is about to
// overwrite: the first unsettled path among them, or "" when none of them is
// (§6's write side).
//
// The write side is all-or-nothing: if any path the incoming change touches is
// unsettled, the apply does not happen and the run aborts. Skipping the path
// instead would be actively harmful, because a partial apply leaves the vault
// holding a tree obsync never computed, which write-verify then turns into a
// full freeze — the guard would manufacture the worst outcome in the design.
// Applying anyway silently eats keystrokes, and write-verify would *not* catch
// it: obsync wrote exactly what it intended, and it is the user's write that is
// lost. Aborting costs nothing, because the merge is computed out of tree and
// recomputing it is cheap.
//
// So the read side excludes and the write side aborts, and the asymmetry is
// principled: a partial commit is a valid state, and a partial apply is not.
//
// The caller scopes this to the paths the change touches and never to the whole
// tree — that scoping is load-bearing, because checking everything would let one
// continuously-edited note block every incoming change indefinitely.
func Unsettled(clk clock.Clock, root string, paths []string) string {
	before, after := acrossTheSettleInterval(clk, root, paths)
	for _, relative := range paths {
		if !before[relative].same(after[relative]) {
			return relative
		}
	}
	return ""
}
