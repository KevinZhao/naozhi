package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
)

// TriggerRetry is a run that goes again for a waiting one once the Midway
// session is renewed (§15.14 ⑦).
const TriggerRetry = "retry"

// briefGiveUp is how long before its meeting a brief waiting for the
// credential gives up: a brief that late is no use.
const briefGiveUp = 5 * time.Minute

// errCredStill heads the error of a run failed for the credential the last
// run already waited on.
const errCredStill = "凭证仍然失效，上次巡检以来 Midway 没有续过"

// credWait is a run waiting for the credential: it goes again once the
// Midway session expires later than cred, until the next scheduled slot,
// which reads the same span anyway.
type credWait struct {
	run   string
	cred  time.Time
	until time.Time
}

// credExpiry is the Midway session's expiry now, zero with none to watch.
func (s *Scout) credExpiry() time.Time {
	if s.cfg.CredExpiry == nil {
		return time.Time{}
	}
	return s.cfg.CredExpiry()
}

// credential reports whether err is the model call finding the credential
// expired or missing, the class custody waits on (health.ErrCredentialExpired),
// exp being the Midway session's expiry read before the session started: a
// renewal while the failing session still ran must count as one. False
// with no Midway session to watch: nothing would tell a renewal, so the
// failure stands.
func credential(err error, exp time.Time) bool {
	return err != nil && !exp.IsZero() && health.Classify(err.Error()) == health.ErrCredentialExpired
}

// renewed reports whether the Midway session was renewed since a failure
// that saw it expire at cred: it expires later now, and not yet.
func (s *Scout) renewed(cred, now time.Time) bool {
	exp := s.credExpiry()
	return exp.After(cred) && exp.After(now)
}

// waitedOn reports whether the last run already waited for the credential
// with the Midway session expiring at exp, not renewed since. A second run
// failing so is failed: waiting again would hide, run after run, a
// credential that renewing Midway does not fix.
func (s *Scout) waitedOn(exp time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last != nil && s.last.State == StateWaiting && !exp.After(s.last.Cred)
}

// waitLocked records r, a run just ended, as the one to go again or, any
// other outcome, clears the wait: a run that went through, or failed for another
// cause, leaves nothing to retry. Called under mu.
func (s *Scout) waitLocked(r Run) {
	if r.State != StateWaiting {
		s.wait = nil
		return
	}
	s.wait = &credWait{run: r.ID, cred: r.Cred, until: nextSlot(r.Started)}
}

// checkRetry starts the retry of the waiting run once the credential is
// renewed, and drops the wait at the next slot. Only Run calls it.
func (s *Scout) checkRetry(now time.Time) {
	s.mu.Lock()
	w := s.wait
	if w != nil && !now.Before(w.until) {
		slog.Info("scout: credential not renewed before the next slot; that run reads the span instead", "run", w.run)
		s.wait, w = nil, nil
	}
	s.mu.Unlock()
	if w == nil || !s.renewed(w.cred, now) {
		return
	}
	if _, err := s.startRetry(w.run); err != nil && !errors.Is(err, ErrBusy) {
		slog.Warn("scout: retry not started", "run", w.run, "err", err)
	}
}

// nextSlot is the first scheduled slot after t.
func nextSlot(t time.Time) time.Time {
	for d := 0; d < 8; d++ {
		day := t.AddDate(0, 0, d)
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		for _, hm := range slots {
			if s := time.Date(day.Year(), day.Month(), day.Day(), hm[0], hm[1], 0, 0, t.Location()); s.After(t) {
				return s
			}
		}
	}
	return t.Add(7 * 24 * time.Hour) // unreachable: every week has a weekday
}

// Timeout is what a session cut short by its time limit was doing: how long
// the run had gone, the tool calls in flight, oldest first, each with how
// long it had run, and how long the session had sent nothing. No tool in
// flight is the model's own wait.
type Timeout struct {
	ElapsedSec int           `json:"elapsed_s"`
	Tools      []TimeoutTool `json:"tools,omitempty"`
	QuietSec   int           `json:"quiet_s"`
}

// TimeoutTool is a tool call in flight at a timeout.
type TimeoutTool struct {
	Tool       string `json:"tool"`
	ElapsedSec int    `json:"elapsed_s"`
}

// TimeoutError is a Session's error for a session its ctx's deadline cut
// short, with what it was doing then (ElapsedSec left for the caller).
type TimeoutError struct {
	Timeout
	Err error
}

func (e *TimeoutError) Error() string { return e.Timeout.describe() + ": " + e.Err.Error() }
func (e *TimeoutError) Unwrap() error { return e.Err }

// describe is the timeout as the run record's error says it.
func (t Timeout) describe() string {
	var b strings.Builder
	b.WriteString("超时")
	if t.ElapsedSec > 0 {
		fmt.Fprintf(&b, "（已用 %s）", seconds(t.ElapsedSec))
	}
	if len(t.Tools) == 0 {
		fmt.Fprintf(&b, "：没有工具在运行，模型 %s 没有输出", seconds(t.QuietSec))
		return b.String()
	}
	b.WriteString("：正在调用 ")
	for i, tt := range t.Tools {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s（%s）", strings.TrimPrefix(tt.Tool, "mcp__"), seconds(tt.ElapsedSec))
	}
	return b.String()
}

func seconds(n int) string {
	if n < 60 {
		return fmt.Sprintf("%d 秒", n)
	}
	return fmt.Sprintf("%d 分 %d 秒", n/60, n%60)
}

// timedOut is err's timeout with the run's elapsed time, nil for an error
// that is no timeout.
func timedOut(err error, elapsed time.Duration) *Timeout {
	if !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var t Timeout
	if te, ok := errors.AsType[*TimeoutError](err); ok {
		t = te.Timeout
	}
	t.ElapsedSec = int(elapsed / time.Second)
	return &t
}
