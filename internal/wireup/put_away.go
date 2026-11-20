package wireup

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// PutAwayRecord reads the event log of a dispatched session that was put
// away (planner/reap): out of the router, so GET /api/sessions/events has no
// session to ask, while its log is kept for the day it is reopened. The 任务
// card of a done task opens on it (docs/rfc/planner.md D136). Only a key
// whose sidecar records a put away is read: any other key missing from the
// router has no session, and stays a 404.
type PutAwayRecord struct {
	Sidecars *sidecar.Store
	// EventsDir is the event-log directory (datadir.Layout.EventsRoot).
	EventsDir string
}

// Events is the page of key's record before beforeMS (0 for the latest), at
// most limit entries, and whether older ones remain. ok is false when key
// is not a session put away.
func (p PutAwayRecord) Events(ctx context.Context, key string, beforeMS int64, limit int) (entries []clievent.EventEntry, more, ok bool) {
	if p.Sidecars == nil || p.EventsDir == "" {
		return nil, false, false
	}
	if m, found := p.Sidecars.Get(key); !found || m.Reap == nil {
		return nil, false, false
	}
	src := naozhilog.New(p.EventsDir, key)
	// One more than asked says whether an older page remains.
	var err error
	if beforeMS > 0 {
		entries, err = src.LoadBefore(ctx, beforeMS, limit+1)
	} else {
		entries, err = src.LoadLatest(ctx, limit+1)
	}
	if err != nil {
		// A record that cannot be read is an empty one, not a missing
		// session: the pane says there is nothing rather than load forever.
		return nil, false, true
	}
	if len(entries) > limit {
		entries, more = entries[len(entries)-limit:], true
	}
	return entries, more, true
}
