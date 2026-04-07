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

	// Last execution result, persisted across restarts.
	LastResult string    `json:"last_result,omitempty"`
	LastRunAt  time.Time `json:"last_run_at,omitempty"`
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

// validateSchedule checks if the cron expression is valid.
func validateSchedule(schedule string) error {
	_, err := cronParser.Parse(schedule)
	return err
}
