package scratch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// #2433 P2: the aside contract is "inherit the source session's agent
// settings", but HandleOpen only carried Agent / Backend / Workspace into
// the pool. A source spawned under a named access profile and/or an
// explicit model produced a scratch on the global default auth chain and
// backend default model. Drive the real HTTP handler against a router stub
// whose snapshot carries both fields and assert the pool's BaseOpts.
func TestHandleOpen_InheritsAccessProfileAndModel(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	const srcKey = "cron:inherit-src"
	r.RegisterCronStubWithChain(srcKey, "", "", nil)
	src := r.SessionFor(srcKey)
	if src == nil {
		t.Fatal("stub source session not registered")
	}
	src.SetAccessProfile("bedrock")
	src.SetModel("us.anthropic.claude-opus-5")
	if snap := src.Snapshot(); snap.AccessProfile != "bedrock" || snap.Model != "us.anthropic.claude-opus-5" {
		t.Fatalf("fixture snapshot = %+v; setters did not land", snap)
	}

	pool := session.NewScratchPool(r, 4, time.Minute)
	h := New(Deps{
		Router: sourceRouter{r},
		Pool:   pool,
		Agents: map[string]session.AgentOpts{"general": {Model: "registry-default"}},
	})

	body := strings.NewReader(`{"source_key":"` + srcKey + `","quote":"why does this fail?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleOpen(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sc := pool.Get(resp.ScratchID)
	if sc == nil {
		t.Fatalf("scratch %q not in pool", resp.ScratchID)
	}
	if sc.BaseOpts.AccessProfile != "bedrock" {
		t.Errorf("BaseOpts.AccessProfile=%q want bedrock (inherited from source)", sc.BaseOpts.AccessProfile)
	}
	if sc.BaseOpts.Model != "us.anthropic.claude-opus-5" {
		t.Errorf("BaseOpts.Model=%q want the source's model, not the registry default", sc.BaseOpts.Model)
	}
}

// An aside runs on its source's account, not on the agent's access profile.
// A planner key resolves to agent "general", whose profile never applies to
// planners; a source recorded on the global default ("") must keep the
// aside there too, or quoted turns land in a session on another account.
func TestHandleOpen_AccessProfileFollowsSourceNotAgent(t *testing.T) {
	cases := []struct {
		name, key, recorded string
	}{
		{"planner on the global default", "project:p:planner", ""},
		{"planner on its project pin", "project:p:planner", "personal"},
		{"chat recorded on the global default", "feishu:direct:alice:general", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := session.NewRouter(session.RouterConfig{MaxProcs: 3})
			r.InjectSession(tc.key, nil).SetAccessProfile(tc.recorded)
			pool := session.NewScratchPool(r, 4, time.Minute)
			h := New(Deps{
				Router: sourceRouter{r},
				Pool:   pool,
				Agents: map[string]session.AgentOpts{"general": {AccessProfile: "company"}},
			})
			body := strings.NewReader(`{"source_key":"` + tc.key + `","quote":"why?"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", body)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.HandleOpen(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var resp openResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			sc := pool.Get(resp.ScratchID)
			if sc == nil {
				t.Fatalf("scratch %q not in pool", resp.ScratchID)
			}
			if sc.BaseOpts.AccessProfile != tc.recorded {
				t.Errorf("BaseOpts.AccessProfile=%q want %q (the source's account, not agent general's)", sc.BaseOpts.AccessProfile, tc.recorded)
			}
		})
	}
}

// A source that never spawned has no backend and resumes on the router
// default; its aside must run there too, not on the inherited profile's
// default_backend, and the response reports that backend.
func TestHandleOpen_UnspawnedSourceBackendIsRouterDefault(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{
		MaxProcs: 3,
		BackendRuntimes: map[string]session.BackendRuntime{
			"claude": {Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
			"kiro":   {Wrapper: cli.NewWrapper("/nonexistent/kiro", &cli.ClaudeProtocol{}, "kiro")},
		},
		DefaultBackend: "claude",
		AccessProfiles: map[string]session.AccessProfile{"viakiro": {DefaultBackend: "kiro"}},
	})
	const srcKey = "feishu:direct:alice:general"
	r.InjectSession(srcKey, nil).SetAccessProfile("viakiro")
	pool := session.NewScratchPool(r, 4, time.Minute)
	h := New(Deps{Router: sourceRouter{r}, Pool: pool})

	body := strings.NewReader(`{"source_key":"` + srcKey + `","quote":"why?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleOpen(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Backend != "claude" {
		t.Errorf("response backend = %q, want claude (the router default)", resp.Backend)
	}
	sc := pool.Get(resp.ScratchID)
	if sc == nil {
		t.Fatalf("scratch %q not in pool", resp.ScratchID)
	}
	if sc.BaseOpts.Backend != "claude" {
		t.Errorf("BaseOpts.Backend = %q, want claude", sc.BaseOpts.Backend)
	}
}

// inheritSourceTuning is the pure merge behind HandleOpen. Snapshot values
// are CLI-reported, so anything that would fail the router's argv-injection
// gate (e.g. a flag-shaped value or an out-of-set effort tier) must be skipped — falling back to the
// registry default — rather than turning every later send into an
// ErrInvalidModel failure. The gate must be the router's own
// (session.ValidateModelID), not a stricter one: Bedrock ARN / inference-
// profile IDs carry ':' and '/' and must still be inherited.
func TestInheritSourceTuning_GatesUnsafeValues(t *testing.T) {
	t.Parallel()
	base := session.AgentOpts{Model: "reg-model", Effort: "low", AccessProfile: "reg-profile", DefaultBackend: "kiro", ExtraArgs: []string{"--x"}}
	// The profile is never a registry default: a source on the global
	// default ("") keeps the aside there.
	keep := base
	keep.AccessProfile = ""

	cases := []struct {
		name string
		snap session.SessionSnapshot
		want session.AgentOpts
	}{
		{
			name: "empty snapshot keeps registry tuning but not the registry profile",
			snap: session.SessionSnapshot{},
			want: keep,
		},
		{
			name: "valid values override",
			snap: session.SessionSnapshot{AccessProfile: "bedrock", Model: "us.anthropic.claude-opus-5", Effort: "high"},
			want: session.AgentOpts{Model: "us.anthropic.claude-opus-5", Effort: "high", AccessProfile: "bedrock", ExtraArgs: []string{"--x"}},
		},
		{
			name: "Bedrock ARN / inference-profile model (':' '/') passes the router gate and is inherited",
			snap: session.SessionSnapshot{Model: "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0"},
			want: session.AgentOpts{Model: "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0", Effort: "low", ExtraArgs: []string{"--x"}},
		},
		{
			// claude's init frame echoes the context-window suffix; the
			// router gate admits it (it is a real model id the CLI parses),
			// so the aside follows its source's 1M-context model.
			name: "CLI-reported [1m] suffix passes the router model gate and is inherited",
			snap: session.SessionSnapshot{AccessProfile: "bedrock", Model: "us.anthropic.claude-fable-5-1[1m]"},
			want: session.AgentOpts{Model: "us.anthropic.claude-fable-5-1[1m]", Effort: "low", AccessProfile: "bedrock", ExtraArgs: []string{"--x"}},
		},
		{
			name: "flag-shaped model is skipped",
			snap: session.SessionSnapshot{Model: "--dangerously-skip-permissions"},
			want: keep,
		},
		{
			name: "unknown effort tier is skipped",
			snap: session.SessionSnapshot{Effort: "ultra"},
			want: keep,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inheritSourceTuning(base, tc.snap)
			if got.Model != tc.want.Model || got.Effort != tc.want.Effort || got.AccessProfile != tc.want.AccessProfile {
				t.Errorf("got {Model:%q Effort:%q AccessProfile:%q} want {Model:%q Effort:%q AccessProfile:%q}",
					got.Model, got.Effort, got.AccessProfile, tc.want.Model, tc.want.Effort, tc.want.AccessProfile)
			}
			if len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "--x" {
				t.Errorf("ExtraArgs must pass through unchanged, got %v", got.ExtraArgs)
			}
			// The aside's backend is its source's (OpenOptions.Backend), never
			// the agent's.
			if got.DefaultBackend != "" {
				t.Errorf("DefaultBackend = %q, want \"\"", got.DefaultBackend)
			}
		})
	}
	if base.Model != "reg-model" || base.AccessProfile != "reg-profile" {
		t.Errorf("base was mutated: %+v", base)
	}
}

// sourceRouter adapts a real router for the handler the way server's
// scratchRouter does in production.
type sourceRouter struct{ *session.Router }

func (s sourceRouter) SessionFor(key string) SourceSession {
	if ms := s.Router.SessionFor(key); ms != nil {
		return ms
	}
	return nil
}
