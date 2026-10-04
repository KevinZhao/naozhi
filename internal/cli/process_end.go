package cli

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// ProcessEnd is what a process knew about itself when its read loop exited:
// enough for the session to book the spend no result frame reported.
type ProcessEnd struct {
	// Detached: naozhi let go of a CLI that keeps running (Detach), so the
	// CLI did not end and owes nothing yet.
	Detached bool
	// StartedAt is when naozhi attached to the process (spawn or reattach);
	// LastResultAt is when the read loop received its last result frame,
	// zero before one; EndedAt is when the read loop exited.
	StartedAt, LastResultAt, EndedAt time.Time
	// SessionID is the CLI session id at the end, "" if none was reported.
	SessionID string
	// Shadow is the main loop's usage since the last result; empty when
	// Detached.
	Shadow clievent.ShadowUsage
}

// endHook delivers a process's ProcessEnd exactly once, to whichever
// callback is set when the end happens or, if none was, to the first one set
// after it.
type endHook struct {
	mu        sync.Mutex
	fn        func(ProcessEnd)
	end       *ProcessEnd
	delivered bool
}

// SetOnEnd sets the callback that receives this process's ProcessEnd. A
// callback set after the end receives it at once, unless an earlier one
// already did: the end is delivered once however often the hook is rebound.
func (p *Process) SetOnEnd(fn func(ProcessEnd)) {
	h := &p.endHook
	h.mu.Lock()
	h.fn = fn
	end, deliver := h.takeLocked()
	h.mu.Unlock()
	if deliver {
		fn(end)
	}
}

// fireEnd records the end and delivers it; deferred by readLoop, which exits
// once per process.
func (p *Process) fireEnd() {
	end := ProcessEnd{
		Detached:     p.detached.Load(),
		StartedAt:    p.startedAt,
		LastResultAt: p.meter.LastResultAt(),
		EndedAt:      time.Now(),
		SessionID:    p.SessionID(),
	}
	if !end.Detached {
		end.Shadow = p.meter.TakeShadow()
	}
	h := &p.endHook
	h.mu.Lock()
	h.end = &end
	end, deliver := h.takeLocked()
	fn := h.fn
	h.mu.Unlock()
	if deliver {
		fn(end)
	}
}

// takeLocked reports the end to deliver now, marking it delivered. Caller
// holds mu.
func (h *endHook) takeLocked() (ProcessEnd, bool) {
	if h.end == nil || h.fn == nil || h.delivered {
		return ProcessEnd{}, false
	}
	h.delivered = true
	return *h.end, true
}
