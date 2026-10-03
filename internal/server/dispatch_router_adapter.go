package server

import (
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// Compile-time guards so method-set drift between the dispatcher's consumer
// interfaces and the types buildDispatcher hands it lands here.
var (
	_ dispatch.SessionRouter = (*session.Router)(nil)
	_ dispatch.Turns         = (*turn.Orchestrator)(nil)
	_ turn.Session           = (*session.ManagedSession)(nil)
	_ dispatch.SessionView   = (*session.ManagedSession)(nil)
	_ dispatch.KeyResolver   = (*session.KeyResolver)(nil)
)
