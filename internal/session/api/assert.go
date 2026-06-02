package api

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertions that the concrete *session.Router satisfies
// every capability interface published here. A method-signature change
// on Router that breaks any consumer surfaces as a build failure in this
// one file, rather than as silent interface drift across the five
// consumer packages.
var (
	_ SessionLookup    = (*session.Router)(nil)
	_ SessionLifecycle = (*session.Router)(nil)
	_ SessionMutator   = (*session.Router)(nil)
	_ SessionVisitor   = (*session.Router)(nil)
	_ SessionRouter    = (*session.Router)(nil)
)
