package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestUploadCleanup_UsesAppCtxNotHubCtx pins R215-ARCH-P2-3 (#579): the
// upload-store cleanup goroutine must follow the app lifecycle, not the Hub
// lifecycle, so a future Hub hot-reload (drain + swap) cannot prematurely
// cancel the cleanup loop and leak temp-file entries.
//
// #2552 simplified the wiring it checks. The old code resolved a local
// `cleanupCtx := s.appCtx` with an `if cleanupCtx == nil { s.hub.ctx }`
// fallback, because appCtx did not exist until Start; the fallback WAS the
// regression risk this test guarded. appCtx is now created in buildServer, so
// the fallback is gone and StartCleanup takes it directly.
func TestUploadCleanup_UsesAppCtxNotHubCtx(t *testing.T) {
	t.Parallel()

	// The store is owned by the Server from construction, so there is no
	// window where a request could find it unwired.
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})
	t.Cleanup(srv.appCancel)
	if srv.uploadStore == nil {
		t.Fatal("uploadStore is nil after NewWithOptions — it must be built with the rest of the dashboard (#2552)")
	}
	if srv.hub == nil || srv.hub.uploadStore != srv.uploadStore {
		t.Error("the Hub must share the Server's upload store instance (HubOptions.UploadStore), " +
			"otherwise a WS file_id and an HTTP upload resolve against different stores")
	}
	if hs.sendH == nil || hs.sendH.uploadStore != srv.uploadStore {
		t.Error("SendHandler must share the same upload store instance")
	}

	// Which ctx the loop follows is a wiring decision with no cheap runtime
	// probe (the loop only ticks on a long timer), so it stays a narrow
	// source assertion.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "routes.go"))
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "s.uploadStore.StartCleanup(s.appCtx)") {
		t.Error("routes.go: the upload-cleanup loop must start against s.appCtx so it survives a " +
			"Hub hot-reload — R215-ARCH-P2-3 (#579)")
	}
	// Reverse guard, kept narrow so unrelated intentional s.hub.ctx uses still compile.
	if strings.Contains(body, "StartCleanup(s.hub.ctx)") {
		t.Error("routes.go: StartCleanup is back on the Hub context — cleanup must follow the app lifecycle (#579)")
	}
}
