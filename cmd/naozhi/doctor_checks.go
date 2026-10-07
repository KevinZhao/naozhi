// doctor 的各项诊断 check 方法；编排层（run / render）在 doctor.go。
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/selfupdate"
)

func (d *doctor) checkBinary() {
	exe, err := os.Executable()
	if err != nil {
		d.add("binary", "warn", "cannot resolve own path: "+err.Error())
		return
	}
	resolved, _ := filepath.EvalSymlinks(exe)
	if resolved == "" {
		resolved = exe
	}
	d.add("binary", "pass", fmt.Sprintf("%s · version=%s · %s/%s",
		resolved, version, runtime.GOOS, runtime.GOARCH))
}

// codesignGOOS and inspectCodesignFn are indirected so the verdicts are
// testable off darwin.
var (
	codesignGOOS      = runtime.GOOS
	inspectCodesignFn = selfupdate.InspectCodesign
)

// checkCodesign warns when macOS privacy grants will not survive an upgrade:
// an ad-hoc requirement is the cdhash, which every new binary changes.
func (d *doctor) checkCodesign() {
	if codesignGOOS != "darwin" {
		d.add("codesign", "pass", "skipped (not darwin)")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		d.add("codesign", "warn", "cannot resolve own path: "+err.Error())
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	switch kind, id := inspectCodesignFn(exe); kind {
	case selfupdate.CodesignLeaf:
		d.add("codesign", "pass", fmt.Sprintf("identifier=%s leaf=%s · upgrades re-sign with it, macOS privacy grants persist",
			id.Identifier, id.LeafSHA1))
	case selfupdate.CodesignOther:
		d.add("codesign", "pass", "signed with a non-ad-hoc identity · macOS privacy grants persist")
	case selfupdate.CodesignAdhoc:
		d.add("codesign", "warn", "ad-hoc signature · macOS re-asks for folder access after every upgrade; see docs/ops/macos-codesign.md")
	default:
		d.add("codesign", "warn", "cannot read the code signature of "+exe)
	}
}

func (d *doctor) checkSystemd() {
	if runtime.GOOS != "linux" {
		d.add("systemd", "pass", "skipped (not linux)")
		return
	}
	out, err := runOutput(exec.Command("systemctl", "is-active", "naozhi"))
	state := strings.TrimSpace(out)
	if err != nil && state == "" {
		d.add("systemd", "warn", "systemctl unavailable: "+err.Error())
		return
	}
	if state != "active" {
		d.add("systemd", "fail", fmt.Sprintf("naozhi.service is %q (expected active)", state))
		return
	}
	show, _ := runOutput(exec.Command("systemctl", "show", "naozhi",
		"--property=MainPID,ActiveEnterTimestamp,NRestarts", "--no-pager"))
	show = strings.ReplaceAll(strings.TrimSpace(show), "\n", " · ")
	// Sanitize bidi/C1/ANSI escapes so a crafted unit file cannot flip the
	// operator's terminal display.
	d.add("systemd", "pass", "active · "+osutil.SanitizeForLog(show, 512))
}

func (d *doctor) checkAuth() {
	if d.token == "" {
		d.add("auth", "warn", "no token (set NAOZHI_DASHBOARD_TOKEN); auth-scoped checks skipped")
		return
	}
	url := d.addr + "/api/sessions"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.add("auth", "fail", "request build: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("auth", "fail", "request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		d.add("auth", "pass", "token accepted (/api/sessions 200)")
	case http.StatusUnauthorized, http.StatusForbidden:
		d.add("auth", "fail", fmt.Sprintf("token rejected (%d); check NAOZHI_DASHBOARD_TOKEN", resp.StatusCode))
	default:
		d.add("auth", "warn", fmt.Sprintf("unexpected status %d on /api/sessions", resp.StatusCode))
	}
}

func (d *doctor) checkPprof() {
	if d.token == "" {
		d.add("pprof", "warn", "no token; pprof reachability not verified")
		return
	}
	url := d.addr + "/api/debug/pprof/"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.add("pprof", "fail", "request build: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("pprof", "fail", "request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		d.add("pprof", "pass", "reachable at "+url)
	case http.StatusForbidden:
		d.add("pprof", "warn",
			"403 — non-loopback (doctor not running on the naozhi host?) or hardening works as intended")
	default:
		d.add("pprof", "warn", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}

// checkExpvar probes /api/debug/vars (auth + loopback-only; a 403 from
// outside the host is the hardening working).
func (d *doctor) checkExpvar() {
	if d.token == "" {
		d.add("expvar", "warn", "no token; expvar reachability not verified")
		return
	}
	url := d.addr + "/api/debug/vars"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.add("expvar", "fail", "request build: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("expvar", "fail", "request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// Spot-check one naozhi_* counter so a misrouted stdlib /debug/vars
		// mount fails instead of passing; read errors are reported distinctly.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if readErr != nil {
			d.add("expvar", "fail", "read body failed: "+readErr.Error())
			return
		}
		if !strings.Contains(string(body), "naozhi_session_create_total") {
			d.add("expvar", "fail", "reachable but counter missing from payload — routing wrong?")
			return
		}
		d.add("expvar", "pass", "reachable at "+url)
	case http.StatusForbidden:
		d.add("expvar", "warn",
			"403 — non-loopback (doctor not running on the naozhi host?) or hardening works as intended")
	default:
		d.add("expvar", "warn", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}

// checkMetrics probes GET /metrics, which only exists with
// server.metrics_enabled; 404 therefore means "not enabled", not a fault.
func (d *doctor) checkMetrics() {
	if d.token == "" {
		d.add("metrics", "warn", "no token; /metrics reachability not verified")
		return
	}
	url := d.addr + "/metrics"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.add("metrics", "fail", "request build: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("metrics", "fail", "request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		if readErr != nil {
			d.add("metrics", "fail", "read body failed: "+readErr.Error())
			return
		}
		if !strings.Contains(string(body), "# TYPE naozhi_session_create_total counter") {
			d.add("metrics", "fail", "reachable but not Prometheus text with naozhi_* counters — routing wrong?")
			return
		}
		d.add("metrics", "pass", "reachable at "+url)
	case http.StatusNotFound:
		d.add("metrics", "pass", "not enabled (server.metrics_enabled is off)")
	case http.StatusForbidden:
		d.add("metrics", "warn", "403 — server.dashboard_token is not configured on the server")
	case http.StatusUnauthorized:
		d.add("metrics", "warn", "token rejected (401); check NAOZHI_DASHBOARD_TOKEN")
	default:
		d.add("metrics", "warn", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}

func (d *doctor) checkStateDir() {
	home, err := os.UserHomeDir()
	if err != nil {
		d.add("state dir", "warn", "cannot resolve home: "+err.Error())
		return
	}
	dir := filepath.Join(home, ".naozhi")
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			d.add("state dir", "warn", dir+" missing (first run?)")
			return
		}
		d.add("state dir", "warn", "stat: "+err.Error())
		return
	}
	if !info.IsDir() {
		d.add("state dir", "fail", dir+" exists but is not a directory")
		return
	}
	// Files inside are 0600, but the dir mode decides whether other local
	// users can list filenames and traverse to sidecar artefacts.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		d.add("state dir", "warn",
			fmt.Sprintf("%s is group/world-accessible (mode %04o); restrict with: chmod 0700 %s",
				dir, mode, dir))
		return
	}
	// A real write catches owner/uid mismatches a Stat would miss.
	tmp, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		d.add("state dir", "fail", dir+" not writable: "+err.Error())
		return
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())
	d.add("state dir", "pass", dir+" writable")
}

func (d *doctor) checkZeroDowntimeScopes() {
	if runtime.GOOS != "linux" {
		d.add("zero-downtime", "pass", "skipped (not linux)")
		return
	}
	// naozhi-shim-*.scope units exist only when sudoers hardening let the
	// busctl call through; 0 with live shims means the cgroup fallback ran.
	out, err := runOutput(exec.Command("systemctl", "--no-legend",
		"--no-pager", "list-units", "--type=scope"))
	if err != nil {
		d.add("zero-downtime", "warn", "systemctl list-units failed: "+err.Error())
		return
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "naozhi-shim-") {
			count++
		}
	}
	if count == 0 {
		d.add("zero-downtime", "warn",
			"0 naozhi-shim-*.scope units — sudoers hardening not active OR no shims alive yet (see docs/ops/sudoers-hardening.md)")
		return
	}
	d.add("zero-downtime", "pass", fmt.Sprintf("%d shim scope(s) active (sudoers hardening is working)", count))
}

// checkServerSecurity warns when dashboard_token is set on a non-loopback
// addr without trusted_proxy: behind a TLS-terminating proxy r.TLS is nil and
// X-Forwarded-Proto is ignored, so the dashboard cookie is minted without
// Secure and can leak on a downgrade of the proxy hop. Warn, not FAIL —
// doctor reserves FAIL for "broken now".
func (d *doctor) checkServerSecurity() {
	cfg, err := d.loadConfig()
	if err != nil {
		d.add("server security", "pass", "skipped (config not loaded)")
		return
	}
	if cfg.Server.DashboardToken == "" {
		d.add("server security", "pass", "no dashboard token configured (open mode)")
		return
	}
	if isLoopbackAddr(cfg.Server.Addr) {
		d.add("server security", "pass", "loopback bind — TLS-terminating proxy unlikely")
		return
	}
	if cfg.Server.TrustedProxy {
		d.add("server security", "pass", "trusted_proxy=true — Secure cookie flag honours X-Forwarded-Proto")
		return
	}
	d.add("server security", "warn",
		"dashboard_token set + non-loopback addr ("+cfg.Server.Addr+") + trusted_proxy=false: "+
			"if you front naozhi with HTTPS termination (ALB/CloudFront/nginx), set server.trusted_proxy: true so dashboard cookies get Secure flag")
}

// checkIMAccess reports who may message the bot on each configured IM
// platform. A platform with no im_access entry while default_deny is off
// serves every sender, and an IM message there runs commands on this host.
func (d *doctor) checkIMAccess() {
	cfg, err := d.loadConfig()
	if err != nil {
		d.add("im access", "pass", "skipped (config not loaded)")
		return
	}
	postures := cfg.IMAccessPostures()
	if len(postures) == 0 {
		d.add("im access", "pass", "no IM platform configured")
		return
	}
	var open, parts []string
	for _, p := range postures {
		switch {
		case p.Open:
			open = append(open, p.Platform)
		case p.Users == 0:
			parts = append(parts, p.Platform+" refused (default_deny)")
		case p.Admins == 0:
			parts = append(parts, fmt.Sprintf("%s %d user(s), all admin", p.Platform, p.Users))
		default:
			parts = append(parts, fmt.Sprintf("%s %d user(s), %d admin(s)", p.Platform, p.Users, p.Admins))
		}
	}
	if len(open) > 0 {
		d.add("im access", "warn", strings.Join(open, ", ")+
			" open to every sender (no im_access entry, default_deny off): anyone who can message the bot runs commands on this host; "+
			"set im_access.platforms.<platform>.allowed_users or im_access.default_deny: true")
		return
	}
	d.add("im access", "pass", strings.Join(parts, " · "))
}

// isLoopbackAddr returns true when addr clearly binds to localhost only.
// Conservative: empty, ":port" (0.0.0.0) and unparseable addrs return false
// so checkServerSecurity warns rather than silently passing.
func isLoopbackAddr(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// runOutput runs cmd with a 3s hard deadline (a hung systemd must not freeze
// the report) and returns combined stdout+stderr; callers care about the
// exec.ExitError path (e.g. systemctl is-active exits 3 for "inactive").
func runOutput(cmd *exec.Cmd) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bound := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	out, err := bound.CombinedOutput()
	return string(out), err
}

// checkConfigDrift compares the disk config's sha256 with the fingerprint the
// running process reports on authenticated /health (#2538): a mismatch means
// config.yaml changed after the process loaded it, and config_restart_required
// lists what a reload could not apply. No token / unreachable process /
// unreadable config degrade to a skip, not a fail: this check is about drift,
// not liveness (checkHealth owns that).
func (d *doctor) checkConfigDrift() {
	if d.token == "" {
		d.add("config-drift", "pass", "skipped (no token; auth-scoped)")
		return
	}
	data, err := os.ReadFile(d.configPath)
	if err != nil {
		d.add("config-drift", "pass", "skipped (config unreadable: "+err.Error()+")")
		return
	}
	diskSum := fmt.Sprintf("%x", sha256.Sum256(data))

	h := d.fetchHealth()
	switch {
	case h.err != nil:
		d.add("config-drift", "pass", "skipped (process unreachable: "+h.err.Error()+")")
		return
	case h.status != http.StatusOK:
		d.add("config-drift", "pass", fmt.Sprintf("skipped (/health status=%d)", h.status))
		return
	case h.decodeErr != nil:
		d.add("config-drift", "warn", "cannot parse /health JSON: "+osutil.SanitizeForLog(h.decodeErr.Error(), 512))
		return
	case !h.authenticated():
		d.add("config-drift", "pass", "skipped (token not accepted by /health; see auth)")
		return
	}
	health := h.payload
	if health.ConfigSHA256 == "" {
		d.add("config-drift", "warn", "process reports no config fingerprint (predates #2538); upgrade to compare")
		return
	}
	if !isSHA256Hex(health.ConfigSHA256) {
		d.add("config-drift", "warn", fmt.Sprintf("process reports a malformed config fingerprint (%q); cannot compare",
			osutil.SanitizeForLog(health.ConfigSHA256, 64)))
		return
	}
	loadedAt := osutil.SanitizeForLog(health.ConfigLoadedAt, 64)
	if pending := health.ConfigRestartRequired; len(pending) > 0 {
		d.checkConfigDriftPending(&health, diskSum, loadedAt)
		return
	}
	if health.ConfigSHA256 == diskSum {
		d.add("config-drift", "pass", "config_sha256 match ("+diskSum[:12]+"…), loaded_at="+loadedAt)
		return
	}
	d.add("config-drift", "warn", fmt.Sprintf(
		"not applied: config.yaml changed at %s after process loaded at %s (disk %s… vs process %s…); "+
			"`naozhi config reload` applies it and lists what still needs a restart",
		configMTime(d.configPath), loadedAt, diskSum[:12], health.ConfigSHA256[:12]))
}

// checkConfigDriftPending reports the sections a reload left for a restart.
// config_sha256 is frozen meanwhile, so an edit made after that reload is
// found by comparing the disk against config_reloaded_sha256 (#3649).
func (d *doctor) checkConfigDriftPending(health *healthPayload, diskSum, loadedAt string) {
	names := make([]string, len(health.ConfigRestartRequired))
	for i, n := range health.ConfigRestartRequired {
		names[i] = osutil.SanitizeForLog(n, 64)
	}
	restart := "restart required for: " + strings.Join(names, ", ")
	frozen := "config_sha256 stays " + health.ConfigSHA256[:12] + "…, loaded_at=" + loadedAt + ", until a restart"
	switch reloaded := health.ConfigReloadedSHA256; {
	case reloaded == "":
		d.add("config-drift", "warn", restart+" ("+frozen+
			"; process reports no config_reloaded_sha256, so edits since the last reload are not compared)")
	case !isSHA256Hex(reloaded):
		d.add("config-drift", "warn", fmt.Sprintf("%s; process reports a malformed config_reloaded_sha256 (%q); cannot compare",
			restart, osutil.SanitizeForLog(reloaded, 64)))
	case reloaded != diskSum:
		d.add("config-drift", "warn", fmt.Sprintf(
			"not applied: config.yaml changed at %s after the last reload (disk %s… vs reloaded %s…); "+
				"`naozhi config reload` applies it; %s (%s)",
			configMTime(d.configPath), diskSum[:12], reloaded[:12], restart, frozen))
	default:
		d.add("config-drift", "warn", restart+" (a config reload applied the hot sections; "+frozen+")")
	}
}

// configMTime is path's RFC3339 mtime, empty when it cannot be stat'ed.
func configMTime(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().Format(time.RFC3339)
}

// isSHA256Hex reports whether s is the 64-char lowercase hex the server writes
// for config_sha256; anything else came from a proxy, an impostor or corruption.
func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
