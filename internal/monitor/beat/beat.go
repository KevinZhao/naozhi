// Package beat tracks heartbeats of the gateway's critical loops so /health
// can tell a stopped loop from a quiet one (docs/rfc/planner.md §3.8).
package beat

import (
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/wake"
)

// StartupGrace is how long after New a Registry reports no stale loop: boot
// work can hold a loop's first iteration past two periods.
const StartupGrace = 3 * time.Minute

// Registry holds the registered loops. Loops may register at any time; the
// nil Registry registers nothing and reports nothing stale.
//
// Beat ages and the startup grace run on the monotonic clock, which the
// loops' own tickers run on too: a sleep of any length costs no loop its
// period, and a wall-clock step moves nothing. A detected wake still restarts
// every loop's baseline from now (§15.1), giving each loop two full periods
// to resume after waking.
type Registry struct {
	clock wake.Clock
	start time.Duration // Mono at New

	mu       sync.Mutex
	detector *wake.Detector
	loops    map[string]*loop
}

type loop struct {
	period time.Duration
	last   time.Duration // Mono at the last beat
}

// New returns an empty Registry whose startup grace begins now.
func New(clock wake.Clock) *Registry {
	return &Registry{
		clock:    clock,
		start:    clock.Mono(),
		detector: wake.NewDetector(clock),
		loops:    make(map[string]*loop),
	}
}

// Beat is one loop's handle. The nil Beat ignores Beat calls, so a loop
// started without a Registry needs no guard.
type Beat struct {
	r *Registry
	l *loop
}

// Register adds a loop expected to beat at least once per period and returns
// its handle. Registering counts as the first beat; registering a name again
// replaces its period and restarts it.
func (r *Registry) Register(name string, period time.Duration) *Beat {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.observeLocked()
	l, ok := r.loops[name]
	if !ok {
		l = &loop{}
		r.loops[name] = l
	}
	l.period, l.last = period, now
	return &Beat{r: r, l: l}
}

// Beat records that the loop completed an iteration.
func (b *Beat) Beat() {
	if b == nil {
		return
	}
	b.r.mu.Lock()
	b.l.last = b.r.observeLocked()
	b.r.mu.Unlock()
}

// Stale returns, sorted, the loops whose last beat is more than two periods
// old; nil during the startup grace.
func (r *Registry) Stale() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.observeLocked()
	if now-r.start < StartupGrace {
		return nil
	}
	var stale []string
	for name, l := range r.loops {
		if now-l.last > 2*l.period {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	return stale
}

// observeLocked returns the monotonic now, first restarting every loop's
// baseline from it when the machine has just woken.
func (r *Registry) observeLocked() time.Duration {
	now := r.clock.Mono()
	if r.detector.Woke() {
		for _, l := range r.loops {
			l.last = now
		}
	}
	return now
}
