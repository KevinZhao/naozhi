package cron

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// resetCountingRouter is a streakRouter (every run succeeds) that counts
// session resets per key.
type resetCountingRouter struct {
	streakRouter
	resetMu sync.Mutex
	resets  map[string]int
}

func (r *resetCountingRouter) Reset(key string) {
	r.resetMu.Lock()
	defer r.resetMu.Unlock()
	r.resets[key]++
}

func (r *resetCountingRouter) resetCount(key string) int {
	r.resetMu.Lock()
	defer r.resetMu.Unlock()
	return r.resets[key]
}

// setFreshInChat is the IM /cron mode edit: UpdateJob of FreshContext alone,
// on the job idPrefix resolves to in (plat, chatID).
func setFreshInChat(s *Scheduler, idPrefix, plat, chatID string, fresh bool) (*Job, error) {
	return s.UpdateJob(idPrefix, JobUpdate{InChat: &JobChat{Platform: plat, ChatID: chatID}, FreshContext: &fresh})
}

// newFreshModeScheduler registers one kept-context IM job in feishu/chat-1
// with a failure history, so a mode change has streaks to clear.
func newFreshModeScheduler(t *testing.T, paused bool) (*Scheduler, *resetCountingRouter, string) {
	t.Helper()
	r := &resetCountingRouter{resets: map[string]int{}}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:   5,
		StorePath: filepath.Join(t.TempDir(), "cron_jobs.json"),
	}, SchedulerDeps{Router: r})
	j := NewJob("@every 10m", "ping", JobIMContext{Platform: "feishu", ChatID: "chat-1"})
	j.Paused = paused
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.editJobForTest(t, j.ID, func(j *Job) {
		j.ConsecutiveFailures = 3
		j.TransientFailures = 2
		j.TransientFailingSince = time.Unix(1_700_000_000, 0)
	})
	return s, r, j.ID
}

// A chat-scoped edit flips the mode both ways by ID prefix, persists it,
// starts the failure streaks over like any edit, and leaves the job's
// identity and pause state alone.
func TestUpdateJobInChat_PersistsBothDirections(t *testing.T) {
	t.Parallel()
	for _, paused := range []bool{false, true} {
		s, _, id := newFreshModeScheduler(t, paused)
		for _, fresh := range []bool{true, false} {
			got, err := setFreshInChat(s, id[:4], "feishu", "chat-1", fresh)
			if err != nil {
				t.Fatalf("paused=%v setFreshInChat(%v): %v", paused, fresh, err)
			}
			if got.ID != id || got.FreshContext != fresh || got.Paused != paused {
				t.Errorf("paused=%v fresh=%v: returned job = {ID:%s Fresh:%v Paused:%v}", paused, fresh, got.ID, got.FreshContext, got.Paused)
			}
			stored := persistedJob(t, s, id)
			if stored.FreshContext != fresh || stored.Paused != paused {
				t.Errorf("paused=%v fresh=%v: stored FreshContext=%v Paused=%v", paused, fresh, stored.FreshContext, stored.Paused)
			}
			if stored.streaks() != (failureStreaks{}) {
				t.Errorf("paused=%v fresh=%v: stored streaks = %+v, want zero", paused, fresh, stored.streaks())
			}
		}
	}
}

// The prefix is resolved in the caller's chat only: a full ID from another
// chat reads as not found and changes nothing, and a prefix that matches two
// jobs is refused.
func TestUpdateJobInChat_ScopeAndAmbiguity(t *testing.T) {
	t.Parallel()
	s, _, id := newFreshModeScheduler(t, false)
	if _, err := setFreshInChat(s, id, "feishu", "chat-2", true); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("other chat: err = %v, want ErrJobNotFound", err)
	}
	if j := s.jobForTest(t, id); j.FreshContext || j.ConsecutiveFailures != 3 {
		t.Errorf("other chat changed the job: FreshContext=%v ConsecutiveFailures=%d", j.FreshContext, j.ConsecutiveFailures)
	}

	other := NewJob("@every 10m", "pong", JobIMContext{Platform: "feishu", ChatID: "chat-1"})
	if err := s.AddJob(other); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := setFreshInChat(s, "", "feishu", "chat-1", true); !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("shared prefix: err = %v, want ErrAmbiguousPrefix", err)
	}
	for _, jid := range []string{id, other.ID} {
		if s.jobForTest(t, jid).FreshContext {
			t.Errorf("ambiguous prefix changed job %s", jid)
		}
	}
}

// A persist failure leaves memory agreeing with the unchanged disk: the old
// mode and the failure history it had.
func TestUpdateJobInChat_RollbackOnPersistFailure(t *testing.T) {
	t.Parallel()
	s, _, id := newFreshModeScheduler(t, false)
	before := s.jobForTest(t, id)
	pre := before.streaks()
	withFailingMarshal(t, s)
	if _, err := setFreshInChat(s, id[:4], "feishu", "chat-1", true); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("err = %v, want ErrPersistFailed", err)
	}
	j := s.jobForTest(t, id)
	if j.FreshContext || j.streaks() != pre {
		t.Errorf("after failed persist: FreshContext=%v streaks=%+v, want false and %+v", j.FreshContext, j.streaks(), pre)
	}
}

// The next run after switching a kept-context job to fresh resets its
// session; before the switch a run leaves it alone.
func TestUpdateJobInChat_NextRunResetsSession(t *testing.T) {
	t.Parallel()
	s, r, id := newFreshModeScheduler(t, false)
	key := sessionkey.CronKey(id)
	runN(s, id, 1)
	if n := r.resetCount(key); n != 0 {
		t.Fatalf("kept-context run reset the session %d times", n)
	}
	if _, err := setFreshInChat(s, id, "feishu", "chat-1", true); err != nil {
		t.Fatalf("setFreshInChat: %v", err)
	}
	runN(s, id, 1)
	if r.resetCount(key) == 0 {
		t.Error("first run after switching to fresh did not reset the session")
	}
}

// A chat-scoped schedule change re-registers the job under its full ID, not
// the prefix the caller typed, so the job keeps firing on the new schedule.
func TestUpdateJobInChat_RescheduleUsesFullID(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	j := NewJob("@every 10m", "ping", JobIMContext{Platform: "feishu", ChatID: "chat-1"})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	id := j.ID
	sched := "@every 20m"
	got, err := s.UpdateJob(id[:4], JobUpdate{InChat: &JobChat{Platform: "feishu", ChatID: "chat-1"}, Schedule: &sched})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if got.ID != id || got.Schedule != sched {
		t.Fatalf("UpdateJob = {ID:%s Schedule:%s}, want {%s %s}", got.ID, got.Schedule, id, sched)
	}
	live := s.jobForTest(t, id)
	if next := s.NextRun(&live); next.IsZero() {
		t.Error("job has no live entry after a chat-scoped reschedule")
	}
}
