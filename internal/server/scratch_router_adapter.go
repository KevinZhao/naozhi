package server

import (
	"github.com/naozhi/naozhi/internal/dashboard/ext/scratch"
	"github.com/naozhi/naozhi/internal/session"
)

// Compile-time guards so method-set drift lands here, next to the adapter.
var (
	_ scratch.ScratchRouter = scratchRouter{}
	_ scratch.SourceSession = (*session.ManagedSession)(nil)
)

// scratchRouter adapts the hub's router to scratch.ScratchRouter, so the
// scratch handler never names the concrete session type. Remove and
// RenameSession are promoted.
type scratchRouter struct{ HubRouter }

// SessionFor converts a missing session to a nil interface: a nil
// *ManagedSession inside a non-nil interface would pass the handler's
// "source session not found" check and be dereferenced.
func (r scratchRouter) SessionFor(key string) scratch.SourceSession {
	if s := r.HubRouter.SessionFor(key); s != nil {
		return s
	}
	return nil
}
