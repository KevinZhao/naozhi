package server

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewHub_ParentCtxCancelPropagates locks the CTX1 contract: cancelling
// the parent context threaded via HubOptions.ParentCtx must cause h.ctx
// to become Done even when Shutdown() is never invoked. This closes the
// gap where a panic-early-exit path in main would forget to call Shutdown
// and leak send/push goroutines that observe h.ctx.Done().
func TestNewHub_ParentCtxCancelPropagates(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	hub := NewHub(HubOptions{
		Router:    router,
		Guard:     guard,
		ParentCtx: parent,
	})

	// Pre-cancel sanity: hub context must not be Done yet.
	select {
	case <-hub.ctx.Done():
		t.Fatal("hub.ctx is Done before parent cancel — derivation broken")
	default:
	}

	cancel()

	select {
	case <-hub.ctx.Done():
		// expected: parent-ctx cancel cascaded to h.ctx via WithCancel.
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done within 2s of parent cancel")
	}

	if err := hub.ctx.Err(); err != context.Canceled {
		t.Errorf("hub.ctx.Err() = %v, want context.Canceled", err)
	}

	// Shutdown must still be callable (h.cancel is idempotent) and must
	// not hang. We drive it through a goroutine bounded by a timer so a
	// regression that re-introduces a blocking Shutdown fails loudly.
	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub.Shutdown() hung after parent-ctx cancel")
	}
}

// TestNewHub_NilParentCtxFallsBackToBackground preserves the legacy
// behaviour for call sites that do not thread a parent ctx (tests and
// headless wiring). A nil ParentCtx must not panic and must yield a
// usable hub whose ctx is only Done after Shutdown.
func TestNewHub_NilParentCtxFallsBackToBackground(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	hub := NewHub(HubOptions{
		Router: router,
		Guard:  guard,
		// ParentCtx intentionally omitted.
	})

	select {
	case <-hub.ctx.Done():
		t.Fatal("hub.ctx unexpectedly Done on a background-derived hub")
	default:
	}

	hub.Shutdown()

	select {
	case <-hub.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done after explicit Shutdown")
	}
}

// TestNewHub_DerivesCtxFromOptsParentCtx is a source-level contract test
// that locks NewHub's derivation shape so a future refactor cannot silently
// revert to context.Background() and re-open the CTX1 gap. It reads wshub.go
// and asserts on the specific derivation idiom. Complements the behavioural
// test above — a regression that kept parent plumbing but dropped the
// WithCancel derivation would pass the behavioural test if parent was
// cancelled before Shutdown, but fail the behavioural guarantee in general.
func TestNewHub_DerivesCtxFromOptsParentCtx(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "wshub.go")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	body := string(raw)

	// Must derive from opts.ParentCtx with a nil → Background fallback.
	wantFragments := []string{
		"ParentCtx context.Context",
		"parent := opts.ParentCtx",
		"parent = context.Background()",
		"context.WithCancel(parent)",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(body, frag) {
			t.Errorf("wshub.go missing required fragment %q — CTX1 derivation may have regressed", frag)
		}
	}

	// Reverse guardrail: the legacy literal `context.WithCancel(context.Background())`
	// must not reappear inside NewHub. Matching "NewHub" through the next "}"
	// at column-0 bounds the scan to the constructor body.
	newHubBlock := regexp.MustCompile(`(?s)func NewHub\(.*?\n\}\n`).FindString(body)
	if newHubBlock == "" {
		t.Fatal("could not locate NewHub function body")
	}
	if strings.Contains(newHubBlock, "context.WithCancel(context.Background())") {
		t.Error("NewHub body still contains legacy context.WithCancel(context.Background()) — " +
			"parent-ctx derivation regressed")
	}
}

// TestServer_AppCtxWiredToHub locks the Server-side half of the CTX1 contract:
// the app context must be live when NewHub reads it, and cancelling it must
// tear the Hub down even if Shutdown is never called.
//
// This was a source-level pin on the old shape (`s.appCtx = serveCtx` inside
// Start, `serveCtx, serveCancel := context.WithCancel(ctx)`). #2552 moved both
// the context and the Hub into buildServer, which makes the invariant
// structurally unbreakable rather than merely asserted: there is no longer a
// point in the Server's life where appCtx is nil while a Hub exists. So the
// test now drives the real constructor and observes the cascade — a stronger
// check that cannot rot when the wiring is rearranged again.
func TestServer_AppCtxWiredToHub(t *testing.T) {
	t.Parallel()

	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})

	// The pre-Start window is gone: both exist straight out of the constructor.
	if srv.appCtx == nil {
		t.Fatal("appCtx is nil after NewWithOptions — construction must not wait for Start (#2552)")
	}
	if srv.appCancel == nil {
		t.Fatal("appCancel is nil after NewWithOptions — nothing could cancel the app context")
	}
	if srv.hub == nil {
		t.Fatal("hub is nil after NewWithOptions — the whole point of #2552 is that this state does not exist")
	}

	select {
	case <-srv.hub.ctx.Done():
		t.Fatal("hub.ctx already Done straight after construction")
	default:
	}

	// CTX1: an app-level cancel must reach the Hub without Shutdown().
	srv.appCancel()
	select {
	case <-srv.hub.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done within 2s of appCancel — HubOptions.ParentCtx is not wired to appCtx (CTX1)")
	}

	// Idempotent teardown must still work after the parent cancel.
	done := make(chan struct{})
	go func() {
		srv.hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub.Shutdown() hung after appCancel")
	}
}

// TestServerStart_LinksCallerCtxWithoutSecondContext keeps the one thing the
// behavioural test above cannot observe: Start must NOT mint its own
// cancellable context any more. Two contexts is how the old ordering window
// existed in the first place — construction read one while Start installed
// another — and a refactor that reintroduces `context.WithCancel(ctx)` inside
// Start would give the Hub a parent that nothing cancels.
//
// R20260531-GO-001 is preserved through appCancel: a srv.Serve error cancels
// the app context directly, so the shutdown goroutine's discoveryCache.Wait()
// still wakes instead of deadlocking.
func TestServerStart_LinksCallerCtxWithoutSecondContext(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(raw)

	startIdx := strings.Index(body, "func (s *Server) Start(ctx context.Context) error {")
	if startIdx < 0 {
		t.Fatal("server.go: Server.Start not found")
	}
	start := body[startIdx:]
	if strings.Contains(start, "context.WithCancel(ctx)") {
		t.Error("server.go Start: mints its own cancellable context again — appCtx is created in buildServer, " +
			"and a second context reopens the pre-Start ordering window #2552 closed")
	}
	if !strings.Contains(start, "s.appCancel()") {
		t.Error("server.go Start: must call s.appCancel() (the caller-ctx linker, the deferred teardown and " +
			"the srv.Serve error path all go through it — R20260531-GO-001)")
	}
	// The app context itself must be a cancellable child created at construction.
	if !regexp.MustCompile(`s\.appCtx,\s*s\.appCancel\s*=\s*context\.WithCancel\(`).MatchString(body) {
		t.Error("server.go: buildServer must create appCtx/appCancel with context.WithCancel (#2552)")
	}
	if !regexp.MustCompile(`\bappCtx\s+context\.Context\b`).MatchString(body) {
		t.Error("server.go: Server struct must declare appCtx context.Context field")
	}
}
