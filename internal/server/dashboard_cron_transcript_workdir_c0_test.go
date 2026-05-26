package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestTranscript_RejectsWorkDirWithC0 pins R242-SEC-14 (#649): when the
// persisted CronRun.WorkDir contains a C0 control byte (\t / NUL / DEL /
// any < 0x20 except space), the transcript handler must downgrade to
// Fallback="missing" before constructing jsonlPath via ClaudeProjectSlug.
//
// Pre-fix the WorkDir scan only ran osutil.IsLogInjectionRune which covers
// C1 / bidi / LS-PS but NOT C0 — a tab inside the workdir of an old
// persisted run would slip through to filepath.Join + EvalSymlinks with a
// malformed slug. The fix at dashboard_cron_transcript.go:413 adds the
// `r < 0x20 || r == 0x7f` band so the strict downstream check is no
// longer asked to defend against shell control characters.
//
// Test shape: hand-craft a CronRun JSON file (bypassing AddJob which
// would reject the WorkDir at the write side) so the handler reads the
// persisted shape an old naozhi version could have left on disk.
func TestTranscript_RejectsWorkDirWithC0(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bad  byte // single byte injected mid-WorkDir
	}{
		{"tab", '\t'},
		{"nul", 0x00},
		{"cr", '\r'},
		{"lf", '\n'},
		{"esc", 0x1b},
		{"del", 0x7f},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			claudeDir := filepath.Join(tmp, ".claude")
			storePath := filepath.Join(tmp, "cron_jobs.json")
			cleanWorkDir := filepath.Join(tmp, "workspace")
			if err := os.MkdirAll(cleanWorkDir, 0o755); err != nil {
				t.Fatalf("mkdir workdir: %v", err)
			}

			sched := cron.NewScheduler(cron.SchedulerConfig{StorePath: storePath})

			// Add a clean job — its WorkDir is harmless. The injection point
			// is the persisted run record below, which simulates an
			// older-naozhi disk file with a tampered WorkDir.
			job := cron.Job{
				ID:       strings.Repeat("a", 16),
				Schedule: "@every 1h",
				Prompt:   "fixture",
				WorkDir:  cleanWorkDir,
			}
			if err := sched.AddJob(&job); err != nil {
				t.Fatalf("add job: %v", err)
			}

			runID := strings.Repeat("b", 16)
			jobID := job.ID
			sessionID := "12345678-1234-1234-1234-123456789abc"

			// Inject C0 byte mid-path.
			tamperedWorkDir := cleanWorkDir + string([]byte{c.bad}) + "tail"

			runsDir := filepath.Join(tmp, "runs", jobID)
			if err := os.MkdirAll(runsDir, 0o700); err != nil {
				t.Fatalf("mkdir runs: %v", err)
			}
			now := time.Now().UTC()
			runRec := cron.CronRun{
				RunID:      runID,
				JobID:      jobID,
				State:      cron.RunStateSucceeded,
				Trigger:    cron.TriggerScheduled,
				StartedAt:  now.Add(-time.Minute),
				EndedAt:    now,
				DurationMS: 60_000,
				SessionID:  sessionID,
				WorkDir:    tamperedWorkDir,
			}
			runJSON, err := json.Marshal(runRec)
			if err != nil {
				t.Fatalf("marshal run: %v", err)
			}
			if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
				t.Fatalf("write run json: %v", err)
			}

			h := &CronHandlers{scheduler: sched, claudeDir: claudeDir}
			w := callTranscript(h, jobID, runID)

			if w.Code != http.StatusOK {
				// Handler returns 200 with Fallback="missing" on rejection
				// (the JSON envelope downgrade), not a 4xx — pre-fix would
				// also return 200 but with stale/incorrect transcript data
				// after silently slugging the bad workdir. The load-bearing
				// assertion below is on Fallback, not status.
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			var resp transcriptResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v body=%q", err, w.Body.String())
			}
			if resp.Fallback != "missing" {
				t.Fatalf("Fallback=%q, want \"missing\"; pre-fix C0 (%q) slipped past WorkDir scan and reached ClaudeProjectSlug", resp.Fallback, c.name)
			}
			if len(resp.Turns) != 0 {
				t.Fatalf("turns=%d, want 0 — rejected runs must surface no transcript content", len(resp.Turns))
			}
		})
	}
}
