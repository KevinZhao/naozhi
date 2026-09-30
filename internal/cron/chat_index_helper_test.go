package cron

import "testing"

// TestAddToChatIndexLocked_SyncsBothIndexes pins the invariant the Scheduler
// godoc promises and that R249-CR-4 / R260528-ARCH-7 (#948 / #1368) made
// structural: indexLocked moves chatJobCount and jobsByChat in lockstep, and
// deleteLocked is the exact inverse.
func TestAddToChatIndexLocked_SyncsBothIndexes(t *testing.T) {
	key := chatJobKey{Platform: "feishu", ChatID: "c1"}
	jobs := []*Job{
		{ID: "a", Platform: "feishu", ChatID: "c1"},
		{ID: "b", Platform: "feishu", ChatID: "c1"},
	}
	tbl := tableWith(t, jobs...)

	if got := tbl.chatJobCount[key]; got != 2 {
		t.Fatalf("chatJobCount = %d, want 2", got)
	}
	if got := len(tbl.jobsByChat[key]); got != 2 {
		t.Fatalf("len(jobsByChat) = %d, want 2", got)
	}

	// deleteLocked must unwind both indexes in lockstep.
	tbl.deleteLocked(jobs[0])
	if got := tbl.chatJobCount[key]; got != 1 {
		t.Fatalf("chatJobCount after delete = %d, want 1", got)
	}
	if got := len(tbl.jobsByChat[key]); got != 1 {
		t.Fatalf("len(jobsByChat) after delete = %d, want 1", got)
	}

	// Removing the last job drops both map entries so the working set
	// tracks only live chats.
	tbl.deleteLocked(jobs[1])
	if _, present := tbl.chatJobCount[key]; present {
		t.Fatal("chatJobCount entry should be deleted when count hits zero")
	}
	if _, present := tbl.jobsByChat[key]; present {
		t.Fatal("jobsByChat entry should be deleted when slice empties")
	}
}
