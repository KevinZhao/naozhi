package api

import "github.com/naozhi/naozhi/internal/session"

// Compile-time pin: *session.Router satisfies SessionVisitor, so a
// VisitSessions signature change that breaks the sysession consumer
// surfaces as a build failure here rather than as silent drift.
var _ SessionVisitor = (*session.Router)(nil)
