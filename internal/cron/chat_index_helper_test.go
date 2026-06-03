package cron

import "testing"

// TestAddToChatIndexLocked_SyncsBothIndexes pins the invariant the Scheduler
// godoc promises and that R249-CR-4 / R260528-ARCH-7 (#948 / #1368) made
// structural: addToChatIndexLocked moves chatJobCount and jobsByChat in
// lockstep, and deleteJobLocked is the exact inverse.
func TestAddToChatIndexLocked_SyncsBothIndexes(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10, AllowNilRouter: true})
	key := chatJobKey{Platform: "feishu", ChatID: "c1"}

	jobs := []*Job{
		{ID: "a", Platform: "feishu", ChatID: "c1"},
		{ID: "b", Platform: "feishu", ChatID: "c1"},
	}

	s.mu.Lock()
	for _, j := range jobs {
		s.jobs[j.ID] = j
		s.addToChatIndexLocked(j)
	}
	gotCount := s.chatJobCount[key]
	gotLen := len(s.jobsByChat[key])
	s.mu.Unlock()

	if gotCount != 2 {
		t.Fatalf("chatJobCount = %d, want 2", gotCount)
	}
	if gotLen != 2 {
		t.Fatalf("len(jobsByChat) = %d, want 2", gotLen)
	}

	// deleteJobLocked must unwind both indexes in lockstep.
	s.mu.Lock()
	s.deleteJobLocked(jobs[0])
	afterCount := s.chatJobCount[key]
	afterLen := len(s.jobsByChat[key])
	s.mu.Unlock()

	if afterCount != 1 {
		t.Fatalf("chatJobCount after delete = %d, want 1", afterCount)
	}
	if afterLen != 1 {
		t.Fatalf("len(jobsByChat) after delete = %d, want 1", afterLen)
	}

	// Removing the last job drops both map entries so the working set
	// tracks only live chats.
	s.mu.Lock()
	s.deleteJobLocked(jobs[1])
	_, countPresent := s.chatJobCount[key]
	_, listPresent := s.jobsByChat[key]
	s.mu.Unlock()

	if countPresent {
		t.Fatal("chatJobCount entry should be deleted when count hits zero")
	}
	if listPresent {
		t.Fatal("jobsByChat entry should be deleted when slice empties")
	}
}
