package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// authHealthServer answers /health like the real handler: the authenticated
// body for "Bearer tok", status and uptime for anyone else. It counts requests.
func authHealthServer(t *testing.T, authBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer tok" {
			_, _ = w.Write([]byte(authBody))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","uptime":"1h"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func findingsByCategory(d *doctor) map[string]finding {
	m := make(map[string]finding, len(d.findings))
	for _, f := range d.findings {
		m[f.Category] = f
	}
	return m
}

// TestDoctor_ServerState covers each authenticated /health field doctor
// turns into a finding.
func TestDoctor_ServerState(t *testing.T) {
	t.Parallel()
	recent := time.Now().UTC().Format(time.RFC3339)
	const old = "2020-01-01T00:00:00Z"
	alive := `{"writer_alive":true,"channel_depth":0,"channel_cap":1024,"dropped_total":0}`
	dead := `{"writer_alive":false,"channel_depth":900,"channel_cap":1024,"dropped_total":7}`
	body := func(cli, platforms, eventlog, tracker, dispatch, loadedAt string) string {
		b := `{"status":"ok","uptime":"2h","version":"v9","cli_available":` + cli + `,"platforms":` + platforms
		if eventlog != "" {
			b += `,"eventlog":` + eventlog
		}
		if tracker != "" {
			b += `,"attachment_tracker":` + tracker
		}
		if dispatch != "" {
			b += `,"dispatch":` + dispatch
		}
		return b + `,"config_loaded_at":"` + loadedAt + `"}`
	}
	replied := `{"message_count":5,"reply_error_count":0,"send_fail_count":0,"last_reply_success_ago":"3m0s"}`
	quiet := `{"message_count":0,"reply_error_count":0,"send_fail_count":0}`
	feishu := `{"feishu":"registered","slack":"registered"}`

	tests := []struct {
		name string
		body string
		want map[string]string // category → level, with a detail substring after "|"
	}{
		{"healthy", body("true", feishu, alive, alive, replied, old), map[string]string{
			"cli runtime":        "pass|cli_available=true",
			"platforms":          "pass|registered: feishu, slack",
			"eventlog writer":    "pass|writer alive",
			"attachment tracker": "pass|writer alive",
			"dispatch":           "pass|last successful reply 3m0s ago",
		}},
		{"cli missing", body("false", feishu, alive, alive, replied, old), map[string]string{
			"cli runtime": "fail|cli_available=false",
		}},
		{"eventlog stalled", body("true", feishu, dead, alive, replied, old), map[string]string{
			"eventlog writer":    "fail|queue 900/1024, dropped 7",
			"attachment tracker": "pass|writer alive",
		}},
		{"tracker stalled", body("true", feishu, alive, dead, replied, old), map[string]string{
			"eventlog writer":    "pass|writer alive",
			"attachment tracker": "fail|writer stalled",
		}},
		{"sections disabled", body("true", feishu, "", "", "", old), map[string]string{
			"eventlog writer":    "pass|skipped (disabled",
			"attachment tracker": "pass|skipped (disabled",
			"dispatch":           "pass|skipped (process reports no dispatch stats)",
		}},
		{"dashboard only", body("true", "{}", alive, alive, quiet, old), map[string]string{
			"platforms": "warn|dashboard-only",
			"dispatch":  "pass|no reply sent yet",
		}},
		{"only failures", body("true", feishu, alive, alive, `{"message_count":3,"reply_error_count":2,"send_fail_count":1}`, old), map[string]string{
			"dispatch": "warn|only failures · messages=3 reply_errors=2 send_fails=1",
		}},
		{"quiet since start", body("true", feishu, alive, alive, quiet, old), map[string]string{
			"dispatch": "warn|no inbound IM messages (slash commands not counted) since start at " + old + " — feishu, slack may not be connected (inferred: no connection state reported)",
		}},
		{"quiet just started", body("true", feishu, alive, alive, quiet, recent), map[string]string{
			"dispatch": "pass|no inbound IM messages yet",
		}},
		{"awaiting first reply", body("true", feishu, alive, alive, `{"message_count":1,"reply_error_count":0,"send_fail_count":0}`, old), map[string]string{
			"dispatch": "pass|no reply sent yet",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := authHealthServer(t, tc.body)
			d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second}
			d.checkServerState()
			if len(d.findings) != len(serverStateCategories) {
				t.Fatalf("got %d findings, want one per category: %+v", len(d.findings), d.findings)
			}
			got := findingsByCategory(d)
			wantFail := false
			for cat, want := range tc.want {
				level, sub, _ := strings.Cut(want, "|")
				f := got[cat]
				if f.Level != level || !strings.Contains(f.Detail, sub) {
					t.Errorf("%s = %+v, want level %s with %q", cat, f, level, sub)
				}
				wantFail = wantFail || level == "fail"
			}
			if d.hasFail != wantFail {
				t.Errorf("hasFail = %v, want %v", d.hasFail, wantFail)
			}
		})
	}
}

// TestDoctor_PlatformConn: the platforms line grades each platform's live
// connection state at the worst platform's level, and the dispatch quiet
// heuristic speaks only for the platforms that report no state. Ages run
// against the reply's Date header, pinned far from the local clock.
func TestDoctor_PlatformConn(t *testing.T) {
	t.Parallel()
	const old = "2020-01-01T00:00:00Z"
	serverNow := time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)
	quiet := `{"message_count":0,"reply_error_count":0,"send_fail_count":0}`
	body := func(platforms, conn string) string {
		return `{"status":"ok","cli_available":true,"platforms":` + platforms + `,"platform_conn":` + conn +
			`,"dispatch":` + quiet + `,"config_loaded_at":"` + old + `"}`
	}
	entry := func(state, ago, lastError string) string {
		d, err := time.ParseDuration(ago)
		if err != nil {
			t.Fatal(err)
		}
		since := serverNow.Add(-d).Format(time.RFC3339)
		return `{"state":"` + state + `","since":"` + since + `","last_error":"` + lastError + `"}`
	}
	one := func(state, sinceAgo, lastError string) string {
		return body(`{"slack":"`+state+`"}`, `{"slack":`+entry(state, sinceAgo, lastError)+`}`)
	}
	long := strings.Repeat("x", 300)

	tests := []struct {
		name     string
		body     string
		level    string
		detail   string // exact platforms detail
		dispatch string // level|substring
	}{
		{"connected", one("connected", "3m0s", "earlier blip"), "pass",
			"slack connected for 3m0s", "pass|no inbound IM messages yet"},
		{"reconnecting inside grace", one("disconnected", "4m59s", "read: connection reset"), "warn",
			"slack disconnected for 4m59s (last error: read: connection reset) — retrying", ""},
		{"connecting inside grace", one("connecting", "30s", ""), "warn",
			"slack connecting for 30s — retrying", ""},
		{"disconnected at grace", one("disconnected", "5m0s", "dial tcp: i/o timeout"), "fail",
			"slack disconnected for 5m0s (last error: dial tcp: i/o timeout) — still not connected after 5m0s", ""},
		{"connecting beyond grace", one("connecting", "1h2m3s", ""), "fail",
			"slack connecting for 1h2m3s — still not connected after 5m0s", ""},
		{"failed fails at once", one("failed", "1s", "invalid_auth"), "fail",
			"slack failed for 1s (last error: invalid_auth) — needs operator action: fix the cause, then restart unless it reconnects by itself", ""},
		{"no since", body(`{"slack":"disconnected"}`, `{}`), "warn",
			"slack disconnected — retrying", ""},
		{"since after the server clock", one("disconnected", "-3s", ""), "warn",
			"slack disconnected for 0s — retrying", ""},
		{"long errors keep every platform", body(`{"discord":"disconnected","slack":"failed","weixin":"connecting"}`,
			`{"discord":`+entry("disconnected", "1m0s", long)+`,"slack":`+entry("failed", "1s", long)+`,"weixin":`+entry("connecting", "2s", long)+`}`), "fail",
			"discord disconnected for 1m0s (last error: " + long[:117] + "...) — retrying; " +
				"slack failed for 1s (last error: " + long[:117] + "...) — needs operator action: fix the cause, then restart unless it reconnects by itself; " +
				"weixin connecting for 2s (last error: " + long[:117] + "...) — retrying", ""},
		{"long name capped", body(`{"`+long+`":"connected"}`, `{"`+long+`":`+entry("connected", "1m0s", "")+`}`), "pass",
			long[:253] + "...", ""},
		{"unknown state", one("suspended", "1m0s", ""), "warn",
			"slack suspended for 1m0s — unknown state (doctor older than the server?)", ""},
		{"mixed with registered", body(`{"slack":"connected","feishu":"registered","weixin":"registered"}`, `{"slack":`+entry("connected", "3m0s", "")+`}`), "pass",
			"slack connected for 3m0s; registered: feishu, weixin (registration only, not a connection state)",
			"warn|since start at " + old + " — feishu, weixin may not be connected (inferred"},
		{"worst platform wins", body(`{"discord":"connected","slack":"failed","weixin":"connecting"}`,
			`{"discord":`+entry("connected", "9m0s", "")+`,"slack":`+entry("failed", "2m0s", "invalid_auth")+`,"weixin":`+entry("connecting", "10s", "")+`}`), "fail",
			"discord connected for 9m0s; slack failed for 2m0s (last error: invalid_auth) — needs operator action: fix the cause, then restart unless it reconnects by itself; weixin connecting for 10s — retrying",
			"pass|no inbound IM messages yet"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", serverNow.Format(http.TimeFormat))
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second}
			d.checkServerState()
			got := findingsByCategory(d)
			if f := got["platforms"]; f.Level != tc.level || f.Detail != tc.detail {
				t.Errorf("platforms = %+v\nwant level %s detail %q", f, tc.level, tc.detail)
			}
			if d.hasFail != (tc.level == "fail") {
				t.Errorf("hasFail = %v, want %v", d.hasFail, tc.level == "fail")
			}
			if tc.dispatch == "" {
				return
			}
			level, sub, _ := strings.Cut(tc.dispatch, "|")
			if f := got["dispatch"]; f.Level != level || !strings.Contains(f.Detail, sub) {
				t.Errorf("dispatch = %+v, want level %s with %q", f, level, sub)
			}
		})
	}
}

// TestDoctor_DispatchQuietServerClock: the dispatch quiet heuristic measures
// time since start on the reply's Date header, not the local clock.
func TestDoctor_DispatchQuietServerClock(t *testing.T) {
	t.Parallel()
	serverNow := time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		ago   time.Duration
		level string
	}{
		{"just started", time.Minute, "pass"},
		{"quiet past the threshold", dispatchQuietWarnAfter + time.Minute, "warn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loadedAt := serverNow.Add(-tc.ago).Format(time.RFC3339)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", serverNow.Format(http.TimeFormat))
				_, _ = w.Write([]byte(`{"status":"ok","cli_available":true,"platforms":{"feishu":"registered"},` +
					`"dispatch":{"message_count":0,"reply_error_count":0,"send_fail_count":0},"config_loaded_at":"` + loadedAt + `"}`))
			}))
			t.Cleanup(srv.Close)
			d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second}
			d.checkServerState()
			if f := findingsByCategory(d)["dispatch"]; f.Level != tc.level {
				t.Errorf("dispatch = %+v, want level %s", f, tc.level)
			}
		})
	}
}

// TestDoctor_PlatformConnLocalClock: a reply without a Date header falls
// back to the local clock for a state's age.
func TestDoctor_PlatformConnLocalClock(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		_, _ = w.Write([]byte(`{"status":"ok","cli_available":true,"platforms":{"slack":"disconnected"},` +
			`"platform_conn":{"slack":{"state":"disconnected","since":"` + since + `"}}}`))
	}))
	t.Cleanup(srv.Close)
	d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second}
	d.checkServerState()
	f := findingsByCategory(d)["platforms"]
	if f.Level != "fail" || !strings.HasPrefix(f.Detail, "slack disconnected for 1h0m") {
		t.Errorf("platforms = %+v, want a fail about an hour-old disconnect", f)
	}
}

// TestDoctor_ServerStateSkips: without a usable authenticated reply every
// category still gets one line saying why it was skipped.
func TestDoctor_ServerStateSkips(t *testing.T) {
	t.Parallel()
	srv, _ := authHealthServer(t, `{"status":"ok","cli_available":true}`)
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>proxy</html>"))
	}))
	t.Cleanup(garbled.Close)
	tests := []struct {
		name, addr, token string
		client            *http.Client
		level, detail     string
	}{
		{"no token", srv.URL, "", srv.Client(), "pass", "skipped (no token; auth-scoped)"},
		{"token rejected", srv.URL, "wrong", srv.Client(), "pass", "skipped (token not accepted by /health; see auth)"},
		{"unreachable", "http://127.0.0.1:1", "tok", &http.Client{Timeout: time.Second}, "pass", "skipped (/health unavailable; see http /health)"},
		{"not json", garbled.URL, "tok", garbled.Client(), "warn", "cannot parse /health JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &doctor{addr: tc.addr, client: tc.client, token: tc.token, timeout: 2 * time.Second}
			d.checkServerState()
			got := findingsByCategory(d)
			for _, cat := range serverStateCategories {
				if f := got[cat]; f.Level != tc.level || !strings.Contains(f.Detail, tc.detail) {
					t.Errorf("%s = %+v, want %s with %q", cat, f, tc.level, tc.detail)
				}
			}
			if d.hasFail {
				t.Error("a skipped server-state check must not fail the run")
			}
		})
	}
}

// TestDoctor_OneHealthRequest: the liveness line, the server-state lines and
// config-drift share a single authenticated GET /health.
func TestDoctor_OneHealthRequest(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("platforms: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, hits := authHealthServer(t, `{"status":"ok","uptime":"2h","version":"v9","cli_available":true,"platforms":{}}`)
	d := &doctor{addr: srv.URL, client: srv.Client(), token: "tok", timeout: 2 * time.Second, configPath: cfgPath}
	d.checkHealth()
	d.checkServerState()
	d.checkConfigDrift()
	if n := hits.Load(); n != 1 {
		t.Errorf("/health requests = %d, want 1", n)
	}
	got := findingsByCategory(d)
	if f := got["http /health"]; f.Level != "pass" || f.Detail != "status=ok uptime=2h version=v9" {
		t.Errorf("liveness = %+v, want a pass summarising the authenticated reply", f)
	}
	if f := got["cli runtime"]; f.Level != "pass" {
		t.Errorf("cli runtime = %+v, want pass from the shared reply", f)
	}
	if f := got["config-drift"]; !strings.Contains(f.Detail, "no config fingerprint") {
		t.Errorf("config-drift = %+v, want it to read the shared authenticated reply", f)
	}
}
