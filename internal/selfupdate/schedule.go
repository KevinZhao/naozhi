package selfupdate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// savedSchedule is the on-disk form of Run's schedule. Without it every
// restart started the interval over, so an instance restarted more often
// than the interval (upgrades, config edits, a crash loop) never checked.
type savedSchedule struct {
	NextCheck time.Time `json:"next_check"`
	Failures  int       `json:"failures,omitempty"`
}

// loadSchedule reads the saved schedule; ok is false when there is none or
// it cannot be read, and Run then schedules as if freshly installed.
func (c *Checker) loadSchedule() (savedSchedule, bool) {
	if c.cfg.StatePath == "" {
		return savedSchedule{}, false
	}
	data, err := os.ReadFile(c.cfg.StatePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("auto-update: read saved schedule failed", "path", c.cfg.StatePath, "err", err)
		}
		return savedSchedule{}, false
	}
	var s savedSchedule
	if err := json.Unmarshal(data, &s); err != nil || s.NextCheck.IsZero() || s.Failures < 0 {
		slog.Warn("auto-update: saved schedule unreadable, starting over", "path", c.cfg.StatePath, "err", err)
		return savedSchedule{}, false
	}
	return s, true
}

// saveSchedule records the next check best-effort: a write failure only
// costs the schedule surviving a restart.
func (c *Checker) saveSchedule(next time.Time, failures int) {
	if c.cfg.StatePath == "" {
		return
	}
	data, err := json.Marshal(savedSchedule{NextCheck: next.UTC(), Failures: failures})
	if err != nil {
		return
	}
	if err := osutil.WriteFileAtomic(c.cfg.StatePath, data, 0o600); err != nil {
		slog.Warn("auto-update: save schedule failed", "path", c.cfg.StatePath, "err", err)
	}
}
