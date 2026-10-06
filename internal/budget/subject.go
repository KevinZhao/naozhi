// Package budget answers "how much has this cron job, or the whole machine,
// spent today" from an in-memory index fed by the cost ledger, and turns it
// into a verdict against daily USD limits. It imports only costledger and
// sessionkey, so costledger stays a leaf.
package budget

import (
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Subject names what a daily limit applies to: "job:<id>" or Global.
type Subject string

// Global is the subject every USD entry counts toward.
const Global Subject = "global"

const jobPrefix = "job:"

// JobSubject is the subject of cron job id.
func JobSubject(id string) Subject { return Subject(jobPrefix + id) }

// SubjectForKey maps a router session key to its scoped subject: a cron key
// to its job. Every other key (IM, planner, dashboard, takeover, sys,
// scratch) has none ("") and counts only toward Global.
func SubjectForKey(key string) Subject {
	if sessionkey.IsCronKey(key) {
		if id := sessionkey.CronJobIDFromKey(key); id != "" {
			return JobSubject(id)
		}
	}
	return ""
}

// subjectFor is e's scoped subject: its job when it carries one (a cron
// run's rows, or a session on a cron key), else its key's.
func subjectFor(e costledger.Entry) Subject {
	if e.JobID != "" {
		return JobSubject(e.JobID)
	}
	return SubjectForKey(e.SessionKey)
}
