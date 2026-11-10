package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// switchRouter is awsProfileRouter recording each spawn's options as well.
func switchRouter(t *testing.T) (r *Router, spawned func() []map[string]any, procs func() []*fakeProcess, opts func() []cli.SpawnOptions) {
	t.Helper()
	r, spawned, procs = awsProfileRouter(t, "/opt/naozhi/bin/naozhi")
	var mu sync.Mutex
	var seen []cli.SpawnOptions
	prev := r.spawn.hook
	r.spawn.hook = func(ctx context.Context, o cli.SpawnOptions) (processIface, error) {
		mu.Lock()
		seen = append(seen, o)
		mu.Unlock()
		return prev(ctx, o)
	}
	opts = func() []cli.SpawnOptions {
		mu.Lock()
		defer mu.Unlock()
		return append([]cli.SpawnOptions(nil), seen...)
	}
	return r, spawned, procs, opts
}

// A session that talked on the global credentials moves onto a profile with
// its own AWS profile and a new model: the live CLI closes, and the next
// message resumes the same Claude session under the profile's credential
// export and the new model; moving back to "" restores the global ones.
func TestSwitchAccessProfile_RespawnsOnNewCredentials(t *testing.T) {
	r, spawned, procs, opts := switchRouter(t)
	ctx := context.Background()
	const key = "dashboard:direct:sw:general"
	const isoExport = `"/opt/naozhi/bin/naozhi" aws-credential-export --profile team-iso`

	sess, _, err := r.GetOrCreate(ctx, key, AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	sess.setSessionID("cc-sid-1")
	if got := spawned()[0]["awsCredentialExport"]; got != testGlobalCredExport {
		t.Fatalf("first spawn awsCredentialExport = %v, want the global one", got)
	}

	model := "global.anthropic.claude-fable-5-2[1m]"
	via, err := r.SwitchAccessProfile(ctx, key, "isengard", &model)
	if err != nil || via != TuningAppliedRespawn {
		t.Fatalf("switch: via=%q err=%v, want respawn", via, err)
	}
	if procs()[0].Alive() {
		t.Error("the CLI under the old credentials is still running")
	}
	if sess.AccessProfile() != "isengard" || sess.TuningModel() != model {
		t.Errorf("recorded profile=%q model=%q", sess.AccessProfile(), sess.TuningModel())
	}

	// The next message: the caller's own opts carry no profile, as for a
	// dashboard key; the recorded one must win.
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatalf("GetOrCreate after switch: %v", err)
	}
	docs, so := spawned(), opts()
	if len(docs) != 2 {
		t.Fatalf("spawns = %d, want 2", len(docs))
	}
	if got := docs[1]["awsCredentialExport"]; got != isoExport {
		t.Errorf("respawn awsCredentialExport = %v, want %q", got, isoExport)
	}
	if so[1].ResumeID != "cc-sid-1" {
		t.Errorf("respawn ResumeID = %q, want the same Claude session", so[1].ResumeID)
	}
	if so[1].Model != model {
		t.Errorf("respawn Model = %q, want %q", so[1].Model, model)
	}
	if so[1].EnvOverlay["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("respawn env overlay = %v, want the profile's", so[1].EnvOverlay)
	}

	cur := r.SessionFor(key)
	cur.setSessionID("cc-sid-1")
	if via, err := r.SwitchAccessProfile(ctx, key, "", nil); err != nil || via != TuningAppliedRespawn {
		t.Fatalf("switch back: via=%q err=%v", via, err)
	}
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatalf("GetOrCreate after switching back: %v", err)
	}
	docs, so = spawned(), opts()
	if got := docs[2]["awsCredentialExport"]; got != testGlobalCredExport {
		t.Errorf("after switching back awsCredentialExport = %v, want the global one", got)
	}
	if len(so[2].EnvOverlay) != 0 {
		t.Errorf("after switching back env overlay = %v, want none", so[2].EnvOverlay)
	}
	if so[2].ResumeID != "cc-sid-1" || so[2].Model != model {
		t.Errorf("after switching back ResumeID=%q Model=%q", so[2].ResumeID, so[2].Model)
	}
	if got := r.SessionFor(key).AccessProfile(); got != "" {
		t.Errorf("recorded profile = %q, want global", got)
	}
}

// A switch never cuts a turn off, never lands on an unknown profile, and
// never hands a profile's own AWS credentials to a CLI that cannot read them.
func TestSwitchAccessProfile_Refusals(t *testing.T) {
	r, _, procs, _ := switchRouter(t)
	ctx := context.Background()

	const key = "dashboard:direct:busy:general"
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	p := procs()[0]
	p.mu.Lock()
	p.isRunning = true
	p.mu.Unlock()
	if _, err := r.SwitchAccessProfile(ctx, key, "isengard", nil); !errors.Is(err, ErrAccessProfileSwitchBusy) {
		t.Errorf("running turn: err = %v, want ErrAccessProfileSwitchBusy", err)
	}
	if !p.Alive() || r.SessionFor(key).AccessProfile() != "" {
		t.Error("a refused switch still closed the CLI or recorded the profile")
	}

	if _, err := r.SwitchAccessProfile(ctx, key, "nope", nil); !errors.Is(err, ErrAccessProfileUnknown) {
		t.Errorf("unknown profile: err = %v", err)
	}
	if _, err := r.SwitchAccessProfile(ctx, key, "plain", strp("-x")); err == nil {
		t.Error("flag-shaped model accepted")
	}

	const kiroKey = "dashboard:direct:kiro:general"
	if _, _, err := r.GetOrCreate(ctx, kiroKey, AgentOpts{Backend: "kiro"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SwitchAccessProfile(ctx, kiroKey, "isengard", nil); err == nil || !strings.Contains(err.Error(), "claude backend") {
		t.Errorf("kiro session onto an aws_profile profile: err = %v", err)
	}
	if via, err := r.SwitchAccessProfile(ctx, kiroKey, "plain", nil); err != nil || via != TuningAppliedRespawn {
		t.Errorf("kiro session onto an env-only profile: via=%q err=%v", via, err)
	}
}

// A key with no session yet parks the profile and model as the picks its
// first spawn consumes.
func TestSwitchAccessProfile_PendingKey(t *testing.T) {
	r, spawned, _, opts := switchRouter(t)
	ctx := context.Background()
	const key = "dashboard:direct:new:general"
	model := "fable"
	if via, err := r.SwitchAccessProfile(ctx, key, "isengard", &model); err != nil || via != TuningAppliedDeferred {
		t.Fatalf("pending: via=%q err=%v", via, err)
	}
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := spawned()[0]["awsCredentialExport"]; !strings.Contains(got.(string), "team-iso") {
		t.Errorf("first spawn awsCredentialExport = %v", got)
	}
	if got := opts()[0].Model; got != model {
		t.Errorf("first spawn Model = %q, want %q", got, model)
	}
}

// The switched profile outlives a naozhi restart: a suspended session
// restored from the store resumes on it, not on the one it started under.
func TestSwitchAccessProfile_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "sessions.json")
	const key = "dashboard:direct:persist:general"
	cfg := RouterConfig{
		MaxProcs:  4,
		StorePath: store,
		AccessProfiles: map[string]AccessProfile{
			"isengard": {Env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
		},
	}
	r := NewRouter(cfg)
	s := newSessionWithID(key, "cc-sid-9")
	s.storeProcess(newDeadProc())
	putT(r, key, s)
	if via, err := r.SwitchAccessProfile(context.Background(), key, "isengard", nil); err != nil || via != TuningAppliedDeferred {
		t.Fatalf("switch suspended: via=%q err=%v", via, err)
	}
	r.Shutdown()

	r2 := NewRouter(cfg)
	t.Cleanup(r2.Shutdown)
	got := r2.SessionFor(key)
	if got == nil {
		t.Fatal("session not restored")
	}
	if got.AccessProfile() != "isengard" {
		t.Errorf("restored access profile = %q, want isengard", got.AccessProfile())
	}
}
