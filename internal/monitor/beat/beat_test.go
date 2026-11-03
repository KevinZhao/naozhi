package beat

import (
	"slices"
	"testing"
	"time"
)

// fakeClock moves wall and monotonic time together on tick; sleep moves only
// the wall clock, as a machine sleep does.
type fakeClock struct {
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) Now() time.Time      { return c.wall }
func (c *fakeClock) Mono() time.Duration { return c.mono }
func (c *fakeClock) tick(d time.Duration) {
	c.wall = c.wall.Add(d)
	c.mono += d
}
func (c *fakeClock) sleep(d time.Duration) { c.wall = c.wall.Add(d) }

func TestRegistryStale(t *testing.T) {
	tests := []struct {
		name string
		// run starts right after loop "a" (1m period) registered at boot.
		run  func(c *fakeClock, r *Registry, a *Beat)
		want []string
	}{
		{"beating", func(c *fakeClock, r *Registry, a *Beat) {
			for range 10 {
				c.tick(time.Minute)
				a.Beat()
			}
		}, nil},
		{"two periods without a beat is not yet stale", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
			a.Beat()
			c.tick(2 * time.Minute)
		}, nil},
		{"missed two periods", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
			a.Beat()
			c.tick(2*time.Minute + time.Second)
		}, []string{"a"}},
		{"stopped inside the startup grace", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace - time.Second)
		}, nil},
		{"stopped past the startup grace", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
		}, []string{"a"}},
		{"late registration starts from its own registration", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(10 * time.Minute)
			a.Beat()
			r.Register("b", 30*time.Second)
			c.tick(time.Minute)
		}, nil},
		{"late registration goes stale", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(10 * time.Minute)
			r.Register("b", 30*time.Second)
			c.tick(time.Minute + time.Second)
			a.Beat()
		}, []string{"b"}},
		{"sleep restarts every baseline", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
			a.Beat()
			r.Register("b", time.Minute)
			c.sleep(time.Hour)
			c.tick(time.Minute)
		}, nil},
		{"a loop stopped across a sleep goes stale after waking", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
			a.Beat()
			c.sleep(time.Hour)
			if got := r.Stale(); got != nil {
				t.Errorf("right after waking: Stale() = %v, want nil", got)
			}
			c.tick(2*time.Minute + time.Second)
		}, []string{"a"}},
		{"wall drift under the sleep threshold is not a sleep", func(c *fakeClock, r *Registry, a *Beat) {
			c.tick(StartupGrace)
			a.Beat()
			c.tick(time.Minute)
			c.sleep(time.Minute + 30*time.Second)
		}, []string{"a"}},
		{"stale names are sorted", func(c *fakeClock, r *Registry, a *Beat) {
			r.Register("c", time.Minute)
			r.Register("b", time.Minute)
			c.tick(StartupGrace)
		}, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClock{wall: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
			r := New(c)
			tt.run(c, r, r.Register("a", time.Minute))
			if got := r.Stale(); !slices.Equal(got, tt.want) {
				t.Errorf("Stale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNilRegistry(t *testing.T) {
	var r *Registry
	r.Register("a", time.Minute).Beat()
	if got := r.Stale(); got != nil {
		t.Errorf("nil Registry Stale() = %v, want nil", got)
	}
}
