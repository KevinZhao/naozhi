package wireup

import (
	"cmp"
	"net"
	"strings"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// unitNameRunes caps a unit's name in an inbox item or a push.
const unitNameRunes = 40

// UnitNames is notify.Config.Name: a cron job's title, a session's label or
// summary; "" when neither knows the unit.
func UnitNames(router *session.Router, sched *cron.Scheduler) func(unit string) string {
	return func(unit string) string {
		var name string
		if sessionkey.IsCronKey(unit) && sched != nil {
			if j, ok := sched.GetJob(sessionkey.CronJobIDFromKey(unit)); ok {
				name = j.Title
			}
		}
		if name == "" && router != nil {
			if s := router.SessionFor(unit); s != nil {
				snap := s.Snapshot()
				name = cmp.Or(snap.UserLabel, snap.Summary)
			}
		}
		return textutil.TruncateRunes(strings.Join(strings.Fields(name), " "), unitNameRunes)
	}
}

// DashboardLink is the dashboard URL on this machine for the listen address
// addr (":8180", "0.0.0.0:8180", "127.0.0.1:8180").
func DashboardLink(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/dashboard"
}
