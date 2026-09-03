package scratch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	r.RegisterCronStub(srcKey, "", "")
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
		Router: r,
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

// inheritSourceTuning is the pure merge behind HandleOpen. Snapshot values
// are CLI-reported, so anything that would fail the router's argv-injection
// gate (e.g. claude echoing a "[1m]" context-window suffix in its init frame,
// or an out-of-set effort tier) must be skipped — falling back to the
// registry default — rather than turning every later send into an
// ErrInvalidModel failure.
func TestInheritSourceTuning_GatesUnsafeValues(t *testing.T) {
	t.Parallel()
	base := session.AgentOpts{Model: "reg-model", Effort: "low", AccessProfile: "reg-profile", ExtraArgs: []string{"--x"}}

	cases := []struct {
		name string
		snap session.SessionSnapshot
		want session.AgentOpts
	}{
		{
			name: "empty snapshot keeps registry defaults",
			snap: session.SessionSnapshot{},
			want: base,
		},
		{
			name: "valid values override",
			snap: session.SessionSnapshot{AccessProfile: "bedrock", Model: "us.anthropic.claude-opus-5", Effort: "high"},
			want: session.AgentOpts{Model: "us.anthropic.claude-opus-5", Effort: "high", AccessProfile: "bedrock", ExtraArgs: []string{"--x"}},
		},
		{
			name: "CLI-reported [1m] suffix fails the model gate and is skipped",
			snap: session.SessionSnapshot{AccessProfile: "bedrock", Model: "us.anthropic.claude-fable-5-1[1m]"},
			want: session.AgentOpts{Model: "reg-model", Effort: "low", AccessProfile: "bedrock", ExtraArgs: []string{"--x"}},
		},
		{
			name: "flag-shaped model is skipped",
			snap: session.SessionSnapshot{Model: "--dangerously-skip-permissions"},
			want: base,
		},
		{
			name: "unknown effort tier is skipped",
			snap: session.SessionSnapshot{Effort: "ultra"},
			want: base,
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
		})
	}
	if base.Model != "reg-model" || base.AccessProfile != "reg-profile" {
		t.Errorf("base was mutated: %+v", base)
	}
}
