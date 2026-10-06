// Package budget answers "how much has this chat, project, cron job or the
// whole machine spent today" from an in-memory index fed by the cost ledger,
// and turns it into a verdict against daily USD limits. IM dispatch and the
// cron scheduler consult it before a turn or a run. It imports only
// costledger and sessionkey, so costledger stays a leaf.
package budget

import (
	"strings"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Subject names what a daily limit applies to: "chat:<platform:type:id>",
// "project:<name>", "job:<id>" or Global.
type Subject string

// Global is the subject every USD entry counts toward.
const Global Subject = "global"

const (
	chatPrefix    = "chat:"
	projectPrefix = "project:"
	jobPrefix     = "job:"
)

// takeoverPlatform is the platform segment of sessionkey.TakeoverKey.
const takeoverPlatform = "local"

// JobSubject is the subject of cron job id.
func JobSubject(id string) Subject { return Subject(jobPrefix + id) }

// Kind is s's scope: "chat", "project", "job" or "global"; "" for the empty
// subject.
func (s Subject) Kind() string {
	if s == Global {
		return string(Global)
	}
	kind, _, ok := strings.Cut(string(s), ":")
	if !ok {
		return ""
	}
	return kind
}

// Name is s without its scope: the chat key, project name or job id.
func (s Subject) Name() string {
	_, name, _ := strings.Cut(string(s), ":")
	return name
}

// SubjectForKey maps a router session key to its scoped subject: a cron key
// to its job, a planner key to its project (shared by every chat bound to
// it), an IM key (platform:chatType:chatID:agent) to its chat across agents
// and threads (sessionkey.ParentChatKey).
// Dashboard, takeover, sys and scratch keys have none ("") and count only
// toward Global: the dashboard user is the owner and is never gated.
func SubjectForKey(key string) Subject {
	switch {
	case sessionkey.IsCronKey(key):
		if id := sessionkey.CronJobIDFromKey(key); id != "" {
			return JobSubject(id)
		}
		return ""
	case sessionkey.IsPlannerKey(key):
		return Subject(projectPrefix + sessionkey.PlannerNameFromKey(key))
	}
	if strings.Count(key, ":") != 3 {
		return ""
	}
	platform, _, _ := strings.Cut(key, ":")
	if platform == sessionkey.DashboardPlatform || platform == takeoverPlatform {
		return ""
	}
	return Subject(chatPrefix + sessionkey.ParentChatKey(key[:strings.LastIndexByte(key, ':')]))
}

// subjectFor is e's scoped subject: its job when it carries one (a cron
// run's rows, or a session on a cron key), else its key's.
func subjectFor(e costledger.Entry) Subject {
	if e.JobID != "" {
		return JobSubject(e.JobID)
	}
	return SubjectForKey(e.SessionKey)
}
