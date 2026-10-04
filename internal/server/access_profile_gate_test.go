package server

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// stubAPResolver satisfies accessProfileResolver for the gate test.
type stubAPResolver struct{ profile string }

func (s stubAPResolver) AccessProfileForKey(key string) string { return s.profile }

func TestGateRemoteAccessProfile(t *testing.T) {
	cases := []struct {
		name       string
		resolver   accessProfileResolver
		targetNode string
		wantErr    bool
	}{
		{"local dispatch always ok", stubAPResolver{"1p-fable"}, "", false},
		{"local literal ok", stubAPResolver{"1p-fable"}, "local", false},
		{"nil resolver no-op", nil, "node-a", false},
		{"empty profile remote ok", stubAPResolver{""}, "node-a", false},
		{"non-default profile remote rejected", stubAPResolver{"1p-fable"}, "node-a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gateRemoteAccessProfile(tc.resolver, tc.targetNode, "feishu:user:bob:general")
			if (err != nil) != tc.wantErr {
				t.Fatalf("gateRemoteAccessProfile() err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrAccessProfileRemote) {
				t.Errorf("error should wrap ErrAccessProfileRemote, got %v", err)
			}
		})
	}
}

// An agent pinned to an access profile (#3106) spawns on it locally, so its
// keys must not reach a remote node — even with no project data source.
func TestGateRemoteAccessProfile_AgentProfile(t *testing.T) {
	r := session.NewKeyResolver(map[string]session.AgentOpts{
		"reviewer": {AccessProfile: "personal"},
		"general":  {},
	}, nil)
	for _, key := range []string{"dashboard:direct:1700000000-x:reviewer", "feishu:user:bob:reviewer"} {
		if err := gateRemoteAccessProfile(r, "node-a", key); !errors.Is(err, ErrAccessProfileRemote) {
			t.Errorf("remote dispatch of %q: err = %v, want ErrAccessProfileRemote", key, err)
		}
		if err := gateRemoteAccessProfile(r, "local", key); err != nil {
			t.Errorf("local dispatch of %q: err = %v, want nil", key, err)
		}
	}
	if err := gateRemoteAccessProfile(r, "node-a", "feishu:user:bob:general"); err != nil {
		t.Errorf("agent without a profile: err = %v, want remote OK", err)
	}
}

// A cron job routed to a profile-pinned agent spawns on that profile (#3106),
// so both send paths' gates must hold its key local. buildServer wires a
// job → routed agent → profile lookup into the resolver the engine (HTTP) and
// the Hub (WS) share.
func TestGateRemoteAccessProfile_CronKey(t *testing.T) {
	t.Parallel()
	agentCommands := map[string]string{"review": "reviewer"}
	sched := cron.NewScheduler(cron.SchedulerConfig{MaxJobs: 4, AllowNilRouter: true}, cron.SchedulerDeps{
		Agents:        map[string]cron.AgentOpts{"general": {}, "reviewer": {AccessProfile: "personal"}},
		AgentCommands: agentCommands,
	})
	pinned := &cron.Job{Schedule: "@every 30m", Prompt: "/review the diff", Paused: true}
	plain := &cron.Job{Schedule: "@every 30m", Prompt: "hello", Paused: true}
	for _, j := range []*cron.Job{pinned, plain} {
		if err := sched.AddJob(j); err != nil { // assigns j.ID
			t.Fatalf("AddJob: %v", err)
		}
	}
	srv, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: session.NewRouter(session.RouterConfig{}),
		Backend: "claude", Scheduler: sched, AgentCommands: agentCommands,
		Agents: map[string]session.AgentOpts{"general": {}, "reviewer": {AccessProfile: "personal"}}})
	t.Cleanup(srv.appCancel)
	engine, hub := hs.wiring.engine, srv.hub

	key := sessionkey.CronKey(pinned.ID)
	if err := engine.gateRemoteAccess("node-a", key); !errors.Is(err, ErrAccessProfileRemote) {
		t.Errorf("HTTP gate, remote %q: err = %v, want ErrAccessProfileRemote", key, err)
	}
	if err := gateRemoteAccessProfile(hub.resolver, "node-a", key); !errors.Is(err, ErrAccessProfileRemote) {
		t.Errorf("WS gate, remote %q: err = %v, want ErrAccessProfileRemote", key, err)
	}
	if err := engine.gateRemoteAccess("local", key); err != nil {
		t.Errorf("local %q: err = %v, want nil", key, err)
	}
	for _, k := range []string{sessionkey.CronKey(plain.ID), sessionkey.CronKey("missing")} {
		if err := engine.gateRemoteAccess("node-a", k); err != nil {
			t.Errorf("remote %q: err = %v, want remote OK", k, err)
		}
	}
}
