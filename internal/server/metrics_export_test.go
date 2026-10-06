package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
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

// TestMetrics_LabelNamesAndHistogram: the real registrations in internal/metrics
// reach the scraper as named labels and a histogram family, not a `key` label.
func TestMetrics_LabelNamesAndHistogram(t *testing.T) {
	t.Parallel()
	metrics.RecordCLISpawn("zz-metrics-backend")
	metrics.RecordSpawnDiag("zz-layer", "zz-action")
	metrics.ObserveCronExecutionDuration(42)
	w := getMetrics(metricsServer(t, "tok", true), "tok", "10.0.0.9:4000")
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE naozhi_cli_spawn_total_by_backend counter\n",
		`naozhi_cli_spawn_total_by_backend{backend="zz-metrics-backend"} `,
		`naozhi_spawn_diag_total{layer="zz-layer",action="zz-action"} `,
		"# TYPE naozhi_cron_execution_duration_ms histogram\n",
		`naozhi_cron_execution_duration_ms_bucket{le="+Inf"} `,
		"naozhi_cron_execution_duration_ms_count ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `naozhi_spawn_diag_total{key=`) {
		t.Error("a registered map fell back to the generic key label")
	}
}
