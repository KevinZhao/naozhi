package cron

import (
	"crypto/rand"
	"fmt"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// Job represents a scheduled cron task.
type Job struct {
	ID        string    `json:"id"`
	Schedule  string    `json:"schedule"`
	Prompt    string    `json:"prompt"`
	Platform  string    `json:"platform"`
	ChatID    string    `json:"chat_id"`
	ChatType  string    `json:"chat_type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Paused    bool      `json:"paused"`

	// Optional working directory override for the CLI process.
	WorkDir string `json:"work_dir,omitempty"`

	// Optional notification target for dashboard-created jobs.
	// When set, execution results are also sent to this IM channel.
	NotifyPlatform string `json:"notify_platform,omitempty"`
	NotifyChatID   string `json:"notify_chat_id,omitempty"`

	// Notify controls whether execution results are pushed to an IM channel
	// after each run. Tri-state pointer so old jobs (nil) preserve legacy
	// behavior: IM-created jobs reply to their source chat; dashboard-created
	// jobs honor per-job NotifyPlatform/NotifyChatID if set.
	// Explicit true/false lets dashboard users toggle delivery using the
	// scheduler's notify_default target (or per-job override) without touching
	// platform/chat fields.
	Notify *bool `json:"notify,omitempty"`

	// FreshContext, when true, resets the cron session before each run so the
	// CLI starts from a clean slate instead of inheriting the conversation
	// history from previous executions. Default (false) preserves the existing
	// behavior — session is long-lived and each run appends a new turn to the
	// accumulated context. Fresh mode keeps per-run latency bounded when the
	// job repeatedly does independent work (reviews, status scans, etc.).
	FreshContext bool `json:"fresh_context,omitempty"`

	// Last execution result, persisted across restarts. LastRunAt has no
	// omitempty: encoding/json does not drop zero-value time.Time structs,
	// so the tag was a lint-only hint that falsely implied zero-value
	// omission. Dashboard code already checks LastRunAt.IsZero() before
	// formatting, which handles the "never run" case.
	LastResult string    `json:"last_result,omitempty"`
	LastRunAt  time.Time `json:"last_run_at"`
	LastError  string    `json:"last_error,omitempty"`

	entryID robfigcron.EntryID // runtime only, not persisted
}

// generateID returns a 16-char hex string (8 bytes of entropy).
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("%x", b)
}

// cronParser is the shared parser for all schedule validation and preview.
var cronParser = robfigcron.NewParser(
	robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor,
)

// minCronInterval is the minimum allowed interval between cron runs.
// Prevents resource exhaustion from overly frequent schedules like "@every 1s".
const minCronInterval = 5 * time.Minute

// jobTimeoutRatio scales a job's schedule period into its execution timeout.
// 0.8 leaves a 20% margin between timeout expiry and the next scheduled tick,
// so a long-running job does not collide with its own next trigger (which the
// SkipIfStillRunning chain and jobRunningGuard would otherwise drop entirely).
const jobTimeoutRatio = 0.8

// minJobTimeout floors the scaled timeout so schedules near minCronInterval
// (5m × 0.8 = 4m) still leave the job a workable budget. 3m matches the
// smallest prompt-roundtrip plus startup shim reconnect observed in prod.
const minJobTimeout = 3 * time.Minute

// computeJobTimeout returns the per-run deadline for a job whose schedule is
// `schedule`. The timeout is period × jobTimeoutRatio, clamped to
// [minJobTimeout, maxCap]. maxCap is the scheduler-level ceiling
// (SchedulerConfig.ExecTimeout) so operators retain a global upper bound.
//
// Clamp order matters: cap is applied last so a caller-configured cap that
// happens to sit below minJobTimeout (pathological / misconfiguration —
// production applies a 5m cap default, above the 3m floor) still wins.
// Without that final cap clamp a 30s-cap caller would get 3m back, which
// would violate the "operators retain a global upper bound" contract.
//
// If schedule is unparseable or the period is non-positive (fixed times, DST
// edge), returns maxCap — safer to fall back to the historical single-timeout
// behaviour than to misapply a ratio to an undefined period.
func computeJobTimeout(schedule string, maxCap time.Duration) time.Duration {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return maxCap
	}
	now := time.Now()
	first := sched.Next(now)
	second := sched.Next(first)
	period := second.Sub(first)
	if period <= 0 {
		return maxCap
	}
	scaled := time.Duration(float64(period) * jobTimeoutRatio)
	if scaled < minJobTimeout {
		scaled = minJobTimeout
	}
	if scaled > maxCap {
		scaled = maxCap
	}
	return scaled
}

// validateSchedule checks if the cron expression is valid and respects the minimum interval.
func validateSchedule(schedule string) error {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return err
	}
	// Check that the interval between the first two runs is at least minCronInterval.
	now := time.Now()
	first := sched.Next(now)
	second := sched.Next(first)
	if interval := second.Sub(first); interval > 0 && interval < minCronInterval {
		return fmt.Errorf("interval %v is too short, minimum is %v", interval, minCronInterval)
	}
	return nil
}
