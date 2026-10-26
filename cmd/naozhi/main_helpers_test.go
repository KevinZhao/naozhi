package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/sysession"
)

// TestSysSessionsWorkDir pins the resolution order the image-orient vision
// runner and the sysession daemon both depend on. Landing their JSONLs in
// this directory (the history panel's SkipWorkspace target) is what keeps
// background/vision sessions out of the history list — a regression that
// pointed either consumer at the user workspace root would leak transcripts
// (and orientation-prompt fragments) into the history panel.
func TestSysSessionsWorkDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		name      string
		override  string
		storePath string
		want      string
	}{
		{
			name:      "explicit_override_wins",
			override:  "/custom/sys",
			storePath: "/data/sessions.json",
			want:      "/custom/sys",
		},
		{
			name:      "sibling_of_store",
			override:  "",
			storePath: "/data/state/sessions.json",
			want:      filepath.Join("/data/state", "sys-sessions"),
		},
		{
			name:      "empty_store_falls_back_to_home",
			override:  "",
			storePath: "",
			want:      filepath.Join(home, ".naozhi", "sys-sessions"),
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sysession.Runner.WorkDir = tc.override
			if got := sysSessionsWorkDir(cfg, tc.storePath); got != tc.want {
				t.Fatalf("sysSessionsWorkDir(override=%q, store=%q) = %q, want %q",
					tc.override, tc.storePath, got, tc.want)
			}
		})
	}
}

func TestChatIDSuffix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short_seven", "oc_abcde", "oc_abcde"}, // ≤8 bytes passes through
		{"exact_eight", "12345678", "12345678"},
		{"nine_chars", "123456789", "…23456789"},
		{"feishu_chat_id", "oc_9dcbfd8307c7a4c1e111f163aa47fd5d", "…aa47fd5d"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chatIDSuffix(tc.in); got != tc.want {
				t.Fatalf("chatIDSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMain_DispatchesDiagnosticSubcommands guards the ops runbook: `naozhi
// doctor` must stay reachable from the CLI. The dispatch is a registry
// (subcmd.go) now, so this asserts the registry entry resolves and points at
// runDoctor — a dropped table row or a rewired run func fails immediately
// instead of at 3am.
func TestMain_DispatchesDiagnosticSubcommands(t *testing.T) {
	t.Parallel()
	sc := findSubcmd("doctor")
	if sc == nil {
		t.Fatal(`registry has no "doctor" entry — runDoctor would be unreachable from the CLI`)
	}
	if got, want := reflect.ValueOf(sc.run).Pointer(), reflect.ValueOf(runDoctor).Pointer(); got != want {
		t.Error(`registry "doctor" entry does not run runDoctor — "naozhi doctor" args would not forward`)
	}
}

// TestShimManagerConfig pins which config value feeds each shim.Manager field;
// every value differs from its default so a swapped or dropped accessor shows.
func TestShimManagerConfig(t *testing.T) {
	stateDir := t.TempDir()
	cfg := loadConfigBody(t, "session:\n  shim:\n    state_dir: "+stateDir+"\n    idle_timeout: 2h\n"+
		"    disconnect_watchdog: 45m\n    buffer_size: 123\n    max_buffer_bytes: 1gb\n    max_shims: 3\n")
	want := shim.ManagerConfig{
		StateDir: stateDir, IdleTimeout: 2 * time.Hour, WatchdogTimeout: 45 * time.Minute,
		BufferSize: 123, MaxBufBytes: 1 << 30, MaxShims: 3,
	}
	if got := shimManagerConfig(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("shimManagerConfig = %+v, want %+v", got, want)
	}
}

// TestSysessionDaemons pins which parsed duration feeds each daemon knob.
func TestSysessionDaemons(t *testing.T) {
	cfg := loadConfigBody(t, "sysession:\n  daemons:\n"+
		"    auto-titler:\n      tick: 1m\n      min_rename_interval: 10m\n"+
		"    attachment-gc:\n      tick: 2h\n      upload_ttl: 36h\n      ref_ttl: 720h\n")
	got := sysessionDaemons(cfg)
	at, gc := got[sysession.DaemonAutoTitler], got[sysession.DaemonAttachmentGC]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"auto-titler tick", at.Tick, time.Minute},
		{"auto-titler min_rename_interval", at.Specific["min_rename_interval"], 10 * time.Minute},
		{"attachment-gc tick", gc.Tick, 2 * time.Hour},
		{"attachment-gc upload_ttl", gc.Specific["upload_ttl"], 36 * time.Hour},
		{"attachment-gc ref_ttl", gc.Specific["ref_ttl"], 720 * time.Hour},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestProfileDefaultBackendNotices(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		profiles       map[string]config.AccessProfile
		defaultProfile string
		want           []profileBackendNotice
	}{
		{name: "no_profiles"},
		{
			name:     "equal_to_router_default_skipped",
			profiles: map[string]config.AccessProfile{"p": {DefaultBackend: "claude"}},
		},
		{
			name:     "empty_default_backend_skipped",
			profiles: map[string]config.AccessProfile{"p": {DefaultModel: "opus"}},
		},
		{
			name:     "plain_profile",
			profiles: map[string]config.AccessProfile{"p": {DefaultBackend: "kiro"}},
			want:     []profileBackendNotice{{Profile: "p", DefaultBackend: "kiro", RouterDefault: "claude"}},
		},
		{
			name:           "default_access_profile",
			profiles:       map[string]config.AccessProfile{"p": {DefaultBackend: "kiro"}},
			defaultProfile: "p",
			want:           []profileBackendNotice{{Profile: "p", DefaultBackend: "kiro", RouterDefault: "claude", IsDefault: true}},
		},
		{
			name: "sorted_by_profile_id",
			profiles: map[string]config.AccessProfile{
				"zeta":  {DefaultBackend: "kiro"},
				"alpha": {DefaultBackend: "codex"},
				"mid":   {DefaultBackend: "claude"},
				"beta":  {DefaultBackend: "kiro"},
			},
			defaultProfile: "beta",
			want: []profileBackendNotice{
				{Profile: "alpha", DefaultBackend: "codex", RouterDefault: "claude"},
				{Profile: "beta", DefaultBackend: "kiro", RouterDefault: "claude", IsDefault: true},
				{Profile: "zeta", DefaultBackend: "kiro", RouterDefault: "claude"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := profileDefaultBackendNotices(tt.profiles, tt.defaultProfile, "claude")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestLogProfileDefaultBackends_Levels checks the default_access_profile line
// is a Warn and any other profile's line an Info, each naming the backend
// and the router default it overrides.
func TestLogProfileDefaultBackends_Levels(t *testing.T) {
	// NOT t.Parallel(): swaps the global slog default.
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cfg := &config.Config{
		AccessProfiles: map[string]config.AccessProfile{
			"viakiro": {DefaultBackend: "kiro"},
			"team":    {DefaultBackend: "codex"},
			"same":    {DefaultBackend: "claude"},
		},
		DefaultAccessProfile: "viakiro",
	}
	logProfileDefaultBackends(cfg, "claude")

	type line struct {
		Level, Msg, DefaultBackend, RouterDefault, Scope string
	}
	var got []line
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Level          string `json:"level"`
			Msg            string `json:"msg"`
			DefaultBackend string `json:"default_backend"`
			RouterDefault  string `json:"router_default"`
			Scope          string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
		if !strings.HasPrefix(rec.Msg, "access_profiles[") {
			continue // another goroutine's line
		}
		got = append(got, line{rec.Level, rec.Msg, rec.DefaultBackend, rec.RouterDefault, rec.Scope})
	}
	want := []line{
		{"INFO", "access_profiles[team].default_backend applies to new sessions under this profile", "codex", "claude",
			"new sessions on keys resolved to this profile"},
		{"WARN", "access_profiles[viakiro].default_backend applies to every new session with no other access profile (it is default_access_profile)", "kiro", "claude",
			"new sessions with no other access_profile and no agent, project or dashboard backend pin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("log lines:\n got %+v\nwant %+v\nraw=%s", got, want, buf.String())
	}
}
