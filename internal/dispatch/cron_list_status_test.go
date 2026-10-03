package dispatch

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestHandleCronList_StatusTags: /cron list tells an auto-paused job apart
// from a manually paused one, so the user knows the job stopped by itself.
func TestHandleCronList_StatusTags(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{scheduler: &fakeCronScheduler{listJobsResult: []CronJob{
		{ID: "aaaa", Schedule: "@hourly", Prompt: "active"},
		{ID: "bbbb", Schedule: "@hourly", Prompt: "manual", Paused: true},
		{ID: "cccc", Schedule: "@hourly", Prompt: "broken", Paused: true, AutoPaused: true},
	}}}
	var out string
	d.handleCronList(platform.IncomingMessage{Platform: "feishu", ChatID: "c1"}, func(s string) { out = s })

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("reply = %q, want a header and 3 rows", out)
	}
	for i, want := range []string{"active", "manual [暂停]", "broken [自动暂停：连续失败]"} {
		if !strings.HasSuffix(lines[i+1], want) {
			t.Errorf("row %d = %q, want suffix %q", i, lines[i+1], want)
		}
	}
}
