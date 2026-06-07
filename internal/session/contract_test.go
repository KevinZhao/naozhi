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
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/upstream"
)

// Enforce *session.Router satisfies every consumer's interface. The
// dispatch / server / upstream consumers from
// docs/rfc/consumer-interfaces.md are covered here so any Router
// signature drift surfaces in one CI failure instead of three.
//
// cron.SessionRouter is INTENTIONALLY not pinned: post Phase B
// (docs/rfc/cron-sysession-merge.md §3.3) the cron interface speaks
// in cron-local types (cron.AgentOpts / cron.Session) rather than
// session.AgentOpts / *session.ManagedSession, so *session.Router no
// longer satisfies it directly. The cmd/naozhi adapter
// (cronRouterAdapter) bridges the two; the round-trip test for that
// adapter lives in internal/wireup/cron_router_adapter_test.go.
var (
	_ dispatch.SessionRouter = (*session.Router)(nil)
	_ server.HubRouter       = (*session.Router)(nil)
	_ upstream.SessionRouter = (*session.Router)(nil)
)

// dispatch.ProjectStore is the other consumer interface dispatch declares
// (ARCH-DISP-1 #457). consumer.go relied on a runtime assignment in
// NewDispatcher to catch *project.Manager drift; pin it here too so a
// project.Manager signature change surfaces as one CI compile error
// alongside the SessionRouter pins instead of only at wiring time.
// R260528-ARCH-21 (#1380): consumer-interface drift guard.
var _ dispatch.ProjectStore = (*project.Manager)(nil)

var _ = cron.SessionRouter(nil) // keep cron import alive for godoc cross-ref
