package shim

// State-file persistence hooks for the running shim server.

import "log/slog"

// saveStateCLIDead persists the CLI-dead state to the state file.
func (s *shimServer) saveStateCLIDead() {
	s.mu.Lock()
	s.state.CLIAlive = false
	st := s.state // copy under lock
	s.mu.Unlock()
	if err := WriteStateFile(s.stateFile, st); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
}

func (s *shimServer) saveState() {
	s.mu.Lock()
	st := s.state
	st.BufferCount = s.buffer.Count()
	st.CLIAlive = s.cli.alive()
	s.mu.Unlock()
	if err := WriteStateFile(s.stateFile, st); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
}

// performHandshake runs the pre-active-client auth phase: peer-UID check,
// attach-message read under shimAuthReadDeadline + a LimitedReader (caps
// pre-auth memory), constant-time token compare, then clears the read
// deadline so the post-auth loop is not capped. On token failure it sends
// "auth_failed" so the client can surface the reason (#657).
