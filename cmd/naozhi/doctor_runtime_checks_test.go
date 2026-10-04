package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
	// Exec once untimed: a fresh executable's first launch can be slow on a
	// loaded macOS host, and the probe under test caps --version at 5s.
	_ = exec.Command(p).Run()
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
// binaries: a broken default fails, a broken sibling only warns, and a default
// id with no runtime is graded on the backend startup falls back to.
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
		extra       map[string]string // further categories, same format
		wantHasFail bool
	}{
		{"both_healthy", backends(good, good), "pass 2.1.7 at " + good, "pass 2.1.7 at " + good, nil, false},
		{"sibling_broken", backends(good, bad), "pass 2.1.7", "warn --version failed at " + bad, nil, false},
		{"default_broken_sibling_healthy", backends(bad, good), "fail startup continues on a sibling", "pass 2.1.7", nil, true},
		{"all_broken", backends(bad, bad), "fail startup will refuse to run", "warn --version failed", nil, true},
		{"single_backend", "cli:\n  path: " + bad + "\n", "fail startup will refuse to run", "", nil, true},
		{"unknown_id", "cli:\n  backend: claude\n  backends:\n    - id: claude\n      path: " + good + "\n    - id: kiroo\n",
			"pass 2.1.7", "", map[string]string{"cli backend kiroo": "warn not a registered backend id — startup skips this entry"}, false},
		{"unknown_default_single", "cli:\n  backend: claud\n  path: " + good + "\n", "", "",
			map[string]string{"cli backend claud": "fail not a registered backend id — startup will refuse to run"}, true},
		{"unknown_default_fallback_healthy",
			"cli:\n  backend: claud\n  backends:\n    - id: kiro\n      path: " + good + "\n    - id: claud\n      path: " + good + "\n",
			"", "pass 2.1.7", map[string]string{"cli backend claud": "warn not a registered backend id — default-bound sessions run on kiro instead"}, false},
		{"unknown_default_fallback_broken",
			"cli:\n  backend: claud\n  backends:\n    - id: claud\n    - id: kiro\n      path: " + bad + "\n    - id: claude\n      path: " + good + "\n",
			"pass 2.1.7", "warn --version failed", map[string]string{"cli backend claud": "fail default-bound sessions run on kiro, whose --version failed"}, true},
		{"unknown_default_all_broken",
			"cli:\n  backend: claud\n  backends:\n    - id: claud\n    - id: kiro\n      path: " + bad + "\n",
			"", "warn --version failed", map[string]string{"cli backend claud": "fail startup will refuse to run"}, true},
		{"default_not_listed", "cli:\n  backend: kiro\n  backends:\n    - id: claude\n      path: " + good + "\n",
			"pass 2.1.7", "warn has no cli.backends entry — default-bound sessions run on claude instead", nil, false},
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
			for category, want := range tc.extra {
				check(category, want)
			}
			if n := len(d.findings); n != len(got) {
				t.Errorf("%d findings for %d categories, want one each: %+v", n, len(got), d.findings)
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

// TestDoctor_TranscribeAppliesSettingsEnv: AWS keys the service picks up from
// ~/.claude/settings.json env count for the default credential check too.
func TestDoctor_TranscribeAppliesSettingsEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "aws-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		t.Setenv(k, "") // registers the restore; unset so settings env may fill it
		os.Unsetenv(k)
	}
	settings := `{"env":{"AWS_ACCESS_KEY_ID":"AKIDSETTINGS","AWS_SECRET_ACCESS_KEY":"secret-not-real"}}`
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &doctor{out: &bytes.Buffer{}, timeout: 10 * time.Second,
		configPath: writeDoctorConfig(t, "transcribe:\n  enabled: true\n"),
		ffmpegPath: func() (string, error) { return "/usr/bin/ffmpeg", nil }}
	d.checkTranscribe()
	if f := findingsByCategory(d)["transcribe creds"]; f.Level != "pass" || !strings.Contains(f.Detail, "EnvConfigCredentials") {
		t.Errorf("transcribe creds = %+v, want pass from EnvConfigCredentials", f)
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
	// run() includes checkStateDir, which writes a probe file under ~/.naozhi.
	t.Setenv("HOME", t.TempDir())
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
