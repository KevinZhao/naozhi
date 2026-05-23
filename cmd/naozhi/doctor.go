package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
)

// runDoctor prints a one-shot diagnostic report. Stays CLI-local (no
// unix socket, no new surface on the HTTP server) so a disabled
// naozhi is still a place to start triage. Checks include systemd
// status, HTTP /health, auth validity, pprof reachability, state dir
// writability, and — on Linux — the zero-downtime scope count that
// hints whether the sudoers hardening took.
//
// Exit codes:
//
//	0 — everything passed or only WARN-level findings
//	1 — at least one FAIL finding (service down, auth broken, etc.)
//	2 — invalid flags / cannot render report
//
// Designed to be grep/pipe friendly: every line is `<icon> <category>
// — <detail>`. The icon is ✓/⚠/✗ so scripts can filter by the
// leading byte without parsing the full column.
func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	addr := fs.String("addr", envDefault("NAOZHI_BASE_URL", "http://127.0.0.1:8180"),
		"base URL for HTTP checks (NAOZHI_BASE_URL)")
	tokenFlag := fs.String("token", "",
		"dashboard token; defaults to NAOZHI_DASHBOARD_TOKEN env or ~/.naozhi/env")
	timeout := fs.Duration("timeout", 5*time.Second,
		"per-HTTP-check deadline")
	jsonOut := fs.Bool("json", false,
		"emit findings as JSON (one object per line) — easier to consume from CI / monitoring")
	configPath := fs.String("config", "config.yaml",
		"path to config.yaml; used to render the CLI Backends section (multi-backend RFC §11.2)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	token := *tokenFlag
	if token == "" {
		token = loadTokenBestEffort()
	}

	d := &doctor{
		addr:       strings.TrimRight(*addr, "/"),
		token:      token,
		timeout:    *timeout,
		out:        os.Stdout,
		json:       *jsonOut,
		configPath: *configPath,
	}
	d.run()
	if d.hasFail {
		os.Exit(1)
	}
}

// envDefault returns os.Getenv(key) if set, else fallback.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadTokenBestEffort tries NAOZHI_DASHBOARD_TOKEN, then DASHBOARD_TOKEN
// (legacy alias some scripts still export), then scans ~/.naozhi/env for
// either name. Intentionally tolerant: a failure here just means we run
// auth-scoped checks without a token and report them as "cannot verify".
func loadTokenBestEffort() string {
	for _, k := range []string{"NAOZHI_DASHBOARD_TOKEN", "DASHBOARD_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".naozhi", "env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"NAOZHI_DASHBOARD_TOKEN=", "DASHBOARD_TOKEN="} {
			if strings.HasPrefix(line, prefix) {
				return strings.Trim(strings.TrimPrefix(line, prefix), `"'`)
			}
		}
	}
	return ""
}

// finding is one diagnostic result. level is "pass"/"warn"/"fail";
// the human icon is chosen at render time so the JSON and text paths
// stay in sync.
type finding struct {
	Category string `json:"category"`
	Level    string `json:"level"`
	Detail   string `json:"detail"`
}

type doctor struct {
	addr       string
	token      string
	timeout    time.Duration
	out        io.Writer
	json       bool
	configPath string

	hasFail  bool
	findings []finding
}

func (d *doctor) run() {
	d.checkBinary()
	d.checkSystemd()
	d.checkHealth()
	d.checkAuth()
	d.checkPprof()
	d.checkExpvar()
	d.checkStateDir()
	d.checkZeroDowntimeScopes()
	d.checkServerSecurity()
	d.render()
	// Backends section runs after the standard findings render so its
	// section-header layout doesn't interleave with the per-finding ✓/✗
	// stream. JSON mode skips the section entirely — JSON consumers
	// already get backend metadata via /api/cli/backends and shouldn't
	// have to parse free-form section headers from doctor's stdout.
	if !d.json {
		d.renderBackendsSection()
	}
}

func (d *doctor) add(category, level, detail string) {
	if level == "fail" {
		d.hasFail = true
	}
	d.findings = append(d.findings, finding{Category: category, Level: level, Detail: detail})
}

func (d *doctor) render() {
	if d.json {
		enc := json.NewEncoder(d.out)
		for _, f := range d.findings {
			_ = enc.Encode(f)
		}
		return
	}
	for _, f := range d.findings {
		icon := "✓"
		switch f.Level {
		case "warn":
			icon = "⚠"
		case "fail":
			icon = "✗"
		}
		fmt.Fprintf(d.out, "%s %-22s %s\n", icon, f.Category, f.Detail)
	}
}

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

// renderBackendsSection prints the multi-backend RFC §11.2 status block
// to d.out. It is intentionally derived from static data only (config
// file + Profile registry + DetectBackendsCtx --version probe) so doctor
// can run while naozhi.service is down — that's the most common time the
// operator reaches for it. No HTTP, no shim, no server-side state is
// touched.
//
// Layout (also see RFC §11.2):
//
//	=== CLI Backends ===
//	Default: claude
//
//	[claude] claude-code 2.1.92    proto=stream-json  caps=Replay,Priority,StreamJSON
//	  path: /home/user/.local/bin/claude
//	  history: ~/.claude/projects/...
//
//	[kiro] kiro 2.3.0              proto=acp           caps=SoftInterrupt
//	  path: /home/user/.local/bin/kiro-cli
//	  history: ~/.kiro/sessions/cli/
//
//	=== Reverse Nodes ===
//	(no reverse_nodes configured)
//
// or, when reverse_nodes are present:
//
//	node "macbook"   (live caps unknown — register a node to inspect)
//	  can host: claude  (kiro requires "acp" cap)
//
// Reverse-node cap info is intentionally NOT live: doctor cannot start
// the WebSocket hub without booting half the server, and the cap data
// only appears once a node connects. We dump configured nodes plus a
// per-backend "what would be required" line so an operator inspecting
// the config sees the dependency before they ever bring the system up.
func (d *doctor) renderBackendsSection() {
	// Ensure Profile registry is initialised — concurrent-safe and
	// idempotent. EnsureDefaults wraps RegisterDefaults in a sync.Once;
	// it does the right thing whether main has already registered, the
	// helper is being called for the first time, or two parallel doctor
	// invocations race the bootstrap. Replaces the earlier recover()
	// pattern, which could leak a partial registry if a panic fired
	// mid-RegisterDefaults (PR #122 review HIGH).
	backend.EnsureDefaults()

	// Best-effort config load. If config is missing or malformed, fall
	// back to "no config" rendering — we still want to show what the
	// binary CAN drive. The user typically runs doctor in two modes:
	// "service is broken, give me triage data" (config exists) and
	// "I just installed naozhi, what backends does it support" (no
	// config yet).
	cfg, cfgErr := config.Load(d.configPath)
	defaultBackend := "claude"
	var cfgBackends []config.CLIBackendConfig
	var cfgReverseNodes map[string]config.ReverseNodeEntry
	if cfgErr == nil && cfg != nil {
		defaultBackend = cfg.DefaultBackendID()
		cfgBackends = cfg.EnabledBackends()
		cfgReverseNodes = cfg.ReverseNodes
	} else {
		// Synthesise an entry per registered Profile so the section is
		// still informative without a config. ID order matches Profile
		// registration order (claude, kiro, ...).
		for _, p := range backend.All() {
			cfgBackends = append(cfgBackends, config.CLIBackendConfig{ID: p.ID})
		}
	}

	// Probe each registered backend's binary. Use a short context so a
	// hung --version invocation can't freeze doctor; the caller should
	// see the per-backend probe result quickly.
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	probes := cli.DetectBackendsCtx(ctx)
	probeByID := make(map[string]cli.BackendInfo, len(probes))
	for _, p := range probes {
		probeByID[p.ID] = p
	}

	// Index Profiles by ID for caps + history-dir lookup.
	profileByID := make(map[string]backend.Profile, len(backend.All()))
	for _, p := range backend.All() {
		profileByID[p.ID] = p
	}

	fmt.Fprintln(d.out)
	fmt.Fprintln(d.out, "=== CLI Backends ===")
	if cfgErr != nil {
		fmt.Fprintf(d.out, "(config %s not loaded: %v — showing registry defaults only)\n",
			d.configPath, cfgErr)
	}
	fmt.Fprintf(d.out, "Default: %s\n\n", defaultBackend)

	for _, b := range cfgBackends {
		id := b.ID
		if id == "" {
			id = defaultBackend
		}
		profile, profileOK := profileByID[id]
		probe := probeByID[id]
		displayName := id
		if profileOK {
			displayName = profile.DisplayName
		}
		version := probe.Version
		if version == "" {
			version = "unknown"
		}
		// Render protocol + caps. NewProtocol lookup happens through the
		// Profile so an unknown ID degrades gracefully to "proto=?".
		protoName := "?"
		capsStr := "(unknown)"
		if profileOK {
			proto := profile.NewProtocol(backend.ProtocolDeps{})
			protoName = proto.Name()
			capsStr = formatCapsForDoctor(cli.ProtocolCaps(proto))
		}
		fmt.Fprintf(d.out, "[%s] %s %s  proto=%s  caps=%s\n",
			id, displayName, version, protoName, capsStr)
		// path: prefer the probe (it walks $PATH). Fall back to the
		// configured cli.backends[].path so an operator who set an
		// override sees what they typed.
		path := probe.Path
		if path == "" {
			path = b.Path
		}
		if path == "" && profileOK {
			path = profile.DefaultBinary + " (not found on $PATH)"
		}
		fmt.Fprintf(d.out, "  path:    %s\n", path)
		if !probe.Available {
			fmt.Fprintf(d.out, "  status:  unavailable (--version probe failed)\n")
		}
		fmt.Fprintf(d.out, "  history: %s\n", historyDirForBackend(id))
		if len(profile.RequiredNodeCaps) > 0 {
			fmt.Fprintf(d.out, "  reverse-node caps required: %s\n",
				strings.Join(profile.RequiredNodeCaps, ", "))
		}
		fmt.Fprintln(d.out)
	}

	// Reverse Nodes section.
	fmt.Fprintln(d.out, "=== Reverse Nodes ===")
	if len(cfgReverseNodes) == 0 {
		fmt.Fprintln(d.out, "(no reverse_nodes configured)")
		return
	}
	// Sort node IDs so output is deterministic across runs.
	ids := make([]string, 0, len(cfgReverseNodes))
	for id := range cfgReverseNodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := cfgReverseNodes[id]
		display := entry.DisplayName
		if display == "" {
			display = id
		}
		fmt.Fprintf(d.out, "node %q  display=%q  (live caps unknown — visible only after node connects)\n",
			id, display)
		// For each registered backend, list whether the node can
		// host it based on RequiredNodeCaps. Doctor cannot inspect
		// the live cap set, so we phrase the line as a
		// pre-condition: "claude needs no special cap; kiro needs
		// the 'acp' cap from the connected node".
		for _, p := range backend.All() {
			if len(p.RequiredNodeCaps) == 0 {
				fmt.Fprintf(d.out, "  %s: no special cap required\n", p.ID)
			} else {
				fmt.Fprintf(d.out, "  %s: requires node caps [%s]\n",
					p.ID, strings.Join(p.RequiredNodeCaps, ", "))
			}
		}
	}
}

// formatCapsForDoctor renders Caps as a comma-separated list of the
// flags that are TRUE. Empty result becomes "(none)" so the line
// stays parseable. Order matches the struct field order so the
// output is deterministic.
func formatCapsForDoctor(c cli.Caps) string {
	parts := make([]string, 0, 4)
	if c.Replay {
		parts = append(parts, "Replay")
	}
	if c.Priority {
		parts = append(parts, "Priority")
	}
	if c.SoftInterrupt {
		parts = append(parts, "SoftInterrupt")
	}
	if c.StreamJSON {
		parts = append(parts, "StreamJSON")
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ",")
}

// historyDirForBackend returns the documented history directory for the
// given backend ID, sourced from the Profile registry (added in PR #117
// follow-up). Falls back to "(none)" when the backend is unknown OR
// when its Profile has no HistoryDir set — both are valid states for an
// in-memory-only backend.
//
// Reading from the Profile (rather than a private switch in doctor.go)
// closes the compile-safety hole flagged in PR #117 review: adding a
// new backend with a transcript directory now requires only a Profile
// entry; doctor inherits the value automatically.
//
// Self-bootstraps the registry via EnsureDefaults (sync.Once) so the
// helper is callable from unit tests that import it directly, and is
// safe under parallel goroutines — replaces the earlier recover()
// pattern which could leak a partial registry on concurrent racing
// callers (PR #122 review HIGH).
func historyDirForBackend(id string) string {
	backend.EnsureDefaults()
	if p, ok := backend.Get(id); ok && p.HistoryDir != "" {
		return p.HistoryDir
	}
	return "(none)"
}
