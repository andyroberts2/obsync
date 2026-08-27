// Package clock is one of the two things obsync injects, and the only source
// of time the rest of it may use.
//
// The reason it exists is testing rather than abstraction: every timing rule in
// this design is a constant with a measured reason beside it — the 10s quiet
// window, the 5min max-wait cap, the 60s tick, the 1s settle interval, the 120s
// network deadline, the 15m backoff ceiling — and a suite that slept through
// them would take longer to run than the vault takes to sync. Driving them from
// a fake is what makes each one deterministic instead (§21's testing
// decisions).
//
// The budget is exactly two injections, the clock and the watcher, and this is
// one of them. Nothing else in obsync is swappable — in particular git is not,
// because a fake git tests obsync's beliefs about git and those beliefs are the
// whole risk surface.
package clock

import "time"

// Clock is the time obsync reads and the time it waits on. A reading is what a
// duration is measured from, and the two waits are the two kinds this design
// has: one obsync races other events against, and one it simply spends.
type Clock interface {
	Now() time.Time
	// After delivers one value once d has passed. A nil result would block
	// forever, which is why nothing here returns one: a caller that must not
	// be timed out does not call After at all (§1's local commands).
	After(d time.Duration) <-chan time.Time
	// Sleep spends d and returns, and it is a different act from After rather
	// than a convenience over it. After is for a deadline obsync selects on
	// alongside something else — a wake-up, a git finishing, a SIGTERM — where
	// the deadline is one outcome among several. Sleep is the settle interval
	// (§6): obsync is not waiting *for* anything, it is putting a known gap
	// between two readings of the filesystem, and nothing that could arrive in
	// the meantime would change what it does next.
	Sleep(d time.Duration)
}

// System is the clock obsync runs on outside a test.
type System struct{}

func (System) Now() time.Time { return time.Now() }

// After is time.After, whose timer is not collectable until it fires. That
// costs one live timer per network git for at most the 120s deadline, which is
// the whole of the exposure while the loop is serialized and only one git is
// ever in flight.
func (System) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep is time.Sleep, which allocates no timer that outlives it. The settle
// interval is spent inside a sync run, so it also delays a SIGTERM by up to
// that long — 1s against the ~30s obsync has to exit in (§1).
func (System) Sleep(d time.Duration) { time.Sleep(d) }
