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
	d := &doctor{addr: srv.URL, client: srv.Client(), timeout: 2 * time.Second, out: &buf}
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
			d := &doctor{addr: srv.URL, client: srv.Client(), token: "T", timeout: 2 * time.Second, out: io.Discard}
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

// TestDoctor_StateDirProbes covers the writability check — the only
// piece that touches the local fs. Uses t.TempDir as a fake $HOME to
// avoid polluting the real user dir. Cannot t.Parallel because
// t.Setenv disallows parallel per Go stdlib rules.
func TestDoctor_StateDirProbes(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "home")
	// 0o700 satisfies the R229-SEC-13 perm gate; the writability probe
	// should then succeed and report pass.
	if err := os.MkdirAll(filepath.Join(fake, ".naozhi"), 0o700); err != nil {
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

// TestDoctor_StateDirGroupWorldReadable pins R229-SEC-13: when ~/.naozhi
// has any group/world permission bits set, doctor must emit a warn rather
// than silently passing. EventLog/sessions.json files inside use 0600 but
// a 0755 parent still lets local users list keys + traverse.
func TestDoctor_StateDirGroupWorldReadable(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "home")
	stateDir := filepath.Join(fake, ".naozhi")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Re-chmod to defeat any umask-driven downgrade by MkdirAll.
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("HOME", fake)
	d := &doctor{out: io.Discard}
	d.checkStateDir()
	if d.findings[0].Level != "warn" {
		t.Errorf("0o755 state dir → level %q, want warn (detail=%q)",
			d.findings[0].Level, d.findings[0].Detail)
	}
	if !strings.Contains(d.findings[0].Detail, "group/world-accessible") {
		t.Errorf("warn detail missing perm hint: %q", d.findings[0].Detail)
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

// TestValidateDoctorAddr covers the URL validation added in R20260602-SEC-2.
// The function must accept valid http/https addresses and reject malformed
// strings or non-http/https schemes.
func TestValidateDoctorAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"loopback http", "http://127.0.0.1:8180", false},
		{"loopback https", "https://127.0.0.1:8180", false},
		{"localhost http", "http://localhost:8180", false},
		{"remote https", "https://example.com:8180", false},
		{"no scheme", "127.0.0.1:8180", true},
		{"file scheme", "file:///etc/passwd", true},
		{"ftp scheme", "ftp://example.com", true},
		{"empty string", "", true},
		{"scheme only", "http://", false}, // valid URL structure; host empty but scheme ok
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateDoctorAddr(tc.addr)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateDoctorAddr(%q) err=%v, wantErr=%v", tc.addr, err, tc.wantErr)
			}
		})
	}
}

// TestValidateDoctorAddr_SchemelessHostPort pins that a bare host:port
// (no scheme) is rejected — it would be treated as a path by url.Parse,
// and the scheme would be empty, which our check blocks.
func TestValidateDoctorAddr_SchemelessHostPort(t *testing.T) {
	t.Parallel()
	cases := []string{
		"127.0.0.1:8180",
		"localhost:8180",
		"example.com:443",
		"//evil.com/path",
	}
	for _, addr := range cases {
		if err := validateDoctorAddr(addr); err == nil {
			t.Errorf("validateDoctorAddr(%q) = nil, want error for schemeless addr", addr)
		}
	}
}

// TestDoctor_CheckAuth_NonLoopbackWarning verifies that checkAuth still
// works correctly when the doctor is pointed at a remote (non-loopback)
// server — the validation must not break the normal localhost flow and
// the auth check itself must still produce correct pass/fail results
// regardless of host. (The slog.Warn fires at runDoctor level, not here.)
func TestDoctor_CheckAuth_NonLoopbackWarning(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// httptest.NewServer binds 127.0.0.1; addr validation (http scheme) passes.
	if err := validateDoctorAddr(srv.URL); err != nil {
		t.Fatalf("loopback server URL failed validation: %v", err)
	}
	d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second, out: io.Discard}
	d.checkAuth()
	if d.findings[0].Level != "pass" {
		t.Errorf("auth finding level = %q, want pass", d.findings[0].Level)
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

// TestDoctor_IMAccess grades each configured IM platform's im_access state:
// any platform open to every sender warns and is named, otherwise the pass
// line counts each platform's distinct users and admins.
func TestDoctor_IMAccess(t *testing.T) {
	t.Parallel()
	const platforms = "platforms:\n" +
		"  feishu:\n    app_id: a\n    app_secret: s\n" +
		"  slack:\n    bot_token: xoxb-test\n" +
		"  discord:\n    bot_token: d\n"
	cases := []struct {
		name, body, level, detail string
	}{
		{"no platform", "log:\n  level: info\n", "pass", "no IM platform configured"},
		{"open platforms", platforms +
			"im_access:\n  platforms:\n    feishu:\n      allowed_users: [ou_a]\n",
			"warn", "slack, discord open to every sender (no im_access entry, default_deny off): " +
				"anyone who can message the bot runs commands on this host; " +
				"set im_access.platforms.<platform>.allowed_users or im_access.default_deny: true"},
		{"all guarded", platforms + "im_access:\n  default_deny: true\n  platforms:\n" +
			"    feishu:\n      allowed_users: [ou_a, \" ou_b\"]\n      admin_users: [ou_b, ou_c]\n" +
			"    discord:\n      allowed_users: [\"1\", \"2\", \"3\"]\n",
			"pass", "feishu 3 user(s), 2 admin(s) · slack refused (default_deny) · discord 3 user(s), all admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &doctor{out: &bytes.Buffer{}, configPath: writeDoctorConfig(t, tc.body)}
			d.checkIMAccess()
			if f := findingsByCategory(d)["im access"]; f.Level != tc.level || f.Detail != tc.detail {
				t.Errorf("im access = %+v\nwant level %q detail %q", f, tc.level, tc.detail)
			}
		})
	}

	d := &doctor{out: &bytes.Buffer{}, configPath: filepath.Join(t.TempDir(), "missing.yaml")}
	d.checkIMAccess()
	if f := findingsByCategory(d)["im access"]; f.Level != "pass" || f.Detail != "skipped (config not loaded)" {
		t.Errorf("unloadable config: im access = %+v, want a pass skip", f)
	}
}
