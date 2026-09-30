// jobs_by_chat_index_test.go pins the R242-GO-9 (#558) per-chat job index
// invariant: s.tbl.jobsByChat must stay in lock-step with s.tbl.jobs grouped by
// (Platform, ChatID). findByPrefixLocked relies on the index for O(jobs-
// in-chat) scans; any drift would either return a stale *Job (deleted job
// still listed) or miss a legitimate match (added job not yet indexed).
package cron

import (
	"path/filepath"
	"testing"
)

// assertJobsByChatInSync checks the per-chat index against a regrouping of the
// job map — the scan the index replaced — taken from one lock hold.
func assertJobsByChatInSync(t *testing.T, s *Scheduler) {
	t.Helper()
	index, want := s.chatIndexForTest()
	if len(want) != len(index) {
		t.Fatalf("jobsByChat size mismatch: index=%d scan=%d (index=%v)", len(index), len(want), index)
	}
	for k, idSet := range want {
		got := index[k]
		if len(got) != len(idSet) {
			t.Errorf("jobsByChat[%+v] len = %d, want %d", k, len(got), len(idSet))
		}
		for _, id := range got {
			if !idSet[id] {
				t.Errorf("jobsByChat[%+v] holds stale job %q (not in s.tbl.jobs)", k, id)
			}
		}
	}
	// Bonus: zero-length slices must be deleted from the map.
	for k, list := range index {
		if len(list) == 0 {
			t.Errorf("jobsByChat[%+v] is empty; zero-length entries must be deleted", k)
		}
	}
}

// TestJobsByChatIndex_TracksAddDelete exercises the AddJob / DeleteJob
// lifecycle and verifies the per-chat index never drifts from s.tbl.jobs.
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
	if index, _ := s.chatIndexForTest(); len(index) != 0 {
		t.Fatalf("expected empty jobsByChat, got %d entries", len(index))
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
	if got := len(s.tbl.forChat(chatJobKey{Platform: "feishu", ChatID: "A"})); got != 3 {
		t.Errorf("chat A index len = %d, want 3", got)
	}
	if got := len(s.tbl.forChat(chatJobKey{Platform: "feishu", ChatID: "B"})); got != 2 {
		t.Errorf("chat B index len = %d, want 2", got)
	}

	// Delete one job from chat A.
	if _, err := s.DeleteJobByID(idsA[0]); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	assertJobsByChatInSync(t, s)
	if got := len(s.tbl.forChat(chatJobKey{Platform: "feishu", ChatID: "A"})); got != 2 {
		t.Errorf("chat A index len after delete = %d, want 2", got)
	}

	// Delete remaining 2 jobs in chat A — entry must drop from map.
	for _, id := range idsA[1:] {
		if _, err := s.DeleteJobByID(id); err != nil {
			t.Fatalf("DeleteJobByID %s: %v", id, err)
		}
	}
	assertJobsByChatInSync(t, s)
	index, _ := s.chatIndexForTest()
	if _, present := index[chatJobKey{Platform: "feishu", ChatID: "A"}]; present {
		t.Errorf("after deleting all A jobs, jobsByChat still tracks chat A")
	}
}

// TestFindByPrefixLocked_UsesPerChatIndex verifies findByPrefixLocked
// correctly resolves prefix lookups via the per-chat index — same job
// data, but only the matching chat's slice is scanned. The existing
// mutateByPrefix callers (DeleteByPrefix etc.) exercise the full path.
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
	got, err := s.findByPrefixForTest(jA.ID, "feishu", "A")
	if err != nil {
		t.Fatalf("findByPrefixLocked A: %v", err)
	}
	if got.ID != jA.ID {
		t.Errorf("got job %q, want %q", got.ID, jA.ID)
	}

	// Same prefix scoped to B — must NOT match jA (cross-chat isolation).
	_, err = s.findByPrefixForTest(jA.ID, "feishu", "B")
	if err == nil {
		t.Errorf("expected ErrJobNotFound when looking up A's ID under chat B")
	}

	// Empty / nonexistent chat returns ErrJobNotFound, not a panic.
	_, err = s.findByPrefixForTest("any", "feishu", "ghost")
	if err == nil {
		t.Errorf("expected ErrJobNotFound for missing chat key")
	}
}
