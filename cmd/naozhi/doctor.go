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
	"strings"
	"time"
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

	// health memoises the run's single GET /health; see fetchHealth.
	health *healthReply
	// config memoises the run's single config.Load; see loadConfig.
	config *doctorConfig
	// backends memoises the run's backend --version probes; see probeBackends.
	backends *[]backendProbe

	// awsCredentials and ffmpegPath default to the transcribe package's
	// checks; tests inject stubs.
	awsCredentials func(ctx context.Context, region string) (string, error)
	ffmpegPath     func() (string, error)

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
	d.checkCodesign()
	d.checkSystemd()
	d.checkHealth()
	d.checkAuth()
	d.checkServerState()
	d.checkConfigDrift()
	d.checkStateDir()
	d.checkCLIBackends()
	d.checkTranscribe()
	d.checkZeroDowntimeScopes()
	d.checkServerSecurity()
	d.checkIMAccess()
	d.render()
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
