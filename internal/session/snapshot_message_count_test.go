package session

import (
	"testing"
)

// TestSnapshot_MessageCount locks the SessionSnapshot.MessageCount
// population contract:
//   - proc==nil  → 0 (store-restored sessions must not flash a misleading
//     value before the process attaches)
//   - proc!=nil  → value reported by proc.UserTurnCount() (pass-through;
//     EventLog holds the authoritative counter)
//
// Sidebar / main-header renderer gates chip visibility on `> 0`; a broken
// pass-through would silently hide the chip or show zero forever.
func TestSnapshot_MessageCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		procNil   bool
		procCount int64
		wantCount int64
	}{
		{name: "no process yields zero", procNil: true, wantCount: 0},
		{name: "fresh process zero count", procCount: 0, wantCount: 0},
		{name: "single user turn", procCount: 1, wantCount: 1},
		{name: "many user turns", procCount: 142, wantCount: 142},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "platform:direct:chat:general"}
			if !tt.procNil {
				p := newIdleProc()
				p.userTurnCount = tt.procCount
				s.storeProcess(p)
			}
			got := s.Snapshot().MessageCount
			if got != tt.wantCount {
				t.Errorf("Snapshot.MessageCount = %d, want %d", got, tt.wantCount)
			}
		})
	}
}
