package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/promexport"
	"github.com/naozhi/naozhi/internal/session"
)

func metricsServer(t *testing.T, token string, on bool) *Server {
	t.Helper()
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         session.NewRouter(session.RouterConfig{}),
		Platforms:      map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:        "claude",
		DashboardToken: token,
		Features:       FeatureOptions{Metrics: on},
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	return srv
}

func getMetrics(srv *Server, bearer, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestMetrics_OffByDefault(t *testing.T) {
	t.Parallel()
	if w := getMetrics(metricsServer(t, "tok", false), "tok", ""); w.Code != http.StatusNotFound {
		t.Fatalf("metrics_enabled false: code = %d, want 404", w.Code)
	}
}

func TestMetrics_RefusesWithoutTokenConfigured(t *testing.T) {
	t.Parallel()
	if w := getMetrics(metricsServer(t, "", true), "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("no dashboard token: code = %d, want 403", w.Code)
	}
}

func TestMetrics_BearerFromAnyHost(t *testing.T) {
	t.Parallel()
	srv := metricsServer(t, "tok", true)
	if w := getMetrics(srv, "", "10.0.0.9:4000"); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous: code = %d, want 401 with challenge", w.Code)
	}
	if w := getMetrics(srv, "wrong", "10.0.0.9:4000"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: code = %d", w.Code)
	}
	// A scraper on another host is the point; no loopback restriction.
	w := getMetrics(srv, "tok", "10.0.0.9:4000")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != promexport.ContentType {
		t.Fatalf("content-type = %q", ct)
	}
	body := w.Body.String()
	// A counter dispatch registers at init is present with a TYPE line.
	if !strings.Contains(body, "# TYPE naozhi_dispatch_message_total counter\nnaozhi_dispatch_message_total ") {
		t.Fatalf("missing dispatch counter in:\n%s", body[:min(len(body), 800)])
	}
	if strings.Contains(body, "memstats") || strings.Contains(body, "cmdline") {
		t.Fatal("stdlib expvars must not be exported")
	}
}
