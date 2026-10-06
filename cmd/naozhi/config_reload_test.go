package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigReload_CLIExitCodes(t *testing.T) {
	var gotAuth string
	body := `{"sha256":"deadbeefcafe1234","loaded_at":"2026-10-06T00:00:00Z","applied":["im_access"],"restart_required":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/system/config/reload" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := configReload([]string{"-addr", srv.URL, "-token", "tok"}, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if gotAuth != "Bearer tok" || !strings.Contains(out.String(), "applied: im_access") {
		t.Fatalf("auth=%q out=%q", gotAuth, out.String())
	}

	body = `{"sha256":"x","loaded_at":"2026-10-06T00:00:00Z","applied":[],"restart_required":["cli"]}`
	out.Reset()
	if code := configReload([]string{"-addr", srv.URL}, &out); code != 3 || !strings.Contains(out.String(), "restart required for: cli") {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "config reload failed: bad yaml", http.StatusUnprocessableEntity)
	}))
	defer bad.Close()
	if code := configReload([]string{"-addr", bad.URL}, &out); code != 1 {
		t.Fatalf("exit = %d on 422", code)
	}
	if code := configReload([]string{"-addr", "ftp://x"}, &out); code != 2 {
		t.Fatalf("exit = %d on bad addr", code)
	}
}
