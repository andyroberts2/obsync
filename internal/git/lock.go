package git

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockName is the advisory lock that keeps one vault to one obsync, and it is
// an owned path obsync declared (§10, docs/interface.md).
//
// It lives in `.git/` rather than in the vault, which wins twice: it stays
// outside the vault watcher, so taking it wakes nothing, and it stays outside
// the ignore floor's business, so there is no entry to write and no chance of
// committing it.
const lockName = "obsync.lock"

// lockTheVault is gate 8: the advisory flock on `.git/obsync.lock`.
//
// An advisory flock rather than a PID file, and that is the whole decision: the
// lock is a property of an open file description, so it dies with the process
// holding it. A crash therefore leaves nothing to clean up, where a PID file
// leaves a number a later obsync has to guess about — and guessing wrong either
// refuses a healthy deployment for ever or lets two obsyncs commit to one vault.
//
// It guards against a second obsync. It cannot guard against a *human* running
// git in the vault, and it is not meant to: gate 4 catches that on the next
// run. obsync racing its own previous run needs no lock at all, because the
// single serialized loop makes that structurally impossible (§7).
//
// The file descriptor is held for the process lifetime and released by Close,
// which is the only thing that ever drops the lock: obsync holds the vault for
// as long as it is running, whatever tier it is in, because a frozen obsync is
// still the obsync that owns this vault.
func (r *Repo) lockTheVault() error {
	path, err := r.gitPath(lockName)
	if err != nil {
		return fmt.Errorf("obsync could not find where to take its lock: %w", err)
	}
	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, ownedFileMode)
	if err != nil {
		return fmt.Errorf("obsync could not open its lock at %q: %w", path, err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = held.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("obsync could not take its lock at %q: %w", path, err)
		}
		return &GateFailure{
			Gate: freezeSecondObsync,
			Fact: "another process holds obsync's lock at " + path,
			Remedy: "one vault is one obsync: run a second service for a second vault rather than " +
				"a second obsync for this one. The lock is advisory and dies with the process " +
				"holding it, so there is nothing to clean up — stopping the other obsync is the " +
				"whole of it" + SelfClearing,
		}
	}
	r.lock = held
	return nil
}

// unlockTheVault releases gate 8's lock. Closing the descriptor is what drops
// the flock; the file itself stays, because an owned path obsync declared is
// one it keeps rather than one it tidies away.
func (r *Repo) unlockTheVault() error {
	if r.lock == nil {
		return nil
	}
	held := r.lock
	r.lock = nil
	return held.Close()
}
