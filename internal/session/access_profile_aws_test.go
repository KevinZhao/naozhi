package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

const testGlobalCredExport = `"/opt/toolbox/bin/claude" default-credential-export`

// awsProfileRouter is a router whose sessions write real settings files: a
// base naozhi settings file holding the global credential source, a store
// directory, and two profiles, one with its own AWS profile. spawned
// records each spawn's settings file contents, read at spawn time.
func awsProfileRouter(t *testing.T, exe string) (r *Router, spawned func() []map[string]any, procs func() []*fakeProcess) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "naozhi-settings.json")
	if err := os.WriteFile(base, []byte(`{"awsCredentialExport":`+mustJSON(t, testGlobalCredExport)+`,"awsAuthRefresh":"mwinit","model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = NewRouter(RouterConfig{
		MaxProcs: 4,
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
			"kiro":   {Wrapper: cli.NewWrapperLazy("/nonexistent/kiro", &cli.ClaudeProtocol{}, "kiro")},
		},
		DefaultBackend:      "claude",
		StorePath:           filepath.Join(dir, "sessions.json"),
		NaozhiSettingsFile:  base,
		CredentialExportExe: exe,
		AccessProfiles: map[string]AccessProfile{
			"isengard": {AWSProfile: "team-iso", Env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
			"plain":    {Env: map[string]string{"AWS_REGION": "us-east-1"}},
		},
	})
	var mu sync.Mutex
	var docs []map[string]any
	var ps []*fakeProcess
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		doc := map[string]any{}
		if opts.SessionSettingsFile != "" {
			data, err := os.ReadFile(opts.SessionSettingsFile)
			if err != nil {
				t.Errorf("read session settings: %v", err)
			}
			_ = json.Unmarshal(data, &doc)
		}
		p := newIdleProc()
		mu.Lock()
		docs = append(docs, doc)
		ps = append(ps, p)
		mu.Unlock()
		return p, nil
	}
	t.Cleanup(r.Shutdown)
	spawned = func() []map[string]any { mu.Lock(); defer mu.Unlock(); return append([]map[string]any(nil), docs...) }
	procs = func() []*fakeProcess { mu.Lock(); defer mu.Unlock(); return append([]*fakeProcess(nil), ps...) }
	return r, spawned, procs
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A session under a profile with aws_profile gets that profile's credential
// export in its own settings file; a session with no profile, or with a
// profile that has none, keeps the global one. A resume relocks the profile
// the session recorded, so a project re-bound since does not switch accounts.
func TestAccessProfileAWS_CredentialsPerProfile(t *testing.T) {
	const exe = "/opt/naozhi/bin/naozhi"
	r, spawned, procs := awsProfileRouter(t, exe)
	ctx := context.Background()
	want := `"` + exe + `" aws-credential-export --profile team-iso`

	if _, _, err := r.GetOrCreate(ctx, "feishu:direct:iso:general", AgentOpts{AccessProfile: "isengard"}); err != nil {
		t.Fatalf("GetOrCreate isengard: %v", err)
	}
	if _, _, err := r.GetOrCreate(ctx, "feishu:direct:plain:general", AgentOpts{AccessProfile: "plain"}); err != nil {
		t.Fatalf("GetOrCreate plain: %v", err)
	}
	if _, _, err := r.GetOrCreate(ctx, "feishu:direct:none:general", AgentOpts{}); err != nil {
		t.Fatalf("GetOrCreate default: %v", err)
	}
	docs := spawned()
	if len(docs) != 3 {
		t.Fatalf("spawns = %d, want 3", len(docs))
	}
	if got := docs[0]["awsCredentialExport"]; got != want {
		t.Errorf("isengard awsCredentialExport = %v, want %q", got, want)
	}
	if _, ok := docs[0]["awsAuthRefresh"]; ok {
		t.Errorf("isengard kept the global awsAuthRefresh: %v", docs[0])
	}
	if docs[0]["model"] != "opus" {
		t.Errorf("isengard lost the base settings: %v", docs[0])
	}
	for i, name := range map[int]string{1: "plain", 2: "no profile"} {
		if got := docs[i]["awsCredentialExport"]; got != testGlobalCredExport {
			t.Errorf("%s awsCredentialExport = %v, want the global %q", name, got, testGlobalCredExport)
		}
	}

	// The isengard session dies; its project is re-bound to plain.
	p := procs()[0]
	p.mu.Lock()
	p.isAlive = false
	p.mu.Unlock()
	if _, _, err := r.GetOrCreate(ctx, "feishu:direct:iso:general", AgentOpts{AccessProfile: "plain"}); err != nil {
		t.Fatalf("GetOrCreate resume: %v", err)
	}
	docs = spawned()
	if len(docs) != 4 {
		t.Fatalf("spawns after resume = %d, want 4", len(docs))
	}
	if got := docs[3]["awsCredentialExport"]; got != want {
		t.Errorf("resumed awsCredentialExport = %v, want the recorded profile's %q", got, want)
	}
}

// The profile's credentials only reach the Claude CLI, and only through a
// settings file: on another backend, or with no naozhi binary to export with,
// the spawn fails instead of running on the global account.
func TestAccessProfileAWS_FailsLoud(t *testing.T) {
	ctx := context.Background()

	r, spawned, _ := awsProfileRouter(t, "/opt/naozhi/bin/naozhi")
	_, _, err := r.GetOrCreate(ctx, "feishu:direct:k:general", AgentOpts{AccessProfile: "isengard", Backend: "kiro"})
	if err == nil || !strings.Contains(err.Error(), "claude backend") {
		t.Errorf("kiro spawn err = %v, want the claude-backend refusal", err)
	}
	if n := len(spawned()); n != 0 {
		t.Errorf("kiro spawn reached the CLI %d times", n)
	}

	r2, spawned2, _ := awsProfileRouter(t, "")
	if _, _, err := r2.GetOrCreate(ctx, "feishu:direct:e:general", AgentOpts{AccessProfile: "isengard"}); err == nil {
		t.Error("spawn with no credential export executable succeeded")
	}
	if n := len(spawned2()); n != 0 {
		t.Errorf("spawn with no executable reached the CLI %d times", n)
	}
}

func TestAWSCredentialExportFor(t *testing.T) {
	t.Parallel()
	got, err := awsCredentialExportFor("/Users/me/my apps/naozhi", "my-iso_1")
	if err != nil || got != `"/Users/me/my apps/naozhi" aws-credential-export --profile my-iso_1` {
		t.Errorf("got %q, %v", got, err)
	}
	for _, tc := range []struct{ exe, profile string }{
		{"naozhi", "p"},                 // not absolute
		{`/opt/na"ozhi`, "p"},           // breaks the quoting
		{"/opt/$HOME/naozhi", "p"},      // expands inside double quotes
		{"/opt/naozhi", "p; rm -rf ~"},  // not a profile name
		{"/opt/naozhi", "../etc"},       // not a profile name
		{"/opt/naozhi", ""},             // empty
		{"/opt/`id`/naozhi", "profile"}, // command substitution
	} {
		if got, err := awsCredentialExportFor(tc.exe, tc.profile); err == nil {
			t.Errorf("awsCredentialExportFor(%q, %q) = %q, want an error", tc.exe, tc.profile, got)
		}
	}
}
