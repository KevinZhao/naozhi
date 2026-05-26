// R222-ARCH-10 (#728) Role / Mode classification tests.
//
// These pin the derived semantics of Role()/Mode() against the
// underlying implicit fields (exempt + key prefix + process
// liveness). A future refactor that changes the underlying
// representation must keep these passing — and a future refactor
// that switches call sites from direct-field-read to Role()/Mode()
// can rely on these tests as the contract surface.
package session

import (
	"testing"
)

// TestSessionRole_DerivedFromKeyPrefix pins the four-way switch on
// key namespace. A new namespace landing must extend Role() AND
// add a row here.
func TestSessionRole_DerivedFromKeyPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want SessionRole
	}{
		{"cron prefix", "cron:job-7", RoleCron},
		{"sys prefix", "sys:auto-titler", RoleSys},
		// ScratchKeyPrefix is "scratch:"; pin against the real
		// constant so the test stays in sync if the namespace
		// is ever renamed.
		{"scratch prefix", ScratchKeyPrefix + "abc123", RoleScratch},
		{"feishu user", "feishu:p2p:user-7:default", RoleUser},
		{"empty key", "", RoleUser},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: tc.key}
			if got := s.Role(); got != tc.want {
				t.Errorf("Role(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestSessionRole_StringStable pins the lowercase string forms so
// future log/metrics consumers can rely on them.
func TestSessionRole_StringStable(t *testing.T) {
	t.Parallel()
	want := map[SessionRole]string{
		RoleUser:    "user",
		RoleCron:    "cron",
		RoleSys:     "sys",
		RoleScratch: "scratch",
	}
	for r, w := range want {
		if got := r.String(); got != w {
			t.Errorf("(%v).String() = %q, want %q", r, got, w)
		}
	}
}

// TestSessionMode_StubVsPausedVsActive pins the priority rule:
// exempt + dead = stub; non-exempt + dead = paused; alive = active.
func TestSessionMode_StubVsPausedVsActive(t *testing.T) {
	t.Parallel()

	// Stub: exempt + no process.
	stub := &ManagedSession{key: "cron:job-1", exempt: true}
	if got := stub.Mode(); got != ModeStub {
		t.Errorf("exempt+nil-process: Mode() = %v, want %v", got, ModeStub)
	}

	// Paused: non-exempt + no process.
	paused := &ManagedSession{key: "feishu:p2p:user:default"}
	if got := paused.Mode(); got != ModePaused {
		t.Errorf("non-exempt+nil-process: Mode() = %v, want %v", got, ModePaused)
	}

	// Active: process alive (use the existing fakeProcess test helper).
	active := &ManagedSession{key: "feishu:p2p:user:default"}
	active.storeProcess(&fakeProcess{isAlive: true})
	if got := active.Mode(); got != ModeActive {
		t.Errorf("alive process: Mode() = %v, want %v", got, ModeActive)
	}
}

// TestSessionMode_StringStable pins the stub/paused/active vocabulary.
func TestSessionMode_StringStable(t *testing.T) {
	t.Parallel()
	want := map[SessionMode]string{
		ModeActive: "active",
		ModePaused: "paused",
		ModeStub:   "stub",
	}
	for m, w := range want {
		if got := m.String(); got != w {
			t.Errorf("(%v).String() = %q, want %q", m, got, w)
		}
	}
}
