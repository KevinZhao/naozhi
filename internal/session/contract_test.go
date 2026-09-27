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
// dispatch / server consumers from
// docs/rfc/consumer-interfaces.md are covered here so any Router
// signature drift surfaces in one CI failure instead of three.
//
// cron.SessionRouter, upstream.SessionRouter and dispatch.SessionRouter are
// INTENTIONALLY not pinned: each speaks in its own session type
// (cron.Session / upstream.Session / dispatch.Session) rather than
// *session.ManagedSession, so *session.Router does not satisfy them directly.
// The adapters are pinned where they live (wireup's cron_router_adapter.go and
// upstream_router.go, server's dispatch_router_adapter.go).
var _ server.HubRouter = (*session.Router)(nil)

// dispatch.ProjectStore is the other consumer interface dispatch declares
// (ARCH-DISP-1 #457). consumer.go relied on a runtime assignment in
// NewDispatcher to catch *project.Manager drift; pin it here too so a
// project.Manager signature change surfaces as one CI compile error
// alongside the SessionRouter pins instead of only at wiring time.
// R260528-ARCH-21 (#1380): consumer-interface drift guard.
var _ dispatch.ProjectStore = (*project.Manager)(nil)

var _ = cron.SessionRouter(nil) // keep cron import alive for godoc cross-ref

var _ = upstream.SessionRouter(nil) // keep upstream import alive for godoc cross-ref

var _ = dispatch.SessionRouter(nil) // keep dispatch import alive for godoc cross-ref
