package custody

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Watching (§15.12 ⑥, D104) is a second switch beside custody: the gateway
// observing a session the user opened — its digest, its card in 任务's
// "你开的", a result marked when a turn ends with the user away. It sends
// nothing and changes no inbox policy, so unlike custody it is on by
// default. It lives here because it answers to the same "this node's
// session, the Planner on" and the same "你在旁边" as custody.

// watchable reports whether key is one the gateway may observe, as a key:
// not the Planner's, a scout run's or a daemon's (eligible refuses those),
// not a dispatched session (a task already) nor a cron run or a scratch
// session (one-shot).
func watchable(key string) bool {
	return !sessionkey.IsTaskKey(key) && !sessionkey.IsCronKey(key) && !sessionkey.IsScratchKey(key)
}

// watched is a watchable session's switch: as someone set it, else on.
func watched(m sidecar.Meta, ok bool) bool {
	if ok && m.Watch != nil {
		return m.Watch.On
	}
	return true
}

// Watched reports whether the gateway observes key: one of this node's
// sessions the user opened (eligible and watchable), its switch not turned
// off. Off with the Planner off, as custody is: nothing reads what
// observing makes then.
func (e *Engine) Watched(key string) bool {
	if e.eligible(key) != nil || !watchable(key) {
		return false
	}
	if e.cfg.Sessions == nil {
		return true
	}
	m, ok := e.cfg.Sessions.Get(key)
	return watched(m, ok)
}

// SetWatch turns observing key on or off for by. A switch someone other
// than the user flipped (the Planner's custody block) is noted in the
// session, as custody's own is.
func (e *Engine) SetWatch(key string, on bool, by string) error {
	if e.cfg.Sessions == nil {
		return ErrNoSession
	}
	if err := e.eligible(key); err != nil {
		return err
	}
	if !watchable(key) {
		return ErrRefused
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureLocked(key); err != nil {
		return err
	}
	now := e.now()
	changed := false
	if _, err := e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		changed = watched(*m, true) != on
		m.Watch = &sidecar.Custody{On: on, By: by, At: now}
		return true
	}); err != nil {
		return err
	}
	if changed && by != ByUser {
		event := "watch_off"
		if on {
			event = "watch_on"
		}
		e.record(key, note.CustodyData{Event: event, By: by})
	}
	return nil
}

// Ended is told of each session's turn end (the router's result sink). A
// watched session's turn that ended with the user away from it (custody's
// "你在旁边": viewing it, or sent it a message within NearbyFor) and no
// question left for them is a result: its sidecar's ResultAt, the task
// card's blue dot until they look. One that ended with them at it is
// looked at already: a result pending is marked seen. A question left open
// is 等你 (the health machine's), not a result.
func (e *Engine) Ended(key string) {
	if e.cfg.Sessions == nil || !e.Watched(key) {
		return
	}
	asking := e.cfg.Asking != nil && e.cfg.Asking(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var err error
	switch {
	case e.nearbyLocked(key):
		// Most sessions the user is at have no sidecar and nothing to mark.
		_, err = e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
			if !m.ResultAt.After(m.SeenAt) {
				return false
			}
			m.SeenAt = now
			return true
		})
	case asking:
		return
	default:
		err = e.cfg.Sessions.Upsert(key, func(m *sidecar.Meta) bool {
			m.ResultAt = now
			return true
		})
	}
	if err != nil {
		slog.Warn("custody: watched session's result not saved", "key", key, "err", err)
	}
}
