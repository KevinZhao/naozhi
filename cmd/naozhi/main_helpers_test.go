package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
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

// TestKnownWorkspaceRoots_SkipsEmptyPaths pins the R20260601-CR-5 fix:
// KnownWorkspaceRoots must not emit an empty string when raw contains "".
// filepath.Abs("") succeeds (returns cwd), so without the explicit guard
// an empty input would be canonicalised to cwd and appear in the output.
// The nil-router nil-projectMgr lister returns no entries, so we verify
// the guard is present in source rather than wiring up a full router stub.
func TestKnownWorkspaceRoots_SkipsEmptyPaths(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("main_helpers.go")
	if err != nil {
		t.Fatalf("read main_helpers.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, `if p == "" {`) {
		t.Error(`main_helpers.go KnownWorkspaceRoots missing empty-path guard 'if p == "" { continue }' — filepath.Abs("") returns cwd and would pollute the result`)
	}
}

// TestDashboardWiring_RegistersPprof pins that the server startup path
// still calls registerPprof — the pprof endpoints are defense-in-depth
// for memory / goroutine leak triage and the runbook at
// docs/ops/pprof.md tells operators to curl /api/debug/pprof/*. A PR
// that collapses or renames the wiring would silently break those
// commands; this test catches it.
//
// Reading routes.go source instead of reflection keeps the
// contract narrow: a caller that moves the wiring elsewhere just
// needs to update the test's expected file/token pair.
func TestDashboardWiring_RegistersPprof(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../internal/server/routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "s.registerPprof()") {
		t.Error("internal/server/routes.go must call s.registerPprof() during server startup — docs/ops/pprof.md depends on it")
	}
}

// TestNoParseDurationInMain: config.Load parses every duration once and
// reports an unusable one, so `config check` sees it. A time.ParseDuration
// call here would read a config string again, behind the check's back (#3012).
func TestNoParseDurationInMain(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ParseDuration" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" {
				t.Errorf("%s calls time.ParseDuration; parse the value in internal/config and read its accessor",
					fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if scanned < 10 {
		t.Fatalf("scanned %d files; the walk is not looking at cmd/naozhi", scanned)
	}
}

// TestNoRawCLIBackendInMain: cli.backend is only a request; startup may bind
// another backend (#3298). The router, server footer tag and startup log must
// all name the bound one (bws.DefaultID), so nothing here reads the raw field.
func TestNoRawCLIBackendInMain(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Backend" {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "CLI" {
				t.Errorf("%s reads cfg.CLI.Backend; use bws.DefaultID (what startup bound) or cfg.DefaultBackendID()",
					fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if scanned < 10 {
		t.Fatalf("scanned %d files; the walk is not looking at cmd/naozhi", scanned)
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

// TestMain_LogsProfileDefaultBackends pins that main() emits the notices and
// compares against defaultBackend, the backend startup bound.
func TestMain_LogsProfileDefaultBackends(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "logProfileDefaultBackends" {
			return true
		}
		found = true
		if len(call.Args) != 2 {
			t.Fatalf("%s: logProfileDefaultBackends has %d args", fset.Position(call.Pos()), len(call.Args))
		}
		if arg, ok := call.Args[1].(*ast.Ident); !ok || arg.Name != "defaultBackend" {
			t.Errorf("%s: router default argument must be defaultBackend (bws.DefaultID)", fset.Position(call.Pos()))
		}
		return true
	})
	if !found {
		t.Error("main.go no longer calls logProfileDefaultBackends")
	}
}
