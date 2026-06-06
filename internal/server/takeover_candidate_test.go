package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestPickTakeoverCandidate pins the pure matching policy extracted out of
// tryAutoTakeover for ARCH-SVR-2 (#460): exact CWD match, newest LastActive
// wins, and no candidate (nil) when nothing matches or the workspace is empty.
func TestPickTakeoverCandidate(t *testing.T) {
	ds := func(sid, cwd string, lastActive int64) discovery.DiscoveredSession {
		return discovery.DiscoveredSession{SessionID: sid, CWD: cwd, LastActive: lastActive}
	}

	tests := []struct {
		name       string
		discovered []discovery.DiscoveredSession
		workspace  string
		wantSID    string // "" means expect nil
	}{
		{
			name:      "empty workspace never matches",
			workspace: "",
			discovered: []discovery.DiscoveredSession{
				ds("a", "/w", 10),
			},
			wantSID: "",
		},
		{
			name:       "no discovered sessions",
			workspace:  "/w",
			discovered: nil,
			wantSID:    "",
		},
		{
			name:      "no cwd match",
			workspace: "/w",
			discovered: []discovery.DiscoveredSession{
				ds("a", "/other", 10),
			},
			wantSID: "",
		},
		{
			name:      "single exact match",
			workspace: "/w",
			discovered: []discovery.DiscoveredSession{
				ds("a", "/w", 10),
			},
			wantSID: "a",
		},
		{
			name:      "newest LastActive among matches wins",
			workspace: "/w",
			discovered: []discovery.DiscoveredSession{
				ds("old", "/w", 5),
				ds("new", "/w", 50),
				ds("mid", "/w", 20),
			},
			wantSID: "new",
		},
		{
			name:      "non-matching cwd ignored even when newer",
			workspace: "/w",
			discovered: []discovery.DiscoveredSession{
				ds("match", "/w", 10),
				ds("newer-other", "/other", 99),
			},
			wantSID: "match",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickTakeoverCandidate(tc.discovered, tc.workspace)
			if tc.wantSID == "" {
				if got != nil {
					t.Fatalf("pickTakeoverCandidate = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("pickTakeoverCandidate = nil, want session %q", tc.wantSID)
			}
			if got.SessionID != tc.wantSID {
				t.Fatalf("pickTakeoverCandidate = %q, want %q", got.SessionID, tc.wantSID)
			}
		})
	}
}
