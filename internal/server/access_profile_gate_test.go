package server

import (
	"errors"
	"testing"
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
