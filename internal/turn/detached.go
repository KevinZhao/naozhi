package turn

import "context"

// runDetached runs r as its own turn outside the owner loop: a passthrough
// send the CLI's command queue orders, or a PriorityNow preemption. It never
// touches the Queue except on panic, where — like an owner-loop panic — it
// discards key's queue, and it does not call NotifyIdle, since it never held
// the key.
func (o *Orchestrator) runDetached(ctx context.Context, r Request) {
	t := &inflight{first: r.Priority == PriorityNormal}
	if r.Origin != nil {
		t.receivers = []*receiver{{origin: r.Origin, info: TurnInfo{Role: RoleHead, First: t.first, Merged: 1}}}
	}
	defer func() {
		if rec := recover(); rec != nil {
			o.recovered(ctx, r.Key, r.Origin, t, rec)
		}
	}()
	o.runTurn(ctx, r.Key, t, sessionOpts(r.Origin, r.Key), r.Text, r.Images, SendSpec{Passthrough: true, Priority: r.Priority})
}
