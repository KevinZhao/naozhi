package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func driftDoctor(t *testing.T, srv *httptest.Server, token, configPath string) *doctor {
	t.Helper()
	d := &doctor{
		addr:       srv.URL,
		token:      token,
		timeout:    2 * time.Second,
		configPath: configPath,
		client:     srv.Client(),
	}
	return d
}

func driftFinding(t *testing.T, d *doctor) finding {
	t.Helper()
	for _, f := range d.findings {
		if f.Category == "config-drift" {
			return f
		}
	}
	t.Fatalf("no config-drift finding; findings=%+v", d.findings)
	return finding{}
}

// TestCheckConfigDrift covers the #2538 doctor matrix: hash match → pass,
// mismatch → warn "not applied", sections a reload left pending → warn
// "restart required for", no token → skip (pass), old process without a
// fingerprint → warn, malformed fingerprint → warn.
func TestCheckConfigDrift(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("platforms: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSum := fmt.Sprintf("%x", sha256.Sum256([]byte("platforms: {}\n")))

	// Like the real handler, only an accepted token gets the authenticated
	// section; anyone else sees status and uptime.
	healthWithExtra := func(sum, extra string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := `{"status":"ok","uptime":"1h"`
			if r.Header.Get("Authorization") == "Bearer tok" {
				body += `,"cli_available":true` + extra
				if sum != "" {
					body += `,"config_sha256":"` + sum + `","config_loaded_at":"2026-09-05T10:00:00Z"`
				}
			}
			body += `}`
			_, _ = w.Write([]byte(body))
		}))
	}
	healthWith := func(sum string) *httptest.Server { return healthWithExtra(sum, "") }

	t.Run("match_pass", func(t *testing.T) {
		srv := healthWith(diskSum)
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "pass" || !strings.Contains(f.Detail, "match") {
			t.Errorf("finding = %+v, want pass/match", f)
		}
	})

	t.Run("mismatch_warns_not_applied", func(t *testing.T) {
		srv := healthWith(strings.Repeat("0", 64))
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "not applied") || !strings.Contains(f.Detail, "naozhi config reload") {
			t.Errorf("finding = %+v, want warn/not applied", f)
		}
		if d.hasFail {
			t.Error("drift must not flip hasFail; it is a warn")
		}
	})

	// A reload that left restart-only sections keeps the old fingerprint and
	// lists them; doctor names them even when the sha happens to match.
	t.Run("pending_restart_warns_with_sections", func(t *testing.T) {
		srv := healthWithExtra(diskSum, `,"config_restart_required":["cli","session"]`)
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "restart required for: cli, session") {
			t.Errorf("finding = %+v, want warn/restart required for: cli, session", f)
		}
		if d.hasFail {
			t.Error("pending restart must not flip hasFail; it is a warn")
		}
	})

	t.Run("no_token_skips", func(t *testing.T) {
		srv := healthWith(diskSum)
		defer srv.Close()
		d := driftDoctor(t, srv, "", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "pass" || !strings.Contains(f.Detail, "skipped") {
			t.Errorf("finding = %+v, want pass/skipped", f)
		}
	})

	// A rejected token gets the public body, which has no fingerprint; that
	// is not an old process, so it must not warn "predates #2538".
	t.Run("rejected_token_skips", func(t *testing.T) {
		srv := healthWith(diskSum)
		defer srv.Close()
		d := driftDoctor(t, srv, "wrong", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "pass" || !strings.Contains(f.Detail, "token not accepted") {
			t.Errorf("finding = %+v, want pass/token not accepted", f)
		}
	})

	t.Run("process_unreachable_skips", func(t *testing.T) {
		srv := healthWith(diskSum)
		srv.Close() // dead endpoint
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.client = &http.Client{Timeout: time.Second}
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "pass" || !strings.Contains(f.Detail, "skipped") {
			t.Errorf("finding = %+v, want pass/skipped", f)
		}
	})

	// /health is outside input: a fingerprint that is not sha256 hex warns
	// instead of panicking on the [:12] slice, and is sanitised on the way out.
	t.Run("malformed_short_fingerprint_warns", func(t *testing.T) {
		srv := healthWith("x")
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "malformed") {
			t.Errorf("finding = %+v, want warn/malformed", f)
		}
	})

	t.Run("malformed_nonhex_fingerprint_warns", func(t *testing.T) {
		srv := healthWith(`\u001b[31m` + strings.Repeat("0", 59))
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "malformed") {
			t.Errorf("finding = %+v, want warn/malformed", f)
		}
		if strings.Contains(f.Detail, "\x1b") {
			t.Errorf("detail carries a raw escape byte: %q", f.Detail)
		}
	})

	t.Run("malformed_long_fingerprint_capped", func(t *testing.T) {
		srv := healthWith(strings.Repeat("z", 4096))
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || len(f.Detail) > 200 {
			t.Errorf("finding = %d-byte %s, want a capped warn", len(f.Detail), f.Level)
		}
	})

	t.Run("loaded_at_sanitised", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","cli_available":true,"config_sha256":"` + diskSum +
				`","config_loaded_at":"\u001b]0;pwned\u0007"}`))
		}))
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "pass" || strings.ContainsAny(f.Detail, "\x1b\x07") {
			t.Errorf("finding = %+v, want pass with no control bytes", f)
		}
	})

	t.Run("no_fingerprint_warns", func(t *testing.T) {
		srv := healthWith("")
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "no config fingerprint") {
			t.Errorf("finding = %+v, want warn/no fingerprint", f)
		}
	})
}

func TestIsSHA256Hex(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{fmt.Sprintf("%x", sha256.Sum256([]byte("a"))), true},
		{strings.Repeat("f", 64), true},
		{"", false},
		{"x", false},
		{strings.Repeat("0", 63), false},
		{strings.Repeat("0", 65), false},
		{strings.Repeat("A", 64), false},
		{strings.Repeat("0", 63) + "g", false},
		{strings.Repeat("0", 63) + "/", false},
		{strings.Repeat("0", 63) + ":", false},
		{strings.Repeat("0", 63) + "`", false},
	} {
		if got := isSHA256Hex(tc.in); got != tc.want {
			t.Errorf("isSHA256Hex(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
