// Package imauth decides whether an IM sender may use the bot: a per-platform
// allowlist plus an admin tier for commands that change shared state. It is a
// leaf (standard library only) so every inbound surface that needs the same
// "who may do what" answer can import it without reaching into config or
// dispatch.
package imauth

// Class is what a message asks the bot to do, as far as authorization cares.
type Class int

const (
	// Chat covers plain messages, agent commands and the per-chat turn
	// commands (/new, /stop, ...).
	Chat Class = iota
	// Admin covers commands that rebind the chat or create persistent jobs.
	Admin
)

// String returns the class name used in logs.
func (c Class) String() string {
	if c == Admin {
		return "admin"
	}
	return "chat"
}

// Deny reasons returned by Decide. They are stable metric-key fragments.
const (
	ReasonNotAllowed  = "not_allowed"
	ReasonNotAdmin    = "not_admin"
	ReasonEmptyUser   = "empty_user"
	ReasonDefaultDeny = "default_deny"
)

// Rule is one platform's lists, keyed by the platform's sender ID.
type Rule struct {
	// Allowed may chat. Admins may chat too.
	Allowed map[string]struct{}
	// Admins may run Admin-class commands. Empty makes every chat-allowed
	// user an admin, so enabling an allowlist alone removes no command.
	Admins map[string]struct{}
}

// Policy is the whole IM access configuration. A nil *Policy allows
// everything.
type Policy struct {
	// DefaultDeny refuses every sender on a platform that has no Rule.
	DefaultDeny bool
	// DenyReply overrides the direct-chat refusal text; empty uses the
	// caller's default.
	DenyReply string
	// Rules is keyed by platform name ("feishu", "slack", ...).
	Rules map[string]Rule
}

// Decide reports whether userID on platform may send a message of class c.
// On false, reason is one of the Reason* constants. IDs compare exactly.
func (p *Policy) Decide(platform, userID string, c Class) (ok bool, reason string) {
	if p == nil {
		return true, ""
	}
	rule, has := p.Rules[platform]
	if !has {
		if p.DefaultDeny {
			return false, ReasonDefaultDeny
		}
		return true, ""
	}
	if userID == "" {
		return false, ReasonEmptyUser
	}
	_, admin := rule.Admins[userID]
	_, allowed := rule.Allowed[userID]
	if !admin && !allowed {
		return false, ReasonNotAllowed
	}
	if c == Admin && !admin && len(rule.Admins) > 0 {
		return false, ReasonNotAdmin
	}
	return true, ""
}
