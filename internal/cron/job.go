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

// generateID returns an 8-char hex string.
func generateID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// validateSchedule checks if the cron expression is valid.
func validateSchedule(schedule string) error {
	parser := robfigcron.NewParser(robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor)
	_, err := parser.Parse(schedule)
	return err
}
