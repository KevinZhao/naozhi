package cron

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// fixtureRunWithJSONLFresh writes a CronRun JSON record + matching JSONL
// (under <claudeDir>/projects/<slug(workdir)>/<sessionID>.jsonl, the real
// CLI's layout) and returns handlers wired to them. The run spans the last
// two minutes; fresh sets CronRun.Fresh so tests can pin both branches of
// the timestamp-less filter (#1046).
func fixtureRunWithJSONLFresh(t *testing.T, fresh bool, jsonlLines []string) (h *Handlers, jobID, runID, claudeDir string) {
	t.Helper()
	now := time.Now().UTC()
	return fixtureRunWindow(t, fresh, now.Add(-2*time.Minute), now, jsonlLines)
}

func fixtureRunWindow(t *testing.T, fresh bool, startedAt, endedAt time.Time, jsonlLines []string) (h *Handlers, jobID, runID, claudeDir string) {
	t.Helper()

	tmp := t.TempDir()
	claudeDir = filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{StorePath: storePath}, cronpkg.SchedulerDeps{})
	job := cronpkg.Job{
		ID:       strings.Repeat("a", 16),
		Schedule: "@every 1h",
		Prompt:   "transcript fixture",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	sessionID := "12345678-1234-1234-1234-123456789abc"
	jobID = job.ID
	runID = strings.Repeat("b", 16)

	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	runRec := cronpkg.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cronpkg.RunStateSucceeded,
		Trigger:    cronpkg.TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		SessionID:  sessionID,
		WorkDir:    workDir,
		Fresh:      fresh,
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}

	projDir := filepath.Join(claudeDir, "projects", claudefs.ProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(strings.Join(jsonlLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	h = &Handlers{deps: Deps{Scheduler: sched, ClaudeDir: claudeDir}}
	return h, jobID, runID, claudeDir
}

// TestTranscript_BoundaryEnd pins the run's time window: turns from before
// the run started (a prior invocation in the same session) never show, and
// R242-SEC-12 (#642): fresh=false filters on the half-open [startedMS,
// endedMS), so an event stamped exactly at EndedAt belongs to the next run
// (which starts there) and not to both. fresh=true owns its JSONL, so its
// upper bound stays inclusive.
func TestTranscript_BoundaryEnd(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	endedAt := time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC)
	line := func(ts time.Time, text string) string {
		return `{"type":"assistant","timestamp":"` + ts.Format(time.RFC3339Nano) +
			`","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
	}
	lines := []string{
		line(startedAt.Add(-time.Hour), "BEFORE_START"),
		line(startedAt.Add(time.Minute), "IN_WINDOW"),
		line(endedAt, "BOUNDARY_EVENT"),
		line(endedAt.Add(time.Second), "PAST_END"),
	}
	for _, tc := range []struct {
		fresh        bool
		wantBoundary bool
	}{{false, false}, {true, true}} {
		h, jobID, runID, _ := fixtureRunWindow(t, tc.fresh, startedAt, endedAt, lines)
		w := callTranscript(h, jobID, runID)
		if w.Code != http.StatusOK {
			t.Fatalf("fresh=%v: status = %d body=%s", tc.fresh, w.Code, w.Body.String())
		}
		var resp transcriptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		got := map[string]bool{}
		for _, tr := range resp.Turns {
			got[tr.Text] = true
		}
		if !got["IN_WINDOW"] || got["BEFORE_START"] || got["PAST_END"] || got["BOUNDARY_EVENT"] != tc.wantBoundary {
			t.Errorf("fresh=%v: turns=%+v, want IN_WINDOW, no BEFORE_START / PAST_END, BOUNDARY_EVENT=%v",
				tc.fresh, resp.Turns, tc.wantBoundary)
		}
	}
}
