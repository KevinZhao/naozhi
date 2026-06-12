package session

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// TestHandleList_CLIVersion_TracksLiveUpgrade pins R20260612-global-version
// end to end: the global dashboard banner's stats.cli_version must follow a
// live binary-version observation (a host claude upgrade under a long-lived
// naozhi) rather than staying frozen at the spawn-time value baked into the
// static stats block at startup.
func TestHandleList_CLIVersion_TracksLiveUpgrade(t *testing.T) {
	w := cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	w.CLIVersion = "2.1.100" // spawn-time detection at startup
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{Wrapper: w, MaxProcs: 3})

	h := New(Deps{
		Router:        r,
		NodeAccess:    noNodeAccessor{},
		NodeCache:     node.NewCacheManager(func() map[string]node.Conn { return nil }, func() {}),
		StartedAt:     time.Now(),
		WatchdogNoOut: &atomic.Int64{},
		WatchdogTotal: &atomic.Int64{},
	})

	// First poll: banner shows the spawn-time version.
	if got := listCLIVersion(t, h); got != "2.1.100" {
		t.Fatalf("initial cli_version = %q, want spawn-time 2.1.100", got)
	}

	// A spawned process reports a newer binary (host upgrade). The static
	// stats block is now stale, but buildSessionStats re-resolves cli_version
	// from router.CLIVersion() per poll.
	w.ObserveLiveVersion("2.1.174")

	if got := listCLIVersion(t, h); got != "2.1.174" {
		t.Fatalf("post-upgrade cli_version = %q, want live 2.1.174 (banner stayed stale)", got)
	}
}

// listCLIVersion does a GET /api/sessions and extracts stats.cli_version.
func listCLIVersion(t *testing.T, h *Handlers) string {
	t.Helper()
	rec := doList(h, "")
	if rec.Code != 200 {
		t.Fatalf("HandleList status = %d, want 200", rec.Code)
	}
	var resp struct {
		Stats struct {
			CLIVersion string `json:"cli_version"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /api/sessions: %v", err)
	}
	return resp.Stats.CLIVersion
}
