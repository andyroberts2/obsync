// Package watcher is obsync's filesystem watcher: an inotify watch on every
// directory in the vault, maintained as directories come and go, whose entire
// contribution is to wake the sync loop sooner than the next tick would.
//
// It is one of the two things this design injects, and the only one whose
// production form talks to the kernel. The other is the clock.
//
// **The watcher never says what changed.** Every sync run asks git, and that
// split is what keeps obsync correct against the three things a watcher cannot
// be trusted about: a dropped inotify event, an exhausted watch budget, and the
// third writer obsync can neither see nor coordinate with. A design that
// commits what the watcher reported is wrong in all three; one that commits
// what git reports degrades to a plain poller when the watcher dies, and stays
// correct (§2).
//
// That is why the channel below carries struct{} and why there is no other
// method on it worth having.
//
// # Silent non-delivery is documented, not detected
//
// inotify can fail to deliver an event without saying so — a filesystem that
// does not report through it at all, a watch registered a moment after the
// write it would have reported, a mount obsync is on the wrong side of. obsync
// does not try to find out. The only honest probe would be to write into the
// vault and see whether it hears about its own write, which is a write into a
// human's vault to answer a question the tick already answers: the tick is the
// bound on how long a dropped event can cost, and every run asks git what
// changed regardless. So the cost of non-delivery is latency, it is bounded,
// and there is nothing to detect that would change what obsync does about it
// (§1). Its operator-facing home is the operations page (#42).
package watcher

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// wakeInterval is the shortest gap between two wake-ups. The first event after
// a quiet spell wakes the loop at once; everything inside the interval after it
// is one wake-up rather than hundreds.
//
// The loop takes out one deadline per wake-up (§2), so an uncoalesced event
// stream is a busy loop as well as timer garbage: a bulk import into a vault
// produces thousands of events a second and not one of them carries anything
// the run they defer will not ask git about anyway.
//
// 100ms is ignis's own browser-side coalescing floor, from the measurements
// behind §2's quiet window — the shortest interval at which the writer obsync
// is built beside can produce genuinely separate writes. It is 1% of the quiet
// window, so the moment the vault went quiet is still measured to within a
// hundredth of the thing being measured.
//
// It is real time rather than the injected clock deliberately: the watcher is
// itself the injected seam, so nothing obsync's tests assert can depend on it,
// and a clock inside the fake's counterpart would be a third injection.
const wakeInterval = 100 * time.Millisecond

// Watcher watches a vault and wakes the sync loop.
//
// It is created by Watch and read through Wakes. A watcher that cannot watch
// the whole vault stands down rather than watching part of it, and a watcher
// that has stood down is one whose channel is closed — which the loop reads as
// tick-only mode, the mode obsync runs in when there is no watcher at all.
type Watcher struct {
	log   *slog.Logger
	vault string

	// notify is the kernel's side. It is nil on a watcher that never got one,
	// which is a watcher that has already stood down.
	notify *fsnotify.Watcher

	// wakes has room for one, which is the whole of the coalescing that costs
	// nothing: while a wake-up is pending, every further event is already
	// represented by it.
	//
	// It is closed by whichever of deliver and Watch owns it, and by nothing
	// else — in particular never by Close, which can be called from another
	// goroutine while deliver is mid-send. Closing a channel out from under a
	// sender is a panic on obsync's shutdown path, which is exactly where a
	// panic is least welcome.
	wakes chan struct{}

	// done asks deliver to stop and finished says it has. Close waits on the
	// second, so a watcher that has been closed is one whose channel is closed
	// and whose watches are gone, with nothing still running behind it.
	done     chan struct{}
	finished chan struct{}

	// watched is every path obsync has ever handed to the kernel, and it is
	// what tells a directory obsync was watching apart from an ordinary file
	// when one of them is renamed (see maintain). It is written by the walk
	// and read by maintain, both on deliver's goroutine and neither before it
	// starts, so it needs no lock.
	//
	// It is append-only deliberately: a renamed directory keeps being reported
	// under the name it was registered with, so the name obsync has to
	// recognise is the old one, and forgetting it is forgetting the only thing
	// that identifies the event. The set is bounded by the number of distinct
	// directory names a vault has held.
	watched map[string]struct{}

	closeOne sync.Once
	stopOne  sync.Once
}

// Watch starts watching the vault, and never fails.
//
// A vault obsync cannot watch is tick-only mode, not a reason to refuse to
// sync: latency degrades to the tick and what obsync commits does not change,
// because every run asks git what changed. So the failures are a WARN and a
// watcher that has stood down, rather than an error a caller has to decide
// about.
func Watch(vault string, log *slog.Logger) *Watcher {
	w := &Watcher{
		log:      log,
		vault:    filepath.Clean(vault),
		wakes:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		watched:  map[string]struct{}{},
	}

	if err := w.open(); err != nil {
		// standDown gives back whatever open got as far as taking — a watch on
		// the vault root with none of the tree behind it is the half-watched
		// vault §1 refuses. Nothing is running to own the two channels, so
		// they are closed here: a watcher that never delivered anything is
		// already in the state deliver would have left behind.
		w.standDown(err)
		w.closeWakes()
		close(w.finished)
		return w
	}

	go w.deliver()
	return w
}

// open takes the kernel's side and registers the vault's watches, and answers
// with the reason obsync is not watching if it could not.
func (w *Watcher) open() error {
	notify, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.notify = notify
	return w.watchTree(w.vault)
}

// Wakes is the channel the sync loop waits on. A value on it says that
// something in the vault happened and never what; the channel closing says the
// watcher has stood down and obsync is in tick-only mode from here.
func (w *Watcher) Wakes() <-chan struct{} { return w.wakes }

// Close tears down every watch and returns once nothing is still running. It is
// idempotent, and safe on a watcher that stood down before it ever delivered
// anything.
func (w *Watcher) Close() error {
	err := w.tearDown()
	<-w.finished
	return err
}

// tearDown stops the watches. It is separate from Close because standDown and
// deliver both give the watches back on their own way out, and a goroutine
// cannot wait for itself to finish.
func (w *Watcher) tearDown() error {
	w.stopOne.Do(func() { close(w.done) })
	if w.notify == nil {
		return nil
	}
	return w.notify.Close()
}

// deliver is the watcher's one goroutine: it keeps the watches in step with the
// directories that exist, and turns everything else into wake-ups.
//
// Both of fsnotify's channels are drained here and nowhere else. They are
// unbuffered on the sending side, so a consumer that stops reading either one
// stops the kernel reader with it.
func (w *Watcher) deliver() {
	defer close(w.finished)
	defer w.closeWakes()

	// A stopped timer whose channel is only ever read while it is running,
	// which is what holding means below. Go 1.23's timers guarantee no stale
	// value survives a Stop, so there is nothing to drain.
	hold := time.NewTimer(wakeInterval)
	hold.Stop()
	defer hold.Stop()

	holding, pending := false, false
	for {
		select {
		case <-w.done:
			return

		case event, open := <-w.notify.Events:
			if !open {
				return
			}
			if err := w.maintain(event); err != nil {
				// A watcher obsync has already closed is not a watcher that
				// cannot watch. ErrClosed here is Close racing an event on
				// the way out, and standing down would put a WARN saying
				// obsync cannot watch the vault in the log of a process that
				// is stopping — a level that means true but self-healing, on
				// a sentence that is neither (§9).
				if !errors.Is(err, fsnotify.ErrClosed) {
					w.standDown(err)
				}
				return
			}
			if holding {
				pending = true
				continue
			}
			w.wake()
			holding = true
			hold.Reset(wakeInterval)

		case <-hold.C:
			holding = false
			if pending {
				pending = false
				w.wake()
				holding = true
				hold.Reset(wakeInterval)
			}

		case err, open := <-w.notify.Errors:
			if !open {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// The kernel's queue filled and events were dropped. That is
				// non-delivery obsync can see, and it costs latency and
				// nothing else: the run this wakes asks git what changed, so
				// the dropped events are in its answer anyway. Not news, and
				// not a reason to give up watches that are all still good.
				w.log.Debug("the kernel dropped filesystem events; the next sync run reads the "+
					"vault from git either way", "vault_path", w.vault)
				w.wake()
				continue
			}
			w.standDown(err)
			return
		}
	}
}

// maintain keeps the set of watches in step with the set of directories, which
// is the whole of what "a watch per directory, maintained as directories come
// and go" needs: a directory that appears is walked and watched, and one that
// goes takes its watch with it, because the kernel drops a watch on the inode
// it was for.
//
// A symlink is not followed. git does not follow one either — it stores the
// link itself as a blob — so watching its target would be watching something
// that is not vault content, and a link pointing back into the vault would be
// a loop.
//
// # A renamed folder is a folder that lost its watch
//
// Renaming a folder is the most ordinary thing a person does to one, and it is
// the one act that takes a watch away without taking the directory away.
// Measured against fsnotify v1.10.1 and the kernel below it, `mv Projects Work`
// inside the vault delivers three events in this order:
//
//	Rename  <vault>/Projects   IN_MOVED_FROM, from the parent's watch
//	Create  <vault>/Work       IN_MOVED_TO,   from the parent's watch
//	Rename  <vault>/Projects   IN_MOVE_SELF,  from the folder's own watch
//
// and the third one is where the watch goes: inotify reports a watched
// directory moving but never says where to, so fsnotify gives the watch back
// rather than hold one whose path it can no longer state. That happens *after*
// the Create, so the walk the Create starts is undone a moment later and the
// folder ends up watched by nothing — silently, and for as long as the process
// lives. Everything created inside it afterwards is unwatched too, because the
// only event that would have said so was the one the missing watch would have
// carried. That is a vault syncing at two speeds with nothing to tell them
// apart, which is the state §1 refuses.
//
// So a Rename naming a directory obsync watched is answered by walking the
// vault again, which re-registers whatever lost its watch under whatever name
// it has now. It is the same act as startup, and the third event is the one
// that lands: the first two arrive before the watch is given back, so the walks
// they start find it still held and change nothing. Costing two extra walks to
// be sure of the one that matters is the right way round for something that
// happens when a person renames a folder.
//
// The name is the discriminator, and it has to be. inotify appends a filename
// to a parent's event and appends nothing to a watch's own, so an IN_MOVE_SELF
// arrives named exactly as obsync registered it — which is why watched holds
// the names obsync handed over rather than the names on disk now. A note being
// renamed, which happens constantly, costs a map lookup and no more, and the
// worst a name that once belonged to a folder and now belongs to a note can
// cost is one walk that finds nothing to do.
func (w *Watcher) maintain(event fsnotify.Event) error {
	switch {
	case event.Has(fsnotify.Create):
		info, err := os.Lstat(event.Name)
		if err != nil || !info.IsDir() {
			return nil
		}
		return w.rewatch(event.Name)
	case event.Has(fsnotify.Rename):
		if _, ours := w.watched[event.Name]; !ours {
			return nil
		}
		return w.rewatch(w.vault)
	}
	return nil
}

// rewatch walks a directory back into the set of watches, and forgives the one
// failure that leaves no gap behind it: a folder made and unmade before obsync
// could watch it is the ordinary shape of a temporary directory, and there is
// nothing left there to be unwatched.
func (w *Watcher) rewatch(root string) error {
	if err := w.watchTree(root); err != nil && !gone(err) {
		return err
	}
	return nil
}

// watchTree registers a watch for root and for every directory under it, and
// answers with the first failure that leaves a gap.
//
// It walks rather than watching root alone because `mkdir -p a/b/c` creates all
// three before obsync can watch any of them: by the time the event for `a`
// arrives, `b` and `c` exist and no event for them is ever coming.
//
// root itself is not excused. A directory below it that went away between the
// walk reaching it and the walk entering it leaves nothing unwatched, but a
// root that is not there means nothing was watched at all — which is a vault
// obsync is not watching rather than a walk that finished.
//
// The vault's `.git` is not walked. It is not vault content — it is outside the
// working tree by construction, which is exactly why §6 stages obsync's own
// writes inside it — so no change in it is ever a change git will report. It is
// also written constantly by every git obsync runs, so watching it would make
// each sync run wake the next one, forever, on a vault nobody is touching. That
// is a boundary of what the vault is, not a self-write ignore list: obsync's
// own writes into the working tree are watched like anyone else's, and a
// self-triggered wake finds a clean tree and does nothing (§4).
func (w *Watcher) watchTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root || !gone(err) {
				return err
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if err := w.notify.Add(path); err != nil {
			if path == root || !gone(err) {
				return err
			}
			return nil
		}
		w.watched[path] = struct{}{}
		return nil
	})
}

// gone reports whether an error means the thing to watch is not there any more,
// which is the one failure that leaves no gap behind it.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// wake tells the loop that something happened. It never blocks: a wake-up
// already pending says everything a second one would.
func (w *Watcher) wake() {
	select {
	case w.wakes <- struct{}{}:
	default:
	}
}

// standDown gives every watch back and says why. Saying and tearing down are
// one act rather than two calls a caller has to remember to pair, because the
// half of the pair that matters is the one that is easy to leave out.
//
// Wholesale is the decision, not a simplification: a half-watched vault syncs
// at two speeds with nothing to tell them apart, so a vault obsync cannot watch
// completely is one it does not watch at all (§1). The tick does not shorten to
// compensate — it is the same 60s it always was, and what obsync commits is the
// same either way, because every run asks git.
func (w *Watcher) standDown(err error) {
	defer func() { _ = w.tearDown() }()

	switch {
	case errors.Is(err, syscall.ENOSPC):
		// The kernel's watch budget is per-UID, and obsync runs as the same
		// UID as ignis on purpose (§8), so the two share it by design — which
		// is why the sysctl is worth naming rather than leaving to be found.
		w.log.Warn("the kernel has no inotify watches left; obsync is watching nothing and running "+
			"in tick-only mode",
			"raise", "fs.inotify.max_user_watches",
			"note", "the watch budget is per-UID, so it is shared with ignis and anything else running as this user",
			"effect", "the vault still syncs; it is noticed within a tick rather than within the quiet window",
			"vault_path", w.vault)
	default:
		// The errno is relayed rather than turned into advice. The other
		// per-UID budget — `fs.inotify.max_user_instances` — presents as
		// EMFILE, which is equally what a process out of file descriptors
		// gets, so naming a sysctl here would be obsync guessing at a cause
		// from an ambiguous fact rather than reporting one.
		w.log.Warn("obsync cannot watch the vault and is running in tick-only mode",
			"problem", err,
			"effect", "the vault still syncs; it is noticed within a tick rather than within the quiet window",
			"vault_path", w.vault)
	}
}

// closeWakes closes the channel the loop reads, which is how a watcher says it
// has stood down. It happens exactly once however many ways obsync arrives at
// it.
func (w *Watcher) closeWakes() {
	w.closeOne.Do(func() { close(w.wakes) })
}
