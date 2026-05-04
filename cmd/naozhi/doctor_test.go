package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDoctor_HealthSuccess covers the happy path: health 200 with the
// expected JSON body. Exercises the checkHealth rendering.
func TestDoctor_HealthSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","uptime":"5s","version":"test"}`)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	d := &doctor{addr: srv.URL, timeout: 2 * time.Second, out: &buf}
	d.checkHealth()
	d.render()
	got := buf.String()
	if !strings.Contains(got, "✓ http /health") {
		t.Errorf("expected pass icon on /health; got %q", got)
	}
	if !strings.Contains(got, `"status":"ok"`) {
		t.Errorf("detail should include response body; got %q", got)
	}
	if d.hasFail {
		t.Error("happy path must not set hasFail")
	}
}

// TestDoctor_HealthDown asserts that an unreachable URL is fail-level,
// so CI scripts can exit on `naozhi doctor` exit code alone.
func TestDoctor_HealthDown(t *testing.T) {
	t.Parallel()
	// Port 1 is reserved/unused; connect always fails fast.
	var buf bytes.Buffer
	d := &doctor{addr: "http://127.0.0.1:1", timeout: 200 * time.Millisecond, out: &buf}
	d.checkHealth()
	if !d.hasFail {
		t.Error("unreachable health endpoint must produce fail-level finding")
	}
	if !strings.Contains(buf.String()+renderFindings(d.findings), "✗") {
		// buf is empty before render, so assert via findings.
	}
	if d.findings[0].Level != "fail" {
		t.Errorf("finding level = %q, want fail", d.findings[0].Level)
	}
}

// TestDoctor_AuthStatusBranches covers the 3 branch classifications
// that gate the auth finding. 401/403 → fail, 200 → pass, other → warn.
func TestDoctor_AuthStatusBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status    int
		wantLevel string
	}{
		{http.StatusOK, "pass"},
		{http.StatusUnauthorized, "fail"},
		{http.StatusForbidden, "fail"},
		{http.StatusTooManyRequests, "warn"},
		{http.StatusInternalServerError, "warn"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer T" {
					t.Errorf("server expected bearer token; got %q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			d := &doctor{addr: srv.URL, token: "T", timeout: 2 * time.Second, out: io.Discard}
			d.checkAuth()
			if len(d.findings) == 0 {
				t.Fatal("checkAuth produced no finding")
			}
			if d.findings[0].Level != tc.wantLevel {
				t.Errorf("status %d → level %q, want %q (detail=%q)",
					tc.status, d.findings[0].Level, tc.wantLevel, d.findings[0].Detail)
			}
		})
	}
}

// TestDoctor_NoTokenSkipsAuth pins that the auth check gracefully
// degrades to a warn (not fail) when no token is configured. Without
// this, a misconfigured doctor invocation would report the whole
// system as broken.
func TestDoctor_NoTokenSkipsAuth(t *testing.T) {
	t.Parallel()
	d := &doctor{addr: "http://127.0.0.1:1", timeout: 100 * time.Millisecond, out: io.Discard}
	d.checkAuth()
	if d.hasFail {
		t.Error("missing token must not fail the doctor — it's a config issue, not a service issue")
	}
	if d.findings[0].Level != "warn" {
		t.Errorf("no-token auth check level = %q, want warn", d.findings[0].Level)
	}
}

// TestDoctor_PprofLoopbackGate covers the two interesting pprof
// states: 200 (hardening + loopback OK) and 403 (remote or hardening
// blocking). The 403 stays WARN — the operator running doctor from a
// bastion would see 403 even though production is healthy.
func TestDoctor_PprofLoopbackGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status    int
		wantLevel string
	}{
		{http.StatusOK, "pass"},
		{http.StatusForbidden, "warn"},
		{http.StatusBadGateway, "warn"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			d := &doctor{addr: srv.URL, token: "T", timeout: 2 * time.Second, out: io.Discard}
			d.checkPprof()
			if d.findings[0].Level != tc.wantLevel {
				t.Errorf("status %d → level %q, want %q", tc.status, d.findings[0].Level, tc.wantLevel)
			}
		})
	}
}

// TestDoctor_ExpvarLoopbackGate covers the three interesting expvar
// states analogously to pprof plus a fourth: 200 with missing counter
// body (routing broken) must be a fail so operators catch "endpoint up
// but empty".
func TestDoctor_ExpvarLoopbackGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		body      string
		wantLevel string
	}{
		{"pass-ok-body", http.StatusOK, `{"naozhi_session_create_total":0}`, "pass"},
		{"fail-empty-body", http.StatusOK, `{"other":1}`, "fail"},
		{"warn-forbidden", http.StatusForbidden, "", "warn"},
		{"warn-bad-gateway", http.StatusBadGateway, "", "warn"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			d := &doctor{addr: srv.URL, token: "T", timeout: 2 * time.Second, out: io.Discard}
			d.checkExpvar()
			if d.findings[0].Level != tc.wantLevel {
				t.Errorf("status %d body=%q → level %q, want %q",
					tc.status, tc.body, d.findings[0].Level, tc.wantLevel)
			}
		})
	}
}

// TestDoctor_ExpvarNoToken pins the no-token degraded path: warn, not
// fail, consistent with the other auth-gated probes.
func TestDoctor_ExpvarNoToken(t *testing.T) {
	t.Parallel()
	d := &doctor{addr: "http://127.0.0.1:1", timeout: 100 * time.Millisecond, out: io.Discard}
	d.checkExpvar()
	if d.hasFail {
		t.Error("missing token must not fail the doctor — it's a config issue")
	}
	if d.findings[0].Level != "warn" {
		t.Errorf("no-token expvar check level = %q, want warn", d.findings[0].Level)
	}
}

// TestDoctor_StateDirProbes covers the writability check — the only
// piece that touches the local fs. Uses t.TempDir as a fake $HOME to
// avoid polluting the real user dir. Cannot t.Parallel because
// t.Setenv disallows parallel per Go stdlib rules.
func TestDoctor_StateDirProbes(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(fake, ".naozhi"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Swap $HOME for this test so UserHomeDir points at our tmp.
	t.Setenv("HOME", fake)
	d := &doctor{out: io.Discard}
	d.checkStateDir()
	if d.findings[0].Level != "pass" {
		t.Errorf("fresh writable dir → level %q, want pass (detail=%q)",
			d.findings[0].Level, d.findings[0].Detail)
	}
}

// TestDoctor_StateDirMissing pins the "warn, not fail" branch when
// ~/.naozhi doesn't exist yet (first-run scenario). Cannot t.Parallel
// because t.Setenv disallows parallel per Go stdlib rules.
func TestDoctor_StateDirMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "no-such-home"))
	d := &doctor{out: io.Discard}
	d.checkStateDir()
	if d.findings[0].Level != "warn" {
		t.Errorf("missing dir → level %q, want warn", d.findings[0].Level)
	}
	if d.hasFail {
		t.Error("missing state dir is a warn, not a fail (first-run case)")
	}
}

// TestDoctor_RenderFormats covers the two output formats so a future
// refactor doesn't accidentally drop one.
func TestDoctor_RenderFormats(t *testing.T) {
	t.Parallel()
	t.Run("text", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := &doctor{out: &buf}
		d.add("demo", "pass", "ok")
		d.add("demo", "warn", "meh")
		d.add("demo", "fail", "boom")
		d.render()
		got := buf.String()
		for _, want := range []string{"✓", "⚠", "✗"} {
			if !strings.Contains(got, want) {
				t.Errorf("text render missing icon %q; got %q", want, got)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := &doctor{out: &buf, json: true}
		d.add("demo", "pass", "ok")
		d.render()
		got := buf.String()
		if !strings.Contains(got, `"category":"demo"`) || !strings.Contains(got, `"level":"pass"`) {
			t.Errorf("json render missing fields; got %q", got)
		}
		// Each line must be a standalone JSON object (newline-delimited
		// so monitoring tools can line-split).
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("json render must newline-terminate each record; got %q", got)
		}
	})
}

// TestLoadTokenBestEffort_EnvFirst covers the env-var path.
func TestLoadTokenBestEffort_EnvFirst(t *testing.T) {
	t.Setenv("NAOZHI_DASHBOARD_TOKEN", "from-env")
	t.Setenv("DASHBOARD_TOKEN", "legacy")
	if got := loadTokenBestEffort(); got != "from-env" {
		t.Errorf("env precedence failed: got %q, want from-env", got)
	}
}

// TestLoadTokenBestEffort_EnvFileFallback exercises the ~/.naozhi/env
// scan. Important: we must also unset NAOZHI_DASHBOARD_TOKEN via
// t.Setenv("", "") pattern (t.Setenv unset doesn't exist; use os.Unsetenv).
func TestLoadTokenBestEffort_EnvFileFallback(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "home")
	envDir := filepath.Join(fake, ".naozhi")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	envFile := filepath.Join(envDir, "env")
	body := "# comment line\nNAOZHI_DASHBOARD_TOKEN=\"from-file\"\nOTHER=X\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("HOME", fake)
	t.Setenv("NAOZHI_DASHBOARD_TOKEN", "") // t.Setenv empty → unsets at test end
	t.Setenv("DASHBOARD_TOKEN", "")
	os.Unsetenv("NAOZHI_DASHBOARD_TOKEN")
	os.Unsetenv("DASHBOARD_TOKEN")
	if got := loadTokenBestEffort(); got != "from-file" {
		t.Errorf("env-file parse failed: got %q, want from-file", got)
	}
}

// renderFindings helps a few tests that need to inspect findings
// without reaching into the doctor's internal state through render.
func renderFindings(fs []finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Level)
		b.WriteString(": ")
		b.WriteString(f.Detail)
		b.WriteString("\n")
	}
	return b.String()
}
