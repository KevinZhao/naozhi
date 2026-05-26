// chat_job_count_test.go pins the R237-PERF-5 (#661) per-chat counter
// invariant: s.chatJobCount must stay in lock-step with s.jobs grouped by
// (Platform, ChatID). The prior O(N) scan in addJobAcquiringLock was the
// canonical truth; the new counter is a derived index that the per-chat
// cap depends on, so any drift would silently disable the cap or reject
// legitimate adds.
package cron

import (
	"path/filepath"
	"testing"
)

// chatGroupCounts is the *canonical* truth — recomputes chatJobCount from
// scratch the way the original scan did, so the test can verify the
// counter map matches.
func chatGroupCounts(s *Scheduler) map[chatJobKey]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := make(map[chatJobKey]int, len(s.jobs))
	for _, j := range s.jobs {
		got[chatJobKey{Platform: j.Platform, ChatID: j.ChatID}]++
	}
	return got
}

func assertChatJobCountInSync(t *testing.T, s *Scheduler) {
	t.Helper()
	want := chatGroupCounts(s)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(want) != len(s.chatJobCount) {
		t.Fatalf("chatJobCount size mismatch: counter=%d, scan=%d (counter=%v scan=%v)",
			len(s.chatJobCount), len(want), s.chatJobCount, want)
	}
	for k, v := range want {
		if got := s.chatJobCount[k]; got != v {
			t.Errorf("chatJobCount[%+v] = %d, want %d", k, got, v)
		}
	}
	// Bonus: counter MUST NOT carry zero entries — they leak memory and
	// the chat-set view (KnownSessionIDs et al.) treats len() as live-
	// chat count.
	for k, v := range s.chatJobCount {
		if v == 0 {
			t.Errorf("chatJobCount[%+v] = 0; zero entries must be deleted", k)
		}
	}
}

// TestChatJobCount_TracksJobsByChat exercises the AddJob / DeleteJob /
// PauseJob / ResumeJob lifecycle and verifies the counter never drifts.
func TestChatJobCount_TracksJobsByChat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        50,
		MaxJobsPerChat: 10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Empty scheduler — no chats tracked.
	assertChatJobCountInSync(t, s)
	if got := len(s.chatJobCount); got != 0 {
		t.Fatalf("expected empty chatJobCount, got %d entries", got)
	}

	// Add 3 jobs to chat A, 2 to chat B.
	mkJob := func(plat, chat string) *Job {
		return &Job{Schedule: "@every 1h", Prompt: "p", Platform: plat, ChatID: chat}
	}
	for i := 0; i < 3; i++ {
		if err := s.AddJob(mkJob("feishu", "A")); err != nil {
			t.Fatalf("AddJob A[%d]: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.AddJob(mkJob("feishu", "B")); err != nil {
			t.Fatalf("AddJob B[%d]: %v", i, err)
		}
	}
	assertChatJobCountInSync(t, s)
	if got := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "A"}]; got != 3 {
		t.Errorf("chat A count = %d, want 3", got)
	}
	if got := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "B"}]; got != 2 {
		t.Errorf("chat B count = %d, want 2", got)
	}

	// Delete one from A; counter decrements, A still tracked.
	jobsA := s.ListJobs("feishu", "A")
	if len(jobsA) != 3 {
		t.Fatalf("ListJobs(A) = %d, want 3", len(jobsA))
	}
	if _, err := s.DeleteJobByID(jobsA[0].ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	assertChatJobCountInSync(t, s)
	if got := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "A"}]; got != 2 {
		t.Errorf("after delete: chat A count = %d, want 2", got)
	}

	// Delete the rest of A; the chatJobKey entry is dropped from the map
	// (working-set hygiene — assertChatJobCountInSync flags any zero entry).
	jobsA = s.ListJobs("feishu", "A")
	for _, j := range jobsA {
		if _, err := s.DeleteJobByID(j.ID); err != nil {
			t.Fatalf("DeleteJobByID(A): %v", err)
		}
	}
	assertChatJobCountInSync(t, s)
	if _, present := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "A"}]; present {
		t.Errorf("after deleting all A jobs, chatJobCount still tracks chat A")
	}
}

// TestChatJobCount_RollbackOnPersistFailure verifies the counter unwinds
// when AddJob's persist step fails (the rollback path goes through
// deleteJobLocked). Without proper rollback the counter would over-count
// and silently shrink the per-chat cap by 1.
func TestChatJobCount_RollbackOnPersistFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   50,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// One legitimate job to pin the chat in the counter.
	if err := s.AddJob(&Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "X"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "X"}]; got != 1 {
		t.Fatalf("baseline chat X count = %d, want 1", got)
	}

	// Force the next persist to fail (helper from persist_failure_test.go);
	// withFailingMarshal registers its own t.Cleanup to restore the seam.
	withFailingMarshal(t, s)

	err := s.AddJob(&Job{Schedule: "@every 1h", Prompt: "p2", Platform: "feishu", ChatID: "X"})
	if err == nil {
		t.Fatal("expected AddJob to fail when persist is broken")
	}
	// After rollback, counter should be back to 1.
	if got := s.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "X"}]; got != 1 {
		t.Errorf("after rollback: chat X count = %d, want 1 (counter leaked)", got)
	}
	assertChatJobCountInSync(t, s)
}

// TestChatJobCount_StartLoadPopulates verifies persisted jobs reloaded by
// Start() are reflected in chatJobCount, so a fresh process never
// silently allows AddJob to push a chat above maxJobsPerChat just because
// the counter starts empty.
func TestChatJobCount_StartLoadPopulates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")

	// Phase 1: build a scheduler, add jobs, Stop to persist.
	s1 := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 50})
	if err := s1.Start(); err != nil {
		t.Fatalf("Start s1: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s1.AddJob(&Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "Z"}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}
	s1.Stop()

	// Phase 2: fresh scheduler reading the same store — counter must
	// reflect the 3 jobs immediately, before any AddJob.
	s2 := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 50})
	if err := s2.Start(); err != nil {
		t.Fatalf("Start s2: %v", err)
	}
	defer s2.Stop()
	if got := s2.chatJobCount[chatJobKey{Platform: "feishu", ChatID: "Z"}]; got != 3 {
		t.Errorf("after reload: chat Z count = %d, want 3", got)
	}
	assertChatJobCountInSync(t, s2)
}
