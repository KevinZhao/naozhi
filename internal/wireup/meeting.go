package wireup

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/meeting"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/session"
)

// MeetingDeps is what the meeting recorder is wired to.
type MeetingDeps struct {
	Enabled  bool
	Layout   datadir.Layout
	Router   *session.Router
	Promises *promise.Store
	Notifier *notify.Notifier
	Journal  *journal.Journal
	// Calendar is the scout's cached calendar; nil names meetings by program.
	Calendar func() []scout.Event
}

// OpenMeetings builds the meeting recorder, nil when it is off, not on macOS, or there is no
// state root.
func OpenMeetings(d MeetingDeps) *meeting.Manager {
	if !d.Enabled || runtime.GOOS != "darwin" || d.Layout.Root() == "" {
		return nil
	}
	root := d.Layout.PlannerMeetingsRoot()
	engine := &meeting.LocalEngine{Dir: root}
	cfg := meeting.Config{
		Root:       root,
		Helper:     meeting.ExecHelper{Path: meeting.HelperPath()},
		Recognizer: engine,
		Diarizer:   engine,
		Summarize:  MeetingSession(d.Router, d.Layout.PlannerScoutWorkDir()),
	}
	if d.Calendar != nil {
		cfg.Calendar = func() []meeting.CalendarEvent {
			var out []meeting.CalendarEvent
			for _, e := range d.Calendar() {
				out = append(out, meeting.CalendarEvent{Title: e.Title, Start: e.Start, End: e.End, Organizer: e.Organizer, Attendees: e.Attendees})
			}
			return out
		}
	}
	if d.Promises != nil {
		cfg.Promise = func(text string, due time.Time) (string, error) {
			// Source user: the user's own to-do from their meeting, not something the Planner
			// took on. The close condition keeps it open past its reminder (a pure reminder
			// closes itself once it reminded).
			p, err := d.Promises.Add(promise.Promise{Text: text, Due: due, Source: promise.SourceUser, CloseWhen: "你做完这件事"})
			return p.ID, err
		}
	}
	if d.Notifier != nil {
		cfg.Notify = func(key, title, body string) {
			d.Notifier.Add(notify.Item{Kind: notify.KindNotice, Level: notify.LevelTimely, Uncapped: true, Source: key, Title: title, Why: body})
		}
	}
	if d.Journal != nil {
		cfg.Journal = func(data map[string]any) {
			b, err := json.Marshal(data)
			if err != nil {
				return
			}
			_ = d.Journal.Append(journal.Entry{Kind: journal.KindActivity, Unit: meeting.UnitID, Data: b})
		}
	}
	return meeting.New(cfg)
}

// meetingSessionTools is the summary session's whole tool list: the transcript is in its
// message, Read is there for nothing else to be.
var meetingSessionTools = []string{"Read"}

// MeetingSession is meeting.Config.Summarize over the session router: a fresh one-shot session
// (meeting:<id>) with no MCP server and only Read, allowed under dontAsk, run in workDir (the
// scout's, whose CLI transcripts the history popover hides), its one turn judged like a cron
// run's, then reset.
func MeetingSession(router *session.Router, workDir string) meeting.Session {
	return func(ctx context.Context, key, _ string, text string) (string, error) {
		if router == nil {
			return "", errors.New("no session router")
		}
		opts := session.AgentOpts{
			Backend:             "claude",
			Workspace:           workDir,
			Exempt:              true,
			Tools:               meetingSessionTools,
			PermissionMode:      "dontAsk",
			StrictMCPConfig:     true,
			CrossSessionInbound: "hold",
			PermissionsAllow:    meetingSessionTools,
		}
		sess, _, err := router.GetOrCreate(ctx, key, opts)
		if err != nil {
			return "", err
		}
		defer router.Reset(key)
		res, err := cronSessionAdapter{sess}.Send(ctx, text)
		if ctx.Err() != nil {
			sess.Interrupt()
		}
		if err == nil && res.Text == "" {
			err = errors.New("empty reply")
		}
		return res.Text, err
	}
}
