// jobs_by_chat_index_test.go pins the R242-GO-9 (#558) per-chat job index
// invariant: s.jobsByChat must stay in lock-step with s.jobs grouped by
// (Platform, ChatID). findByPrefixLocked relies on the index for O(jobs-
// in-chat) scans; any drift would either return a stale *Job (deleted job
// still listed) or miss a legitimate match (added job not yet indexed).
package cron

import (
	"path/filepath"
	"testing"
)

// chatGroupIndex is the canonical truth: recomputes jobsByChat from
// scratch by scanning s.jobs grouped by (Platform, ChatID), so the test
// can compare set-equality with the maintained index.
func chatGroupIndex(s *Scheduler) map[chatJobKey]map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := make(map[chatJobKey]map[string]bool, len(s.jobs))
	for _, j := range s.jobs {
		key := chatJobKey{Platform: j.Platform, ChatID: j.ChatID}
		if got[key] == nil {
			got[key] = make(map[string]bool)
		}
		got[key][j.ID] = true
	}
	return got
}

func assertJobsByChatInSync(t *testing.T, s *Scheduler) {
	t.Helper()
	want := chatGroupIndex(s)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(want) != len(s.jobsByChat) {
		t.Fatalf("jobsByChat size mismatch: index=%d scan=%d (index keys=%v)",
			len(s.jobsByChat), len(want), keysOfChatIndex(s.jobsByChat))
	}
	for k, idSet := range want {
		got := s.jobsByChat[k]
		if len(got) != len(idSet) {
			t.Errorf("jobsByChat[%+v] len = %d, want %d", k, len(got), len(idSet))
		}
		for _, p := range got {
			if p == nil {
				t.Errorf("jobsByChat[%+v] holds nil pointer", k)
				continue
			}
			if !idSet[p.ID] {
				t.Errorf("jobsByChat[%+v] holds stale job %q (not in s.jobs)", k, p.ID)
			}
		}
	}
	// Bonus: zero-length slices must be deleted from the map.
	for k, list := range s.jobsByChat {
		if len(list) == 0 {
			t.Errorf("jobsByChat[%+v] is empty; zero-length entries must be deleted", k)
		}
	}
}

func keysOfChatIndex(m map[chatJobKey][]*Job) []chatJobKey {
	keys := make([]chatJobKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestJobsByChatIndex_TracksAddDelete exercises the AddJob / DeleteJob
// lifecycle and verifies the per-chat index never drifts from s.jobs.
func TestJobsByChatIndex_TracksAddDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        50,
		MaxJobsPerChat: 10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Empty scheduler — no chats indexed.
	assertJobsByChatInSync(t, s)
	if got := len(s.jobsByChat); got != 0 {
		t.Fatalf("expected empty jobsByChat, got %d entries", got)
	}

	mkJob := func(plat, chat string) *Job {
		return &Job{Schedule: "@every 1h", Prompt: "p", Platform: plat, ChatID: chat}
	}

	// Add 3 jobs to chat A, 2 to chat B.
	idsA := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		j := mkJob("feishu", "A")
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob A[%d]: %v", i, err)
		}
		idsA = append(idsA, j.ID)
	}
	for i := 0; i < 2; i++ {
		j := mkJob("feishu", "B")
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob B[%d]: %v", i, err)
		}
	}
	assertJobsByChatInSync(t, s)
	if got := len(s.jobsByChat[chatJobKey{Platform: "feishu", ChatID: "A"}]); got != 3 {
		t.Errorf("chat A index len = %d, want 3", got)
	}
	if got := len(s.jobsByChat[chatJobKey{Platform: "feishu", ChatID: "B"}]); got != 2 {
		t.Errorf("chat B index len = %d, want 2", got)
	}

	// Delete one job from chat A.
	if _, err := s.DeleteJobByID(idsA[0]); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	assertJobsByChatInSync(t, s)
	if got := len(s.jobsByChat[chatJobKey{Platform: "feishu", ChatID: "A"}]); got != 2 {
		t.Errorf("chat A index len after delete = %d, want 2", got)
	}

	// Delete remaining 2 jobs in chat A — entry must drop from map.
	for _, id := range idsA[1:] {
		if _, err := s.DeleteJobByID(id); err != nil {
			t.Fatalf("DeleteJobByID %s: %v", id, err)
		}
	}
	assertJobsByChatInSync(t, s)
	if _, present := s.jobsByChat[chatJobKey{Platform: "feishu", ChatID: "A"}]; present {
		t.Errorf("after deleting all A jobs, jobsByChat still tracks chat A")
	}
}

// TestFindByPrefixLocked_UsesPerChatIndex verifies findByPrefixLocked
// correctly resolves prefix lookups via the per-chat index — same job
// data, but only the matching chat's slice is scanned. The existing
// withJobByPrefix callers (DeleteByPrefix etc.) exercise the full path.
func TestFindByPrefixLocked_UsesPerChatIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        50,
		MaxJobsPerChat: 10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	jA := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "A"}
	if err := s.AddJob(jA); err != nil {
		t.Fatalf("AddJob A: %v", err)
	}
	jB := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "B"}
	if err := s.AddJob(jB); err != nil {
		t.Fatalf("AddJob B: %v", err)
	}

	// Lookup by full ID, scoped to A — must find jA.
	s.mu.RLock()
	got, err := s.findByPrefixLocked(jA.ID, "feishu", "A")
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("findByPrefixLocked A: %v", err)
	}
	if got.ID != jA.ID {
		t.Errorf("got job %q, want %q", got.ID, jA.ID)
	}

	// Same prefix scoped to B — must NOT match jA (cross-chat isolation).
	s.mu.RLock()
	_, err = s.findByPrefixLocked(jA.ID, "feishu", "B")
	s.mu.RUnlock()
	if err == nil {
		t.Errorf("expected ErrJobNotFound when looking up A's ID under chat B")
	}

	// Empty / nonexistent chat returns ErrJobNotFound, not a panic.
	s.mu.RLock()
	_, err = s.findByPrefixLocked("any", "feishu", "ghost")
	s.mu.RUnlock()
	if err == nil {
		t.Errorf("expected ErrJobNotFound for missing chat key")
	}
}
