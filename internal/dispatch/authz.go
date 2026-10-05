package dispatch

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/turn"
)

const (
	// denyReplyWindow is how long a refused sender waits for the next
	// refusal reply; the messages in between are dropped silently.
	denyReplyWindow = 10 * time.Minute
	// denyReplyCap bounds the senders denyThrottle tracks. When it is full
	// of live entries a new sender gets no reply.
	denyReplyCap = 1024

	notAdminReply = "该命令需要管理员权限。"
)

// SetAccessPolicy replaces the IM access policy; the next message is judged
// by p. nil allows every sender.
func (d *Dispatcher) SetAccessPolicy(p *imauth.Policy) {
	d.access.Store(p)
}

// classifyCommand maps a message to the class the access policy judges. The
// admin arms mirror dispatchCommand: /cron and /project, and /cd, which
// rebind the chat or create jobs that outlive it.
func classifyCommand(trimmed string) imauth.Class {
	t := turn.Parse(trimmed).Text
	switch {
	case t == "/cron" || strings.HasPrefix(t, "/cron "),
		t == "/cd" || strings.HasPrefix(t, "/cd "),
		t == "/project" || strings.HasPrefix(t, "/project "):
		return imauth.Admin
	}
	return imauth.Chat
}

// authorize applies the access policy before any command or turn runs and
// reports whether msg may proceed. A refusal is logged at Info (the line an
// operator copies the sender ID from), counted, and answered only where that
// cannot be farmed: an allowed user's admin attempt, or a direct chat at most
// once per denyReplyWindow.
func (d *Dispatcher) authorize(ctx context.Context, msg platform.IncomingMessage, trimmed string, lg *slog.Logger) bool {
	p := d.access.Load()
	if p == nil {
		return true
	}
	class := classifyCommand(trimmed)
	ok, reason := p.Decide(msg.Platform, msg.UserID, class)
	if ok {
		return true
	}
	lg.Info("im access denied", "reason", reason, "class", class.String())
	dispatchDeniedTotal.Add(osutil.SanitizeForLog(msg.Platform, 32)+":"+reason, 1)
	switch {
	case reason == imauth.ReasonNotAdmin:
		d.replyText(ctx, msg, notAdminReply, lg)
	case reason == imauth.ReasonEmptyUser || msg.ChatType != "direct":
		// Nothing to onboard, or a group where replies would be spam.
	case d.denyReplies.allow(msg.Platform+"\x00"+osutil.SanitizeForLog(msg.UserID, 256), time.Now()):
		d.replyText(ctx, msg, denyReplyText(p, msg.UserID), lg)
	}
	return false
}

func denyReplyText(p *imauth.Policy, userID string) string {
	if p.DenyReply != "" {
		return p.DenyReply
	}
	return "你没有使用此 bot 的权限（ID: " + osutil.SanitizeForLog(userID, 128) + "），请联系管理员加入 allowed_users。"
}

// denyThrottle remembers when each refused sender was last answered.
type denyThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether key may be answered at now, recording it if so.
func (t *denyThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, seen := t.last[key]
	if seen && now.Sub(at) < denyReplyWindow {
		return false
	}
	if !seen && len(t.last) >= denyReplyCap {
		for k, at := range t.last {
			if now.Sub(at) >= denyReplyWindow {
				delete(t.last, k)
			}
		}
		if len(t.last) >= denyReplyCap {
			return false
		}
	}
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	t.last[key] = now
	return true
}
