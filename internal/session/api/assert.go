package api

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that the concrete *session.Router satisfies the
// SessionVisitor capability published here. A VisitSessions signature
// change on Router that breaks the sysession consumer surfaces as a build
// failure in this one file rather than as silent drift. The Lookup /
// Lifecycle / Mutator / Router-union pins were removed alongside their
// interfaces in #1600 (zero consumers for over a year).
var _ SessionVisitor = (*session.Router)(nil)
