package dispatch

import (
	"strings"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// GroupScope is what one session of an IM group chat covers
// (session.group_scope). The zero value is GroupScopeThread.
type GroupScope string

const (
	// GroupScopeThread gives each thread or topic its own session; a message
	// outside any thread uses the chat's.
	GroupScopeThread GroupScope = "thread"
	// GroupScopeChat shares one session across the whole chat.
	GroupScopeChat GroupScope = "chat"
	// GroupScopeUser gives each member of the chat their own session.
	GroupScopeUser GroupScope = "user"
)

// sessionChatID is the chat segment of msg's session key: the chat, narrowed
// in a group chat to the thread or sender d's scope splits it by. The chat
// itself still owns the workspace, project binding and cron jobs; only the
// session, and with it /new, /stop and /urgent, is per scope.
func (d *Dispatcher) sessionChatID(msg platform.IncomingMessage) string {
	if msg.ChatType != "group" {
		return msg.ChatID
	}
	switch d.groupScope {
	case GroupScopeChat:
		return msg.ChatID
	case GroupScopeUser:
		return sessionkey.ScopedChatID(msg.ChatID, sessionkey.ScopeUser, msg.UserID)
	default:
		return sessionkey.ScopedChatID(msg.ChatID, sessionkey.ScopeThread, msg.ThreadID)
	}
}

// openThread puts a group @mention posted outside any thread into the new
// thread a reply under it opens (session.thread_auto_open), so the turn
// scopes and answers as that thread's and the follow-ups posted there join
// it. Slash commands never get here and answer where they were posted.
func (d *Dispatcher) openThread(msg platform.IncomingMessage) platform.IncomingMessage {
	if d.threadAutoOpen && msg.ChatType == "group" && msg.ThreadID == "" {
		msg.ThreadID = msg.SelfThread
	}
	return msg
}

// scopedSession reports whether key, the session msg routes to, is a
// thread's or member's own rather than one the whole chat shares (its
// unscoped session, or a project's planner).
func (d *Dispatcher) scopedSession(msg platform.IncomingMessage, key string) bool {
	chatID := d.sessionChatID(msg)
	return chatID != msg.ChatID && strings.HasPrefix(key, sessionkey.ChatKey(msg.Platform, msg.ChatType, chatID)+":")
}
