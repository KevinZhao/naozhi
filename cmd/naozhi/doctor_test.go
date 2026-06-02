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

	"github.com/naozhi/naozhi/internal/cli"
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
	d := &doctor{addr: srv.URL, token: "tok", timeout: 2 * time.Second, out: io.Discard}
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

// TestDoctor_BackendsSection_NoConfig pins that the section still
// renders something useful when --config points at a non-existent file.
// Operators run doctor on first install ("what does this binary
// support?") before they've written config.yaml; the section must
// degrade to "registry defaults only" rather than crash or emit nothing.
// Cannot t.Parallel because backend.RegisterDefaults touches a global
// registry and concurrent doctor renders would race the panic-recover.
func TestDoctor_BackendsSection_NoConfig(t *testing.T) {
	var buf bytes.Buffer
	d := &doctor{
		out:        &buf,
		timeout:    2 * time.Second,
		configPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	}
	d.renderBackendsSection()
	got := buf.String()

	// Header is mandatory — the operator's grep target.
	if !strings.Contains(got, "=== CLI Backends ===") {
		t.Errorf("missing CLI Backends section header; got %q", got)
	}
	// Default line tells operator which backend is router default.
	if !strings.Contains(got, "Default:") {
		t.Errorf("missing Default: line; got %q", got)
	}
	// At least the two built-in backends must appear, even without
	// config (degrade path uses Profile registry).
	for _, want := range []string{"[claude]", "[kiro]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing backend tag %q; got %q", want, got)
		}
	}
	// Reverse Nodes section is unconditional (empty config → "no
	// reverse_nodes configured" line still prints).
	if !strings.Contains(got, "=== Reverse Nodes ===") {
		t.Errorf("missing Reverse Nodes section header; got %q", got)
	}
	if !strings.Contains(got, "no reverse_nodes configured") {
		t.Errorf("missing empty-reverse-nodes hint; got %q", got)
	}
}

// TestDoctor_BackendsSection_WithConfig pins the happy path: a real
// config.yaml with cli.backends configured plus a reverse_nodes entry.
// The section must surface the configured default + each backend block
// + the reverse_nodes block with per-backend cap requirements.
func TestDoctor_BackendsSection_WithConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `cli:
  backend: kiro
  backends:
    - id: claude
    - id: kiro
reverse_nodes:
  macbook:
    token: "test-token-not-real-in-prod"
    display_name: "Macbook"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	d := &doctor{
		out:        &buf,
		timeout:    2 * time.Second,
		configPath: cfgPath,
	}
	d.renderBackendsSection()
	got := buf.String()

	if !strings.Contains(got, "Default: kiro") {
		t.Errorf("Default line should reflect cli.backend=kiro; got %q", got)
	}
	for _, want := range []string{
		"=== CLI Backends ===",
		"[claude] claude-code",
		"[kiro] kiro",
		"proto=stream-json",
		"proto=acp",
		"history: ~/.claude/projects/",
		"history: ~/.kiro/sessions/cli/",
		"=== Reverse Nodes ===",
		`node "macbook"`,
		"requires node caps [acp]", // kiro's RequiredNodeCaps surfaces in reverse-nodes section
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; full output:\n%s", want, got)
		}
	}
	// Reverse-node section must list the per-backend cap rule for
	// claude (no cap required) AND kiro (acp required).
	if !strings.Contains(got, "claude: no special cap required") {
		t.Errorf("reverse-node block missing claude no-cap line; got %q", got)
	}
}

// TestFormatCapsForDoctor pins the rendering shape so dashboards / docs
// linking to specific cap names don't silently drift.
func TestFormatCapsForDoctor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		caps cli.Caps
		want string
	}{
		{"empty", cli.Caps{}, "(none)"},
		{"replay_only", cli.Caps{Replay: true}, "Replay"},
		{
			"claude_full",
			cli.Caps{Replay: true, Priority: true, StreamJSON: true},
			"Replay,Priority,StreamJSON",
		},
		{
			"acp_soft_interrupt",
			cli.Caps{SoftInterrupt: true},
			"SoftInterrupt",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatCapsForDoctor(tc.caps); got != tc.want {
				t.Errorf("formatCapsForDoctor(%+v) = %q, want %q", tc.caps, got, tc.want)
			}
		})
	}
}

// TestHistoryDirForBackend pins the doctor history-dir lookup. After PR
// #117 follow-up the lookup reads from backend.Profile.HistoryDir rather
// than a private switch, so this test pins both the registry-default
// values for the two built-in backends AND the unknown-id fallback.
//
// Safe under t.Parallel since PR #122 follow-up: historyDirForBackend now
// bootstraps via backend.EnsureDefaults (sync.Once), which is concurrent-
// safe and idempotent.
func TestHistoryDirForBackend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want string
	}{
		{"claude", "~/.claude/projects/"},
		{"kiro", "~/.kiro/sessions/cli/"},
		{"unknown-backend-xyz", "(none)"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			if got := historyDirForBackend(tc.id); got != tc.want {
				t.Errorf("historyDirForBackend(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
