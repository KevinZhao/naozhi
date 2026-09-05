package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
)

// runDoctor prints a one-shot diagnostic report. CLI-local (no new HTTP
// surface) so a down naozhi is still triageable. Exit codes: 0 pass/WARN only,
// 1 at least one FAIL, 2 invalid flags. Each line is `<icon> <category> <detail>`
// with icon ✓/⚠/✗ so scripts can filter on the leading byte.
func runDoctor(args []string) {
	fs, configPath := newSubFlagSet("doctor", "config.yaml")
	addr := fs.String("addr", envDefault("NAOZHI_BASE_URL", "http://127.0.0.1:8180"),
		"base URL for HTTP checks (NAOZHI_BASE_URL)")
	tokenFlag := fs.String("token", "",
		"dashboard token; defaults to NAOZHI_DASHBOARD_TOKEN env or ~/.naozhi/env")
	timeout := fs.Duration("timeout", 5*time.Second,
		"per-HTTP-check deadline")
	jsonOut := fs.Bool("json", false,
		"emit findings as JSON (one object per line) — easier to consume from CI / monitoring")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cleanAddr := strings.TrimRight(*addr, "/")
	if err := validateDoctorAddr(cleanAddr); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		os.Exit(2)
	}
	// A token sent to a non-loopback host may be intentional (CI, staging)
	// but warrants operator awareness.
	if parsedURL, _ := url.Parse(cleanAddr); parsedURL != nil {
		host := parsedURL.Hostname()
		if ip := net.ParseIP(host); !(host == "localhost" ||
			(ip != nil && ip.IsLoopback())) {
			hasToken := *tokenFlag != "" || loadTokenBestEffort() != ""
			if hasToken {
				slog.Warn("doctor: sending token to non-loopback host", "addr", cleanAddr, "host", host)
			}
		}
	}

	token := *tokenFlag
	if token == "" {
		token = loadTokenBestEffort()
	}

	d := &doctor{
		addr:       cleanAddr,
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

// validateDoctorAddr rejects anything but a well-formed http/https URL;
// url.Parse alone is lenient (relative paths, schemeless strings).
func validateDoctorAddr(rawAddr string) error {
	parsed, err := url.Parse(rawAddr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", rawAddr, err)
	}
	switch parsed.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("--addr scheme must be http or https, got %q in %q", parsed.Scheme, rawAddr)
	}
}

// envDefault returns os.Getenv(key) if set, else fallback.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadTokenBestEffort tries NAOZHI_DASHBOARD_TOKEN, then DASHBOARD_TOKEN, then
// ~/.naozhi/env. Tolerant: no token just means auth checks report "cannot verify".
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

// finding is one diagnostic result; Level is "pass"/"warn"/"fail" and the
// icon is chosen at render time.
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

	// client issues every HTTP probe; nil means http.DefaultClient. Tests
	// inject httptest.Server.Client() so parallel subtests never share
	// DefaultTransport, whose CloseIdleConnections on Server.Close can kill a
	// sibling's pooled connection mid-RoundTrip (#2473).
	client *http.Client

	hasFail  bool
	findings []finding
}

// httpClient returns the probe client, defaulting to http.DefaultClient.
func (d *doctor) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	return http.DefaultClient
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
	// After the findings so section headers don't interleave with the ✓/✗
	// stream; JSON consumers get backend metadata from /api/cli/backends.
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

// renderBackendsSection prints the CLI Backends + Reverse Nodes status block
// (docs/rfc/multi-backend.md §11.2) from static data only (config + Profile
// registry + --version probe) so it works while naozhi.service is down.
// Reverse-node caps are NOT live — they only exist once a node connects — so
// each backend's required caps are listed as a pre-condition instead.
func (d *doctor) renderBackendsSection() {
	// Idempotent sync.Once bootstrap; safe whether or not main registered.
	backend.EnsureDefaults()

	// Missing/malformed config falls back to "what the binary CAN drive" so
	// a fresh install still gets a useful section.
	cfg, cfgErr := config.Load(d.configPath)
	defaultBackend := "claude"
	var cfgBackends []config.CLIBackendConfig
	var cfgReverseNodes map[string]config.ReverseNodeEntry
	if cfgErr == nil && cfg != nil {
		defaultBackend = cfg.DefaultBackendID()
		cfgBackends = cfg.EnabledBackends()
		cfgReverseNodes = cfg.ReverseNodes
	} else {
		// One synthesised entry per registered Profile, in registration order.
		for _, p := range backend.All() {
			cfgBackends = append(cfgBackends, config.CLIBackendConfig{ID: p.ID})
		}
	}

	// Short context so a hung --version cannot freeze doctor.
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	probes := cli.DetectBackendsCtx(ctx)
	probeByID := make(map[string]cli.BackendInfo, len(probes))
	for _, p := range probes {
		probeByID[p.ID] = p
	}

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
		// Unknown ID degrades to "proto=?".
		protoName := "?"
		capsStr := "(unknown)"
		if profileOK {
			proto := profile.NewProtocol(backend.ProtocolDeps{})
			protoName = proto.Name()
			capsStr = formatCapsForDoctor(cli.ProtocolCaps(proto))
		}
		fmt.Fprintf(d.out, "[%s] %s %s  proto=%s  caps=%s\n",
			id, displayName, version, protoName, capsStr)
		// Prefer the probe (walks $PATH), else show the configured override.
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

	fmt.Fprintln(d.out, "=== Reverse Nodes ===")
	if len(cfgReverseNodes) == 0 {
		fmt.Fprintln(d.out, "(no reverse_nodes configured)")
		return
	}
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
		// Live caps are unknown here, so phrase each backend as a pre-condition.
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

// formatCapsForDoctor renders the TRUE Caps flags as a comma-separated list
// in struct field order; empty becomes "(none)" so the line stays parseable.
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

// historyDirForBackend returns the Profile's documented history directory, or
// "(none)" for an unknown backend or one without a HistoryDir. Self-bootstraps
// the registry so it is callable directly from tests.
func historyDirForBackend(id string) string {
	backend.EnsureDefaults()
	if p, ok := backend.Get(id); ok && p.HistoryDir != "" {
		return p.HistoryDir
	}
	return "(none)"
}
