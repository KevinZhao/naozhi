package wireup

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/meeting"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/workspace"
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
	// Workspaces are the registered workspaces the summary looks into for context.
	Workspaces *workspace.Registry
	// ReadRules is the Planner's read scope (allow, deny): the summary session reads the same
	// trees with the same credentials kept out. nil leaves it the transcript alone.
	ReadRules func() (allow, deny []string)
}

// OpenMeetings builds the meeting recorder, nil when it is off, not on macOS, or there is no
// state root.
func OpenMeetings(d MeetingDeps) *meeting.Manager {
	if !d.Enabled || runtime.GOOS != "darwin" || d.Layout.Root() == "" {
		return nil
	}
	root := d.Layout.PlannerMeetingsRoot()
	helper := meeting.ExecHelper{Path: meeting.HelperPath(), ModelsDir: filepath.Join(root, ".models", "fluidaudio")}
	cfg := meeting.Config{
		Root:       root,
		Helper:     helper,
		Recognizer: &meeting.LocalEngine{Dir: root},
		Diarizer:   helper,
		Summarize:  MeetingSession(d.Router, d.Layout.PlannerScoutWorkDir(), d.ReadRules),
	}
	if d.ReadRules != nil {
		cfg.Clues = func() meeting.Clues {
			c := meeting.Clues{PlannerDir: d.Layout.PlannerRoot()}
			for _, w := range d.Workspaces.LocalDirs() {
				c.Workspaces = append(c.Workspaces, meeting.ClueWorkspace{Name: w.Name, Path: w.Path})
			}
			return c
		}
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

// meetingSessionTools is the summary session's whole tool list: reading only, for the projects
// the meeting touches (the transcript itself is in its message).
var meetingSessionTools = []string{"Read", "Grep", "Glob"}

// MeetingSession is meeting.Config.Summarize over the session router: a fresh one-shot session
// (meeting:<id>) with no MCP server and only the read tools under dontAsk, reading what the
// Planner may read (rules; Read rules cover Grep and Glob) and refused the rest, run in workDir
// (the scout's, whose CLI transcripts the history popover hides), its one turn judged like a
// cron run's, then reset.
func MeetingSession(router *session.Router, workDir string, rules func() (allow, deny []string)) meeting.Session {
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
		}
		if rules != nil {
			opts.PermissionsAllow, opts.PermissionsDeny = rules()
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
