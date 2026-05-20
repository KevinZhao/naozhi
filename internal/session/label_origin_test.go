package session

import (
	"sync"
	"testing"
)

// TestSetUserLabelWithOrigin_Basic exercises the simple flows of the
// daemon-aware label setter (RFC v2.1 §7.3 / §11.1).
func TestSetUserLabelWithOrigin_Basic(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.sessions["feishu:direct:u1:general"] = &ManagedSession{key: "feishu:direct:u1:general"}

	// auto write on a fresh session (origin "") is allowed because UserLabel
	// is empty — empty origin + empty label means "nobody set it yet".
	if !r.SetUserLabelWithOrigin("feishu:direct:u1:general", "auto title", "auto") {
		t.Fatal("first auto write should succeed on empty session")
	}
	s := r.sessions["feishu:direct:u1:general"]
	if got := s.UserLabel(); got != "auto title" {
		t.Errorf("UserLabel = %q, want %q", got, "auto title")
	}
	if got := s.LabelOrigin(); got != "auto" {
		t.Errorf("LabelOrigin = %q, want %q", got, "auto")
	}

	// User overwrite via SetUserLabel sets origin="user".
	if !r.SetUserLabel("feishu:direct:u1:general", "manual title") {
		t.Fatal("user write should succeed")
	}
	if got := s.LabelOrigin(); got != "user" {
		t.Errorf("after SetUserLabel, LabelOrigin = %q, want %q", got, "user")
	}

	// Daemon must not overwrite a user-origin label.
	if r.SetUserLabelWithOrigin("feishu:direct:u1:general", "robot title", "auto") {
		t.Error("daemon should be rejected when origin is 'user'")
	}
	if got := s.UserLabel(); got != "manual title" {
		t.Errorf("UserLabel after rejected daemon write = %q, want %q (preserved)", got, "manual title")
	}

	// ClearUserLabelOrigin lets the daemon take back over. Both label and
	// origin are cleared so the legacy "empty origin = user-set" rule
	// remains unambiguous (RFC v2.1 §7.3).
	if !r.ClearUserLabelOrigin("feishu:direct:u1:general") {
		t.Fatal("ClearUserLabelOrigin should succeed for known key")
	}
	if got := s.LabelOrigin(); got != "" {
		t.Errorf("after Clear, LabelOrigin = %q, want empty", got)
	}
	if got := s.UserLabel(); got != "" {
		t.Errorf("after Clear, UserLabel = %q, want empty (label is cleared along with origin)", got)
	}
	// Now the daemon can write again.
	if !r.SetUserLabelWithOrigin("feishu:direct:u1:general", "robot retake", "auto") {
		t.Error("daemon should succeed after ClearUserLabelOrigin")
	}
	if got := s.LabelOrigin(); got != "auto" {
		t.Errorf("after daemon retake, LabelOrigin = %q, want %q", got, "auto")
	}
}

// TestSetUserLabelWithOrigin_LegacyEmptyTreatedAsUser locks the
// backward-compat rule: a session with non-empty UserLabel but empty
// LabelOrigin (legacy / pre-v2.1 store) must be treated as user-set so
// daemons do not silently overwrite it after upgrade.
func TestSetUserLabelWithOrigin_LegacyEmptyTreatedAsUser(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	s := &ManagedSession{key: "feishu:direct:u1:general"}
	s.SetUserLabel("legacy-set-label")
	// LabelOrigin intentionally left empty (simulates pre-v2.1 store entry).
	r.sessions[s.key] = s

	if r.SetUserLabelWithOrigin(s.key, "robot", "auto") {
		t.Error("daemon write should be rejected when legacy non-empty label has empty origin")
	}
	if got := s.UserLabel(); got != "legacy-set-label" {
		t.Errorf("legacy label preserved? got %q", got)
	}
}

// TestSetUserLabelWithOrigin_UnknownKey reports false without mutation.
func TestSetUserLabelWithOrigin_UnknownKey(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	if r.SetUserLabelWithOrigin("nonexistent:key:1:x", "x", "auto") {
		t.Error("unknown key must return false")
	}
	if r.ClearUserLabelOrigin("nonexistent:key:1:x") {
		t.Error("ClearUserLabelOrigin on unknown key must return false")
	}
}

// TestSetUserLabelWithOrigin_RaceWindow exercises the §11.1 invariant
// under -race: many goroutines race a daemon (origin="auto") against a
// human (origin="user"). The user's write must never be silently
// overwritten by a concurrent daemon write — the daemon path must
// re-read origin under r.mu.
func TestSetUserLabelWithOrigin_RaceWindow(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	key := "feishu:direct:u1:general"
	r.sessions[key] = &ManagedSession{key: key}

	const iterations = 200
	var wg sync.WaitGroup

	// Daemon writers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				r.SetUserLabelWithOrigin(key, "robot", "auto")
			}
		}()
	}
	// Human writers (alternating set/clear so daemon has windows)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				r.SetUserLabel(key, "human")
				r.ClearUserLabelOrigin(key)
			}
		}()
	}
	wg.Wait()

	// Final state can be either, but it must be self-consistent: if origin
	// is "user" the label must be "human"; if origin is "auto" the label
	// must be "robot"; if origin is "" the label can be anything (legacy
	// path Clear just hit). What we MUST NOT see is origin="user" but
	// label="robot" (= silent daemon overwrite of human edit).
	s := r.sessions[key]
	origin := s.LabelOrigin()
	label := s.UserLabel()
	if origin == "user" && label != "human" {
		t.Errorf("invariant violated: origin=user but label=%q (daemon won a race)", label)
	}
	if origin == "auto" && label != "robot" {
		t.Errorf("invariant violated: origin=auto but label=%q", label)
	}
}

// TestRegisterSystemStub_HappyPath verifies the system-stub registration
// path mirrors RegisterCronStub's behaviour minus the chain.
func TestRegisterSystemStub_HappyPath(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	key := SysKey("test-daemon")
	r.RegisterSystemStub(key, "/tmp/work", "initial prompt")

	s, ok := r.sessions[key]
	if !ok {
		t.Fatal("RegisterSystemStub did not insert session")
	}
	if !s.IsExempt() {
		t.Error("system stub must be exempt")
	}
	if got := s.Workspace(); got != "/tmp/work" {
		t.Errorf("workspace = %q, want %q", got, "/tmp/work")
	}
}

// TestRegisterSystemStub_PanicOnWrongPrefix asserts that misuse fails
// loudly (RFC v2.1 §8.1 — panic over silent return).
func TestRegisterSystemStub_PanicOnWrongPrefix(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	defer func() {
		if recover() == nil {
			t.Error("expected panic when key is not sys: prefix")
		}
	}()
	r.RegisterSystemStub("cron:wrong", "/w", "p")
}

// TestRegisterCronStub_PanicOnWrongPrefix locks the symmetric behaviour
// for cron stubs (also panics on misuse rather than silent return).
func TestRegisterCronStub_PanicOnWrongPrefix(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	defer func() {
		if recover() == nil {
			t.Error("expected panic when key is not cron: prefix")
		}
	}()
	r.RegisterCronStub("sys:wrong", "/w", "p")
}

// TestVisitSessions_StreamingFilter verifies the iterator visits all
// sessions and respects an early-stop return value.
func TestVisitSessions_StreamingFilter(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	keys := []string{
		"feishu:direct:a:general",
		"feishu:direct:b:general",
		"feishu:direct:c:general",
		"sys:auto-titler",
	}
	for _, k := range keys {
		r.sessions[k] = &ManagedSession{key: k}
	}

	// Visit all
	visited := map[string]bool{}
	r.VisitSessions(func(s SessionSnapshot) bool {
		visited[s.Key] = true
		return true
	})
	if len(visited) != len(keys) {
		t.Errorf("visited %d sessions, want %d", len(visited), len(keys))
	}

	// Early stop after first
	count := 0
	r.VisitSessions(func(s SessionSnapshot) bool {
		count++
		return false
	})
	if count != 1 {
		t.Errorf("early-stop visit count = %d, want 1", count)
	}
}
