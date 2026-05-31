package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUploadCleanup_UsesAppCtxNotHubCtx pins R215-ARCH-P2-3 (#579): the
// upload-store cleanup goroutine must follow the app lifecycle, not the
// Hub lifecycle, so a future Hub hot-reload (drain + swap) cannot
// prematurely cancel the cleanup loop and leak temp-file entries.
//
// Source-level pin (rather than a runtime test that constructs a real
// Server) keeps the assertion robust against future Server-construction
// refactors that don't touch the actual ctx wiring decision. The full
// runtime semantics are exercised indirectly by the existing
// uploadStore.StartCleanup tests (they use a t.Context() they fully
// control); here we just lock in the *choice* of which ctx to forward.
func TestUploadCleanup_UsesAppCtxNotHubCtx(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)

	src, err := os.ReadFile(filepath.Join(dir, "routes.go"))
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(src)

	// The cleanup ctx MUST be derived from s.appCtx. A literal
	// `s.hub.ctx` next to StartCleanup would mean the Hub-lifecycle
	// regression returned.
	if !strings.Contains(body, "uploads.StartCleanup(cleanupCtx)") {
		t.Error("routes.go: uploads.StartCleanup must be invoked with the resolved " +
			"cleanupCtx variable so its derivation (s.appCtx, not s.hub.ctx) is " +
			"locally visible — see R215-ARCH-P2-3 (#579)")
	}
	if !strings.Contains(body, "cleanupCtx := s.appCtx") {
		t.Error("routes.go: cleanupCtx must be initialised from s.appCtx so the " +
			"upload-cleanup goroutine survives a Hub hot-reload — R215-ARCH-P2-3 (#579)")
	}
	// Defensive: confirm the legacy s.hub.ctx wiring for StartCleanup
	// is gone. The grep is intentionally narrow (StartCleanup-with-hub-ctx)
	// so other intentional s.hub.ctx uses (eg. SetBaseContext, R247-ARCH-15)
	// continue to compile cleanly.
	if strings.Contains(body, "uploads.StartCleanup(s.hub.ctx)") {
		t.Error("routes.go: legacy uploads.StartCleanup(s.hub.ctx) returned — " +
			"upload cleanup must follow app ctx (#579)")
	}
}
