// Package session_test — contract_test.go
//
// Cross-package compile-time assertion that *session.Router satisfies
// each downstream consumer's SessionRouter-shaped interface. The test
// body is empty because satisfaction is verified at package-compile
// time by the `var _ ... = (*session.Router)(nil)` declarations.
//
// Signature drift scenario this catches: a Router method adds an
// argument (say, GetOrCreate gains an options struct). Without this
// file, the change compiles in the session package; dispatch's
// internal SessionRouter interface still lists the old signature;
// *session.Router no longer satisfies it, and dispatch/server/upstream
// each fail to build in isolation. This file brings that failure to
// CI in a single place so a reviewer gets one pointed error instead
// of three scattered ones.
//
// This file MUST live in the session_test package (not session) to
// avoid an import cycle — dispatch, server, upstream all import
// session, so session cannot import them in production code. Test
// packages may reverse-import safely.
package session_test

import (
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/upstream"
)

// Enforce *session.Router satisfies every consumer's interface. All
// three consumers from docs/rfc/consumer-interfaces.md are covered;
// any new Router method signature drift surfaces here as a single
// CI failure instead of three scattered ones.
var (
	_ dispatch.SessionRouter = (*session.Router)(nil)
	_ server.HubRouter       = (*session.Router)(nil)
	_ upstream.SessionRouter = (*session.Router)(nil)
)
