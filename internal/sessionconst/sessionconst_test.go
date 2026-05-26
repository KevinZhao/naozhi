package sessionconst_test

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionconst"
)

// TestDefaultsMatchSessionPackage pins the contract that session.DefaultX
// values are still byte-identical to sessionconst.DefaultX. R222-ARCH-3
// extracted the literals into sessionconst so internal/config can read them
// without reverse-importing internal/session; if a future refactor splits
// the values apart, code paths that assume the two are the same (config
// applyDefaults vs router NewRouter) will silently disagree on the cap.
func TestDefaultsMatchSessionPackage(t *testing.T) {
	if sessionconst.DefaultMaxProcs != session.DefaultMaxProcs {
		t.Errorf("DefaultMaxProcs drift: sessionconst=%d session=%d",
			sessionconst.DefaultMaxProcs, session.DefaultMaxProcs)
	}
	if sessionconst.DefaultTTL != session.DefaultTTL {
		t.Errorf("DefaultTTL drift: sessionconst=%v session=%v",
			sessionconst.DefaultTTL, session.DefaultTTL)
	}
	if sessionconst.DefaultPruneTTL != session.DefaultPruneTTL {
		t.Errorf("DefaultPruneTTL drift: sessionconst=%v session=%v",
			sessionconst.DefaultPruneTTL, session.DefaultPruneTTL)
	}
}

// TestKnownDefaults guards the values themselves so an accidental edit
// in either sessionconst.go or router_core.go that happened to drift
// both copies in lock-step would still get caught.
func TestKnownDefaults(t *testing.T) {
	if sessionconst.DefaultMaxProcs != 3 {
		t.Errorf("DefaultMaxProcs = %d, want 3", sessionconst.DefaultMaxProcs)
	}
	if sessionconst.DefaultTTL != 30*time.Minute {
		t.Errorf("DefaultTTL = %v, want 30m", sessionconst.DefaultTTL)
	}
	if sessionconst.DefaultPruneTTL != 72*time.Hour {
		t.Errorf("DefaultPruneTTL = %v, want 72h", sessionconst.DefaultPruneTTL)
	}
}
