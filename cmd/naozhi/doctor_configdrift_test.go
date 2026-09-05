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
// mismatch → warn "restart required", no token → skip (pass), old process
// without a fingerprint → warn.
func TestCheckConfigDrift(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("platforms: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSum := fmt.Sprintf("%x", sha256.Sum256([]byte("platforms: {}\n")))

	healthWith := func(sum string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := `{"status":"ok"`
			if sum != "" {
				body += `,"config_sha256":"` + sum + `","config_loaded_at":"2026-09-05T10:00:00Z"`
			}
			body += `}`
			_, _ = w.Write([]byte(body))
		}))
	}

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

	t.Run("mismatch_warns_restart_required", func(t *testing.T) {
		srv := healthWith(strings.Repeat("0", 64))
		defer srv.Close()
		d := driftDoctor(t, srv, "tok", cfgPath)
		d.checkConfigDrift()
		f := driftFinding(t, d)
		if f.Level != "warn" || !strings.Contains(f.Detail, "restart required") {
			t.Errorf("finding = %+v, want warn/restart required", f)
		}
		if d.hasFail {
			t.Error("drift must not flip hasFail; it is a warn")
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
