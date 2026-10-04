package server

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
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
