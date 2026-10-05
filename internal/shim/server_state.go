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
	// Once shutdown began the state file is Run's to remove; a departing
	// client's late write would outlive this shim and could clobber the
	// state file of the shim respawned on the same key.
	select {
	case <-s.done:
		return
	default:
	}
	s.mu.Lock()
	st := s.state
	st.BufferCount = s.buffer.Count()
	st.CLIAlive = s.cli.alive()
	s.mu.Unlock()
	if err := WriteStateFile(s.stateFile, st); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
}
