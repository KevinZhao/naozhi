package cli

// stateEvent is something that happens to a process that can move its
// ProcessState. Every state write goes through transition with one of these,
// so the legal moves are the table in nextState and nowhere else.
type stateEvent int

const (
	// evReconnectMidTurn: SpawnReconnect found a turn still in flight.
	evReconnectMidTurn stateEvent = iota
	// evReadLoopStart: the read loop starts; a reconnect's in-flight turn
	// keeps it Running.
	evReadLoopStart
	// evSendBegin: Send takes the process for a turn.
	evSendBegin
	// evSendEnd: Send returns.
	evSendEnd
	// evTurnStarted: a turn no Send owns started (passthrough system/init).
	evTurnStarted
	// evTurnEnded: a turn no Send owns ended (the last passthrough result, or
	// the result of a turn a reconnect found in flight).
	evTurnEnded
	// evDied: the process is gone.
	evDied
)

// nextState is the state ev moves from to, and whether it moves at all.
// Dead is terminal: nothing but evDied applies to it, and that is a no-op.
func nextState(from ProcessState, ev stateEvent) (ProcessState, bool) {
	if from == StateDead {
		return from, false
	}
	switch ev {
	case evReconnectMidTurn:
		return StateRunning, from == StateSpawning
	case evReadLoopStart:
		return StateReady, from == StateSpawning
	case evSendBegin, evTurnStarted:
		return StateRunning, from == StateSpawning || from == StateReady
	case evSendEnd, evTurnEnded:
		return StateReady, from == StateRunning
	case evDied:
		return StateDead, true
	}
	return from, false
}

// transition applies ev to the turn state; see turnState.transition.
func (p *Process) transition(ev stateEvent) (prev ProcessState, moved bool) {
	return p.turn.transition(ev)
}

// die moves the process to Dead and fires onTurnDone, so a dashboard waiting
// on the turn sees it end. onTurnDone is idempotent, so a second death (panic
// recovery, then the loop's own exit) may fire it again.
func (p *Process) die() {
	p.turn.mu.Lock()
	p.turn.transitionLocked(evDied)
	cb := p.turn.onTurnDone
	p.turn.mu.Unlock()
	if cb != nil {
		cb()
	}
}
