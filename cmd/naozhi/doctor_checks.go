// doctor 的各项诊断 check 方法；编排层（run / render）在 doctor.go。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
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

func (d *doctor) checkHealth() {
	url := d.addr + "/health"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.add("http /health", "fail", "request build: "+err.Error())
		return
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("http /health", "fail", fmt.Sprintf("%s unreachable: %v", url, err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Response echoes to the terminal; a hijacked addr could emit escapes.
	bodyStr := osutil.SanitizeForLog(strings.TrimSpace(string(body)), 512)
	if resp.StatusCode != http.StatusOK {
		d.add("http /health", "fail", fmt.Sprintf("status=%d body=%s", resp.StatusCode, bodyStr))
		return
	}
	d.add("http /health", "pass", bodyStr)
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
	cfg, err := config.Load(d.configPath)
	if err != nil || cfg == nil {
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
// config.yaml changed after the process loaded it — restart required. No
// token / unreachable process / unreadable config degrade to a skip, not a
// fail: this check is about drift, not liveness (checkHealth owns that).
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

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.addr+"/health", nil)
	if err != nil {
		d.add("config-drift", "fail", "request build: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		d.add("config-drift", "pass", "skipped (process unreachable: "+err.Error()+")")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.add("config-drift", "pass", fmt.Sprintf("skipped (/health status=%d)", resp.StatusCode))
		return
	}
	var health struct {
		ConfigSHA256   string `json:"config_sha256"`
		ConfigLoadedAt string `json:"config_loaded_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&health); err != nil {
		d.add("config-drift", "warn", "cannot parse /health JSON: "+err.Error())
		return
	}
	if health.ConfigSHA256 == "" {
		d.add("config-drift", "warn", "process reports no config fingerprint (predates #2538); upgrade to compare")
		return
	}
	if health.ConfigSHA256 == diskSum {
		d.add("config-drift", "pass", "config_sha256 match ("+diskSum[:12]+"…), loaded_at="+health.ConfigLoadedAt)
		return
	}
	mtime := ""
	if fi, statErr := os.Stat(d.configPath); statErr == nil {
		mtime = fi.ModTime().Format(time.RFC3339)
	}
	d.add("config-drift", "warn", fmt.Sprintf(
		"restart required: config.yaml changed at %s after process loaded at %s (disk %s… vs process %s…)",
		mtime, health.ConfigLoadedAt, diskSum[:12], health.ConfigSHA256[:12]))
}
