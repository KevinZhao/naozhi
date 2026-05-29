// File: doctor_checks.go
//
// Phase 5-prep / R-cmd-doctor-checks-extract (2026-05-28):
// 把 doctor.go 中 9 个诊断 check 方法 + 2 个 helper 抽到独立文件。
// **纯物理切分，逐字保留原代码、零行为变化**。
//
// 抽出的内容（按 origin/master doctor.go line 184-509 原貌）：
//   - checkBinary / checkSystemd / checkHealth / checkAuth / checkPprof /
//     checkExpvar / checkStateDir / checkZeroDowntimeScopes /
//     checkServerSecurity  — 9 个 (d *doctor) 诊断方法
//   - isLoopbackAddr        — addr 是否仅绑 localhost（纯函数）
//   - runOutput             — 带 3s 硬超时的 exec.Cmd 输出捕获
//
// doctor.go 保留编排层（run / render / renderBackendsSection）+ finding /
// doctor struct + d.add() 等。check 方法通过同 package 的 *doctor receiver
// 继续调用，零改动。
package main

import (
	"context"
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
	// `systemctl is-active` is the canonical liveness check. Doesn't
	// require sudo for read-only queries.
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
	// Augment with MainPID/uptime for quick grep.
	show, _ := runOutput(exec.Command("systemctl", "show", "naozhi",
		"--property=MainPID,ActiveEnterTimestamp,NRestarts", "--no-pager"))
	show = strings.ReplaceAll(strings.TrimSpace(show), "\n", " · ")
	// R187-SEC-M1: systemctl show output is local but goes to the operator
	// terminal. Sanitize any bidi/C1/ANSI escapes defensively so a crafted
	// unit file (or a future --property value) can't flip display order.
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.add("http /health", "fail", fmt.Sprintf("%s unreachable: %v", url, err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// R187-SEC-M1: /health response echoes to terminal. If the configured
	// addr is hijacked or a future /health implementation emits untrusted
	// strings, bidi / ANSI escapes would flip operator display. Sanitize.
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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

// checkExpvar probes /api/debug/vars to confirm the OBS2 counters endpoint
// is reachable. Like pprof, this sits behind auth + loopback-only; a 403
// when doctor runs from outside the host is the hardening working.
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.add("expvar", "fail", "request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// Spot-check one naozhi_* counter — if the endpoint responds but the
		// payload is empty (e.g. operator hit the stdlib /debug/vars mount
		// instead of /api/debug/vars in a misconfigured proxy), we want to
		// surface that as fail, not pass.
		// R185-QUAL-M1: surface read errors distinctly so a truncated body
		// from a transient network glitch is not misreported as a routing bug.
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
	// R229-SEC-13: warn on group/world readable state_dir. EventLog and
	// sessions.json files inside use 0600 explicitly via WriteFileAtomic,
	// but the parent dir's mode determines whether other local users can
	// list filenames + traverse to read sidecar artefacts. cookie_secret
	// uses the same 0600 floor — surface the mismatch via doctor so
	// operators see it once, not via a quiet log line at every startup.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		d.add("state dir", "warn",
			fmt.Sprintf("%s is group/world-accessible (mode %04o); restrict with: chmod 0700 %s",
				dir, mode, dir))
		return
	}
	// Writability probe — avoids chmod/owner noise that a raw Stat
	// wouldn't catch (e.g. naozhi running as a different uid).
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
	// systemctl list-units --type=scope lists transient scopes
	// (including naozhi-shim-*.scope if sudoers hardening is
	// active). 0 scopes with any running shim = sudoers denied the
	// busctl call and moveToShimsCgroup fell through to the fallback.
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

// checkServerSecurity warns when the deployment is likely behind a TLS-
// terminating reverse proxy (ALB / CloudFront / Nginx) but trusted_proxy
// is not enabled. In that mode the dashboard cookie is minted without
// the Secure flag because r.TLS == nil reaches the cookie writer, and
// X-Forwarded-Proto is not consulted unless trusted_proxy is set. The
// resulting cookie can leak over a downgrade attack on the proxy hop.
//
// Heuristic: dashboard_token configured (so the binary intends to serve
// authenticated traffic) + listen addr is non-loopback (so external
// peers can reach it). False positives are possible (single-host
// loopback-only with token, or no proxy at all) — but a "warn"-level
// finding pointed at config.yaml is cheap to dismiss after a one-time
// review and catches the genuinely dangerous case loudly.
//
// Doctor's contract is to use FAIL only for "broken now"; this is
// "broken on next request" so warn is the right level. R232-SEC-13.
func (d *doctor) checkServerSecurity() {
	cfg, err := config.Load(d.configPath)
	if err != nil || cfg == nil {
		// Already surfaced by other checks; don't duplicate noise.
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
// Conservative — when in doubt return false (so checkServerSecurity warns
// rather than silently passing). Recognises empty addr, ":port" (which
// binds 0.0.0.0), and explicit loopback hosts. Anything else is treated
// as potentially externally reachable.
func isLoopbackAddr(addr string) bool {
	if addr == "" {
		return false // empty effectively binds to default which may be 0.0.0.0
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port — could be just a host or a path. Treat unparseable as
		// non-loopback to err on the side of warning.
		host = addr
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// runOutput runs cmd with a 3s hard deadline and returns combined
// stdout+stderr. Intentionally swallows context cancel as the err
// value when the inner process itself exited cleanly — callers only
// care about the exec.ExitError path (non-zero exit = meaningful
// signal, e.g. systemctl is-active returns 3 for "inactive"). The
// 3s cap prevents a hung systemd from freezing the whole report.
func runOutput(cmd *exec.Cmd) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bound := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	out, err := bound.CombinedOutput()
	return string(out), err
}
