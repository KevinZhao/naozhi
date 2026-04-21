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
