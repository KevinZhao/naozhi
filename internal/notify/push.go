package notify

import (
	"cmp"
	"context"
	"log/slog"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Push rules (§15.1). Constants until the Planner can change them by a
// sentence (P3).
const (
	// DailyCap is how many timely pushes a day goes out; urgent ones do not
	// count.
	DailyCap = 20
	// Quiet hours, local time: only urgent items are pushed from QuietStart
	// to QuietEnd.
	QuietStart = 22*time.Hour + 30*time.Minute
	QuietEnd   = 8 * time.Hour
)

// systemTimeout bounds one osascript run.
const systemTimeout = 10 * time.Second

// Caps on what a system notification shows.
const (
	titleRunes = 80
	bodyRunes  = 240
	// linkRunes caps the link the body ends with, leaving the rest of
	// bodyRunes for what happened.
	linkRunes = 160
)

// admitLocked decides whether an item of level is pushed now, counting a
// timely push against today's cap unless uncapped; reason says why one is
// held.
func (n *Notifier) admitLocked(level Level, uncapped bool, now time.Time) (bool, string) {
	if now.Before(n.st.PausedUntil) {
		return false, "paused"
	}
	if level == LevelUrgent {
		return true, ""
	}
	if quiet(now) {
		return false, "quiet_hours"
	}
	if uncapped {
		return true, ""
	}
	if day := now.Format(time.DateOnly); n.st.Day != day {
		n.st.Day, n.st.Pushed = day, 0
	}
	if n.st.Pushed >= DailyCap {
		return false, "daily_cap"
	}
	n.st.Pushed++
	n.saveStateLocked()
	return true, ""
}

// quiet reports whether now falls in the local quiet hours.
func quiet(now time.Time) bool {
	h, m, _ := now.Clock()
	t := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	return t >= QuietStart || t < QuietEnd
}

// pushFor builds its notification: what happened and to what.
func (n *Notifier) pushFor(it Item) Push {
	return Push{ID: it.ID, Kind: it.Kind, Level: it.Level, Title: it.Title, Body: it.Why, Unit: it.Unit}
}

// Announce pushes p once by its level (timely when unset) without an inbox
// item: a message the user reads elsewhere, as the morning brief. It counts
// against the cap and is held like an item's push, never queued, and
// reports whether it went out.
func (n *Notifier) Announce(p Push) bool {
	p.Level = cmp.Or(p.Level, LevelTimely)
	n.mu.Lock()
	ok, reason := n.admitLocked(p.Level, false, n.cfg.Clock.Now())
	n.mu.Unlock()
	d := notifyData{Event: "push", ID: p.ID, Kind: p.Kind, Level: p.Level}
	if !ok {
		d.Event, d.Reason = "held", reason
	}
	n.record(p.Unit, d)
	if ok {
		n.deliver(p)
	}
	return ok
}

// deliver sends p to both exits. The hub only queues frames; the system
// notification runs a process, on its own goroutine so a hung one holds up
// nothing. It has no click callback, so its body ends with the dashboard
// link to what it is about (linkFor), kept whole under the body cap.
func (n *Notifier) deliver(p Push) {
	if b := n.browser.Load(); b != nil {
		(*b).Notify(p)
	}
	if n.cfg.System == nil {
		return
	}
	body := p.Body
	if link := linkFor(n.cfg.Link, p); link != "" {
		// 4: the space and the "..." TruncateRunes appends.
		body = textutil.TruncateRunes(body, bodyRunes-4-utf8.RuneCountInString(link)) + " " + link
	}
	go func() {
		if err := n.cfg.System(p.Title, body); err != nil {
			slog.Warn("notify: system notification failed", "id", p.ID, "err", err)
		}
	}()
}

// linkFor is base deep-linked to p (§15.3 ④), read by notify.js at load: a
// push about a session opens it at the item's note
// (#session=<key>&item=<id>); one about no session or the Planner opens the
// Planner on the item (#planner&item=<id>). A session link over linkRunes
// falls back to the Planner on the item, whose card opens the session.
func linkFor(base string, p Push) string {
	if base == "" {
		return ""
	}
	item := ""
	if p.ID != "" {
		item = "&item=" + url.QueryEscape(p.ID)
	}
	if p.Unit != "" && p.Unit != sessionkey.PlannerMainKey {
		// ':' stays as is: a fragment may hold it, URLSearchParams reads it,
		// and session keys are full of them.
		key := strings.ReplaceAll(url.QueryEscape(p.Unit), "%3A", ":")
		if link := base + "#session=" + key + item; utf8.RuneCountInString(link) <= linkRunes {
			return link
		}
	}
	if link := base + "#planner" + item; utf8.RuneCountInString(link) <= linkRunes {
		return link
	}
	return base
}

// systemExit is the macOS notification centre on darwin, nil elsewhere.
func systemExit() func(title, body string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return func(title, body string) error {
		ctx, cancel := context.WithTimeout(context.Background(), systemTimeout)
		defer cancel()
		return exec.CommandContext(ctx, "osascript", osascriptArgs(title, body)...).Run()
	}
}

// osascriptArgs runs a fixed script that reads the title and body from its
// argv, so neither is ever parsed as AppleScript (a quote in a session name
// cannot end the string literal and run code) and no shell is involved.
// "--" ends osascript's options, so a title starting with "-" (a session
// named "-e …") is an argument, not another script line. Control
// characters are blanked and both are capped to what a notification shows.
func osascriptArgs(title, body string) []string {
	return []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--",
		clean(title, titleRunes), clean(body, bodyRunes),
	}
}

// clean blanks control and bidi-override runes and caps s at max runes.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
			return ' '
		}
		return r
	}, s)
	return textutil.TruncateRunes(strings.TrimSpace(s), max)
}

// diskFree returns the fraction of path's volume available to naozhi.
func diskFree(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 1, nil
	}
	return float64(st.Bavail) / float64(st.Blocks), nil
}
