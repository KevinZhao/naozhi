package cron

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/monitor/beat"
)

// tickBeatClock is a never-sleeping beat clock.
type tickBeatClock struct{ mono time.Duration }

func (c *tickBeatClock) Now() time.Time      { return time.Unix(0, 0).Add(c.mono) }
func (c *tickBeatClock) Mono() time.Duration { return c.mono }

// Start registers cron_tick and schedules an Every(tickBeatPeriod) entry
// whose job beats it.
func TestStart_SchedulesTickBeat(t *testing.T) {
	c := &tickBeatClock{}
	beats := beat.New(c)
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5},
		SchedulerDeps{Router: &fakeRouter{}, Beats: beats})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	var tick robfigcron.Job
	for _, e := range s.cron.Entries() {
		if e.Schedule == robfigcron.Every(tickBeatPeriod) {
			tick = e.Job
		}
	}
	if tick == nil {
		t.Fatalf("no Every(%s) entry after Start", tickBeatPeriod)
	}
	c.mono += beat.StartupGrace + time.Hour
	if got := beats.Stale(); !slices.Equal(got, []string{tickBeat}) {
		t.Fatalf("before the tick: Stale() = %v, want [%s]", got, tickBeat)
	}
	tick.Run()
	if got := beats.Stale(); got != nil {
		t.Errorf("after the tick: Stale() = %v, want nil", got)
	}
}
