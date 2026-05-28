package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestProjectHandlers_FilesExistsLimiter_Wired locks the S13 wiring: server.New
// must install a non-nil filesExistsLimiter on Handlers so the
// production path has DoS protection out of the box. Without this contract a
// refactor that drops the newIPLimiterWithProxy call in server.go would leave
// the handler technically correct but unprotected — regressions like that have
// shipped before (see R59-PERF-M3 where resolveProjectFile was called per
// path). We verify by inspecting the struct field on a minimally-configured
// Server, not by probing the rate-limiter timing (which would be flaky).
func TestProjectHandlers_FilesExistsLimiter_Wired(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: router,
	})
	if srv == nil {
		t.Fatal("NewWithOptions returned nil")
	}
	if srv.projectH == nil {
		t.Fatal("projectH must be constructed even with nil ProjectManager")
	}
	if !srv.projectH.HasFilesExistsLimiter() {
		t.Error("server.New must wire FilesExistsLimiter (S13); " +
			"a nil limiter leaves /api/projects/files/exists unprotected against DoS")
	}
}

