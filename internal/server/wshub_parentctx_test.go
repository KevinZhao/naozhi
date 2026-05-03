package server

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
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
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:    router,
		Guard:     guard,
		NodesMu:   &nodesMu,
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
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:  router,
		Guard:   guard,
		NodesMu: &nodesMu,
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

// TestServer_AppCtxWiredToHub locks the Server-side half of the CTX1
// contract: Start must stash the app ctx on s.appCtx before calling
// registerDashboard, and registerDashboard must forward it as
// HubOptions.ParentCtx. Checked at the source level so a refactor that
// drops either line triggers a clear failure.
func TestServer_AppCtxWiredToHub(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)

	serverSrc, err := os.ReadFile(filepath.Join(dir, "server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	if !strings.Contains(string(serverSrc), "s.appCtx = ctx") {
		t.Error("server.go: Start must assign s.appCtx = ctx before registerDashboard " +
			"(CTX1 requires appCtx to be set when NewHub reads it)")
	}
	if !strings.Contains(string(serverSrc), "appCtx context.Context") {
		t.Error("server.go: Server struct must declare appCtx context.Context field")
	}

	dashSrc, err := os.ReadFile(filepath.Join(dir, "dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	if !strings.Contains(string(dashSrc), "ParentCtx: s.appCtx") {
		t.Error("dashboard.go: registerDashboard must forward s.appCtx into " +
			"HubOptions.ParentCtx (CTX1 wiring)")
	}
}
