package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeCLI writes an executable that prints out and exits with code.
func writeFakeCLI(t *testing.T, dir, name, out string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI needs a POSIX shell")
	}
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\necho '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeDoctorConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDoctor_CLIBackends runs the startup --version probe against fake
// binaries: a broken default fails, a broken sibling only warns.
func TestDoctor_CLIBackends(t *testing.T) {
	dir := t.TempDir()
	good := writeFakeCLI(t, dir, "good", "2.1.7 (Claude Code)", 0)
	bad := writeFakeCLI(t, dir, "bad", "boom", 1)
	backends := func(claudePath, kiroPath string) string {
		return "cli:\n  backend: claude\n  backends:\n    - id: claude\n      path: " + claudePath +
			"\n    - id: kiro\n      path: " + kiroPath + "\n"
	}
	cases := []struct {
		name        string
		config      string
		wantClaude  string // level + detail substring
		wantKiro    string
		wantHasFail bool
	}{
		{"both_healthy", backends(good, good), "pass 2.1.7 at " + good, "pass 2.1.7 at " + good, false},
		{"sibling_broken", backends(good, bad), "pass 2.1.7", "warn --version failed at " + bad, false},
		{"default_broken_sibling_healthy", backends(bad, good), "fail startup continues on a sibling", "pass 2.1.7", true},
		{"all_broken", backends(bad, bad), "fail startup will refuse to run", "warn --version failed", true},
		{"single_backend", "cli:\n  path: " + bad + "\n", "fail startup will refuse to run", "", true},
		{"unknown_id", "cli:\n  backend: claude\n  backends:\n    - id: claude\n      path: " + good + "\n    - id: kiroo\n", "pass 2.1.7", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &doctor{out: &bytes.Buffer{}, timeout: 5 * time.Second, configPath: writeDoctorConfig(t, tc.config)}
			d.checkCLIBackends()
			got := findingsByCategory(d)
			check := func(category, want string) {
				t.Helper()
				f, ok := got[category]
				if want == "" {
					if ok {
						t.Errorf("%s = %+v, want no finding", category, f)
					}
					return
				}
				level, detail, _ := strings.Cut(want, " ")
				if !ok || f.Level != level || !strings.Contains(f.Detail, detail) {
					t.Errorf("%s = %+v, want %s containing %q", category, f, level, detail)
				}
			}
			check("cli backend claude", tc.wantClaude)
			check("cli backend kiro", tc.wantKiro)
			if tc.name == "unknown_id" {
				check("cli backend kiroo", "warn not a registered backend id")
			}
			if d.hasFail != tc.wantHasFail {
				t.Errorf("hasFail = %v, want %v; findings %+v", d.hasFail, tc.wantHasFail, d.findings)
			}
		})
	}
}

func TestDoctor_CLIBackendsSkipsWithoutConfig(t *testing.T) {
	t.Parallel()
	d := &doctor{out: &bytes.Buffer{}, timeout: time.Second, configPath: filepath.Join(t.TempDir(), "missing.yaml")}
	d.checkCLIBackends()
	if len(d.findings) != 1 || d.findings[0].Category != "cli backend" || !strings.HasPrefix(d.findings[0].Detail, "skipped") {
		t.Errorf("findings = %+v, want one skipped cli backend line", d.findings)
	}
}

// TestDoctor_Transcribe covers the credential and ffmpeg lines through
// injected checks.
func TestDoctor_Transcribe(t *testing.T) {
	t.Parallel()
	enabled := "transcribe:\n  enabled: true\n  region: eu-west-1\n"
	okCreds := func(_ context.Context, region string) (string, error) { return "EnvConfigCredentials@" + region, nil }
	noCreds := func(context.Context, string) (string, error) { return "", errors.New("no EC2 IMDS role found") }
	okFFmpeg := func() (string, error) { return "/usr/bin/ffmpeg", nil }
	noFFmpeg := func() (string, error) { return "", errors.New(`exec: "ffmpeg": executable file not found in $PATH`) }
	cases := []struct {
		name               string
		config             string
		creds              func(context.Context, string) (string, error)
		ffmpeg             func() (string, error)
		wantCreds, wantBin string
	}{
		{"healthy", enabled, okCreds, okFFmpeg, "pass AWS credentials from EnvConfigCredentials@eu-west-1", "pass /usr/bin/ffmpeg"},
		{"no_creds", enabled, noCreds, okFFmpeg, "warn no EC2 IMDS role found", "pass /usr/bin/ffmpeg"},
		{"no_ffmpeg", enabled, okCreds, noFFmpeg, "pass AWS credentials", "warn other than ogg/flac/pcm cannot be transcribed"},
		{"disabled", "transcribe:\n  enabled: false\n", noCreds, noFFmpeg, "pass skipped (transcribe disabled)", "pass skipped (transcribe disabled)"},
		{"absent", "cli:\n  backend: claude\n", noCreds, noFFmpeg, "pass skipped (transcribe disabled)", "pass skipped (transcribe disabled)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &doctor{out: &bytes.Buffer{}, timeout: time.Second, configPath: writeDoctorConfig(t, tc.config),
				awsCredentials: tc.creds, ffmpegPath: tc.ffmpeg}
			d.checkTranscribe()
			got := findingsByCategory(d)
			for category, want := range map[string]string{"transcribe creds": tc.wantCreds, "transcribe ffmpeg": tc.wantBin} {
				level, detail, _ := strings.Cut(want, " ")
				if f := got[category]; f.Level != level || !strings.Contains(f.Detail, detail) {
					t.Errorf("%s = %+v, want %s containing %q", category, f, level, detail)
				}
			}
			if d.hasFail {
				t.Errorf("transcribe findings must never fail; got %+v", d.findings)
			}
		})
	}
}

// TestDoctor_LoadConfigOnce: every config-reading check shares one Load, so a
// config removed mid-run is still the one the report describes.
func TestDoctor_LoadConfigOnce(t *testing.T) {
	t.Parallel()
	cfgPath := writeDoctorConfig(t, "transcribe:\n  enabled: true\n")
	d := &doctor{out: &bytes.Buffer{}, timeout: time.Second, configPath: cfgPath,
		awsCredentials: func(context.Context, string) (string, error) { return "stub", nil },
		ffmpegPath:     func() (string, error) { return "/usr/bin/ffmpeg", nil }}
	if _, err := d.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	d.checkTranscribe()
	d.checkServerSecurity()
	got := findingsByCategory(d)
	if f := got["transcribe creds"]; f.Level != "pass" || f.Detail != "AWS credentials from stub" {
		t.Errorf("transcribe creds = %+v, want the memoised config's enabled transcribe", f)
	}
	if f := got["server security"]; strings.Contains(f.Detail, "config not loaded") {
		t.Errorf("server security = %+v, want it to reuse the memoised config", f)
	}
}

// TestDoctor_RunIncludesRuntimeChecks: run() wires the CLI and transcribe
// checks into the report.
func TestDoctor_RunIncludesRuntimeChecks(t *testing.T) {
	good := writeFakeCLI(t, t.TempDir(), "good", "2.1.7", 0)
	cfgPath := writeDoctorConfig(t, "cli:\n  path: "+good+"\ntranscribe:\n  enabled: false\n")
	srv, _ := authHealthServer(t, `{"status":"ok"}`)
	d := &doctor{addr: srv.URL, client: srv.Client(), out: &bytes.Buffer{}, json: true,
		timeout: 2 * time.Second, configPath: cfgPath}
	d.run()
	got := findingsByCategory(d)
	for _, c := range []string{"cli backend claude", "transcribe creds", "transcribe ffmpeg"} {
		if _, ok := got[c]; !ok {
			t.Errorf("run() emitted no %q finding; findings %+v", c, d.findings)
		}
	}
}
