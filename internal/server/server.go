package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dashboard/discovery"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

const defaultDedupCapacity = 10000

// Server is the HTTP entry point for Naozhi.
//
// Fields used to carry a `// 读写: <files>` trailer listing every file that
// touched them, kept honest by a 215-line AST test. Both are gone (Epic I
// #2547): the trailers encoded no invariant, and `grep -rn 's.router'` answers
// the same question without going stale on every file move.
//
// The struct is intentionally flat: routes.go and the routes_snapshot_test.go
// AST contract match the `s.<handlerField>.<method>` selector shape, so the
// role-grouped dividers below are the cognitive map (#2197).
type Server struct {
	// ── HTTP entry ─────────────────────────────────────
	addr    string
	mux     *http.ServeMux
	onReady func() // called after listener is bound
	// appCtx is the process-lifetime context every background loop, the Hub
	// and the upload-store cleaner hang off. Created in buildServer (#2552) so
	// construction no longer has to wait for Start: Start links its own ctx to
	// appCancel instead of minting a second context.
	appCtx    context.Context    // HubOptions.ParentCtx
	appCancel context.CancelFunc // Start's linker + the Serve-error path
	logger    *slog.Logger

	// uploadStore holds pre-uploaded attachments until the matching send
	// consumes them; shared by the Hub (WS file_ids) and SendHandler (HTTP).
	// Built in buildDashboard, cleanup loop started in registerDashboard.
	uploadStore *uploadStore

	// ── core deps ──────────────────────────────────────
	router     serverRouter // nil interface when ServerOptions.Router is nil
	hub        *Hub         // WebSocket hub
	projectMgr *project.Manager

	// ── multi-node ─────────────────────────────────────
	nodes             *nodeRegistry // single owner of the node table; same instance as Hub.nodes
	reverseNodeServer *node.ReverseServer

	// ── dashboard / API handler groups ─────────────────
	auth       *auth.Handlers
	discoveryH *discovery.Handlers
	sessionH   *dashsession.Handlers

	// ── send / dispatch wiring ─────────────────────────
	dispatcher      *dispatch.Dispatcher // ctor builds; Start only calls BuildHandler
	dashboardToken  string
	noOutputTimeout time.Duration // timeout error messages
	totalTimeout    time.Duration

	// ── on-disk paths / caches / sysession ─────────────
	claudeDir      string
	discoveryCache *discoveryCache      // background-cached local discovery results
	scratchPool    *session.ScratchPool // ephemeral aside sessions for preview drawer

	// ── modes / resolver / node cache ──────────────────
	nodeCache *node.CacheManager // background-cached remote node data

	// shutdownComplete closes once Start's shutdown goroutine has drained
	// in-flight HTTP requests; the process shutdown sequencer blocks on it
	// before router.Shutdown(). 读写: server.go (ctor + Start + accessor)
	shutdownComplete chan struct{}

	// platforms wires each IM channel's webhook + outbound sender at
	// routes-registration time. 读写: build_dispatch.go, server.go
	platforms map[string]platform.Platform
}

// replyTagForBackend resolves a backend ID ("claude" / "kiro") to the short
// tag dispatch appends to outbound IM replies; unknown ids return "" so the
// footer is skipped. Empty id means "claude" so stores predating the Backend
// field keep their "[cc]" footer (docs/rfc/multi-backend.md §7). The
// once-guard lazily registers defaults for tests that skip main's
// backend.RegisterDefaults().
func replyTagForBackend(id string) string {
	replyTagForBackendOnce.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
	if id == "" {
		id = "claude"
	}
	if p, ok := backend.Get(id); ok {
		return p.DefaultTag
	}
	return ""
}

var replyTagForBackendOnce sync.Once

// log returns the injected component logger (ServerOptions.Logger) or
// slog.Default(). New structured logging in this package should go through it.
func (s *Server) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// RotateDashboardSessions invalidates every outstanding dashboard auth cookie
// without a restart by bumping the auth generation mixed into the cookie HMAC;
// both the HTTP cookie path and the WS upgrade path read the live MAC. Safe
// from any goroutine (#595).
func (s *Server) RotateDashboardSessions() {
	if s.auth == nil {
		return
	}
	s.auth.RotateCookieGen()
	s.log().Info("dashboard auth sessions rotated; outstanding cookies invalidated",
		"reason", "rotate_dashboard_sessions")
}

// listenTCP is the listener factory Start binds with; a package var so tests
// can inject a listener whose Accept fails post-bind.
var listenTCP = net.Listen

// Start registers routes and begins serving.
func (s *Server) Start(ctx context.Context) error {
	// Early-return error paths run before the shutdown goroutine (the sole
	// closer of shutdownComplete) exists; the process shutdown sequencer
	// blocks on ShutdownComplete() unconditionally, so close it here unless
	// the goroutine took ownership. close() must happen exactly once.
	shutdownClosed := false
	defer func() {
		if !shutdownClosed {
			close(s.shutdownComplete)
		}
	}()
	// appCancel is deferred FIRST (#2633) so every return path — including
	// the platform-start error below, which used to return before this defer
	// existed — tears down the appCtx tree (Hub, dispatcher StopCtx, loops).
	// Idempotent, so the Serve-error path's explicit call below is harmless.
	defer s.appCancel()

	// The dispatcher exists since buildServerWithHandlers; Start only turns
	// it into the platform-facing handler.
	handler := s.dispatcher.BuildHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		if a, ok := platform.AsCapability[platform.Admitter](p); ok {
			a.SetAdmission(s.dispatcher.Admit)
		}
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Roll back already-started platforms; log stop failures so a
				// dangling websocket holding the process open is visible.
				for _, sp := range startedPlatforms {
					if stopErr := sp.Stop(); stopErr != nil {
						slog.Warn("platform rollback stop failed",
							"name", sp.Name(), "err", stopErr)
					}
				}
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
			startedPlatforms = append(startedPlatforms, rp)
		}
	}

	// s.appCtx (created in buildServer) is the single cancel source for every
	// background loop AND the shutdown goroutine. Start does not mint a second
	// context; it links the caller's ctx into appCancel so SIGTERM still
	// cascades, while a srv.Serve error can cancel it directly. Without this
	// the Serve-error path would leave loops alive and discoveryCache.Wait()
	// blocked forever.
	//
	// The linker exits on either side, so it cannot outlive the Server.
	go func() {
		select {
		case <-ctx.Done():
			s.appCancel()
		case <-s.appCtx.Done():
		}
	}()

	s.startDashboardLoops()
	s.nodeCache.StartLoop(s.appCtx)
	s.discoveryCache.startLoop(s.appCtx)
	s.startProjectScanLoop(s.appCtx)
	// Token-protected dashboard over plaintext with no trusted proxy: tokens
	// and cookies are sniffable. trustedProxy=true asserts TLS terminates upstream.
	if s.dashboardToken != "" && !s.auth.TrustedProxy && isPlaintextPublicAddr(s.addr) {
		slog.Warn(plaintextDashboardTokenWarning, "addr", s.addr)
	}
	// No-auth on a publicly reachable address makes every /api/* world-reachable.
	if shouldWarnNoTokenOpen(s.dashboardToken, s.addr, s.auth.TrustedProxy) {
		slog.Warn(noTokenOpenWarning,
			"addr", s.addr,
			"trusted_proxy", s.auth.TrustedProxy,
		)
	} else if s.dashboardToken == "" {
		// Loopback + no token is the local-dev path, but an accidentally
		// cleared token must still be visible in the startup journal.
		slog.Warn("dashboard token not configured; all API callers accepted without authentication",
			"addr", s.addr,
		)
	}
	// /ws-node carries node tokens; plaintext on a public bind lets a sniffer
	// impersonate the remote node.
	if shouldWarnReverseNodePlaintext(s.reverseNodeServer != nil, s.auth.TrustedProxy, s.addr) {
		slog.Warn(reverseNodePlaintextWarning,
			"addr", s.addr,
		)
	}
	// With trustedProxy every per-IP gate trusts the last XFF hop, so the proxy
	// MUST drop-and-replace client-supplied XFF (ALB/CloudFront default; nginx
	// real_ip_recursive + allowlist) or `X-Forwarded-For: <victim>, <attacker>`
	// lands in the victim's bucket. Info level: the upstream contract is
	// unverifiable from here (#848).
	if s.auth.TrustedProxy {
		slog.Info(trustedProxyXFFReminder, "addr", s.addr)
	}
	// Effective turn timeouts are logged so operators can confirm them from
	// journalctl (#1054).
	slog.Info("server starting",
		"addr", s.addr,
		"no_output_timeout", s.noOutputTimeout,
		"total_timeout", s.totalTimeout,
	)

	ln, err := listenTCP("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	// Middleware order: withTraceID outermost so every request (auth or not)
	// carries X-Request-ID before gzip mutates the writer; withAPIVersionAlias
	// innermost so /api/v1/<rest> is rewritten just before mux matching while
	// trace-id + gzip observe the original path (#677, #425).
	srv := &http.Server{
		Handler:           withTraceID(gzipMiddleware(withAPIVersionAlias(s.mux))),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris defense
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Well below the 1 MB default so unauthenticated clients cannot force
		// megabyte header buffering; 64 KB is ample for cookies + X-Forwarded-*.
		MaxHeaderBytes: 64 * 1024,
	}

	// Notify caller that the listener is bound and ready to accept connections.
	if s.onReady != nil {
		s.onReady()
	}

	if s.sessionH != nil && s.sessionH.RetiredStorePresent() {
		go s.runRetiredStoreFlusher(s.appCtx)
	}

	// Channel allocated at construction so ShutdownComplete() may be read
	// before Start runs. From here the goroutine below is the sole closer.
	shutdownComplete := s.shutdownComplete
	shutdownClosed = true
	go func() {
		<-s.appCtx.Done()
		slog.Info("shutting down server")

		// Shutdown WebSocket hub (non-nil since buildServer, #2552)
		s.hub.Shutdown()

		if s.scratchPool != nil {
			s.scratchPool.Stop()
		}

		// Drain WarmHistoryCache before claudeDir-dependent state goes away,
		// then flush the retired-store so retirements between ticks survive.
		if s.sessionH != nil {
			s.sessionH.WaitWarmHistory()
			s.sessionH.FlushRetiredStore()
		}

		// Discovery refresh goroutine must exit before projectMgr state is torn down.
		if s.discoveryCache != nil {
			s.discoveryCache.Wait()
		}

		// Stop RunnablePlatforms (e.g. WebSocket connections)
		for _, p := range s.platforms {
			if rp, ok := p.(platform.RunnablePlatform); ok {
				if err := rp.Stop(); err != nil {
					slog.Error("stop platform", "name", p.Name(), "err", err)
				}
			}
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), session.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
		// No new requests after srv.Shutdown; drain parked takeover/close
		// goroutines before signalling completion.
		if s.discoveryH != nil {
			s.discoveryH.Wait()
		}
		close(shutdownComplete)
	}()

	err = srv.Serve(ln)
	// On a non-shutdown Serve error the parent ctx may never cancel; cancel
	// appCtx so the shutdown goroutine drains and closes shutdownComplete.
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.appCancel()
		<-shutdownComplete
		return err
	}
	// Wait for the shutdown goroutine to finish draining connections.
	select {
	case <-shutdownComplete:
	case <-s.appCtx.Done():
		<-shutdownComplete
	}
	return err
}

// ShutdownComplete returns a channel that closes once Start's shutdown
// goroutine has drained in-flight HTTP requests (or Start returned an error
// early). The process-level shutdown sequencer must block on it before
// router.Shutdown(), otherwise an in-flight handler can observe a
// half-cleaned session map. It never closes if Start is never invoked.
func (s *Server) ShutdownComplete() <-chan struct{} {
	return s.shutdownComplete
}

// retiredStoreFlushInterval bounds retirements lost on a hard kill while
// keeping fsync churn modest.
const retiredStoreFlushInterval = 30 * time.Second

// Prune cutoff is 2× the 7-day history window so entries that just left the
// popover are not raced; RetiredStore.Prune's cap handles pathological volume.
const (
	retiredStorePruneInterval = 6 * time.Hour
	retiredStorePruneCutoff   = 14 * 24 * time.Hour
)

// plaintextDashboardTokenWarning is logged when a token-protected dashboard
// is served over plaintext HTTP with no trusted proxy. Named so tests can
// pin the exact text.
const plaintextDashboardTokenWarning = "dashboard token served over plaintext HTTP with no trusted proxy: " +
	"bearer tokens and session cookies may be sniffed; authenticated /health responses " +
	"also leak workspace_id, node status, version, and watchdog counters in the clear. " +
	"Terminate TLS upstream and set server.trusted_proxy=true, " +
	"or bind to 127.0.0.1 for local-only access."

// noTokenOpenWarning is logged when dashboard_token is unset on a publicly
// reachable bind. Named so tests can pin the exact text.
const noTokenOpenWarning = "no dashboard_token configured on a non-loopback bind: " +
	"the ENTIRE dashboard API is open to any caller. " +
	"Anyone reaching this port can send messages to sessions, read workspace files under allowed_root, " +
	"alter cron schedules, and trigger transcription. Also: uploadOwner falls back to client IP, " +
	"so users sharing a NAT / LAN / egress gateway can see each other's inline uploads. " +
	"Either set server.dashboard_token, bind to 127.0.0.1 for single-user use, " +
	"or set server.trusted_proxy=true with an upstream that enforces access control."
