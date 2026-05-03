package session

import (
	"strings"
	"testing"
)

func TestValidateUserLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty passes", "", "", false},
		{"whitespace-only passes", "   ", "", false},
		{"ASCII label", "hello", "hello", false},
		{"Chinese label", "重构会话", "重构会话", false},
		{"trims surrounding space", "  hi  ", "hi", false},
		{"128 bytes exact", strings.Repeat("a", MaxUserLabelBytes), strings.Repeat("a", MaxUserLabelBytes), false},
		{"129 bytes rejected", strings.Repeat("a", MaxUserLabelBytes+1), "", true},
		{"tab rejected (slog separator)", "a\tb", "", true},
		{"newline rejected", "a\nb", "", true},
		{"CR rejected", "a\rb", "", true},
		{"NUL rejected", "a\x00b", "", true},
		{"DEL rejected", "a\x7fb", "", true},
		{"C1 control rejected (U+0085 NEL)", "a\u0085b", "", true},
		{"C1 control rejected (U+009F)", "a\u009fb", "", true},
		{"escape rejected", "a\x1bb", "", true},
		{"invalid utf-8 rejected", "\xc3\x28", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateUserLabel(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got = %q, want = %q", got, c.want)
			}
		})
	}
}

// TestSetUserLabel_NotifiesChange verifies that a label mutation triggers the
// onChange broadcast so connected dashboards refresh immediately instead of
// waiting up to one poll interval. R64-GO-H1 regression.
func TestSetUserLabel_NotifiesChange(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	sk := "feishu:direct:test_user:default"
	injectSession(r, sk, nil)

	var notified int
	r.SetOnChange(func() { notified++ })

	if !r.SetUserLabel(sk, "custom") {
		t.Fatalf("SetUserLabel returned false for registered session")
	}
	if notified == 0 {
		t.Fatalf("expected onChange to fire after SetUserLabel, did not")
	}
	if got := r.GetSession(sk).UserLabel(); got != "custom" {
		t.Errorf("UserLabel = %q, want %q", got, "custom")
	}
}

// TestSetUserLabel_UnknownKeyNoNotify verifies that SetUserLabel on a missing
// key is a true no-op — no onChange fires, so the dashboard doesn't poll
// uselessly on typos from an RPC client. R64-GO-H1.
func TestSetUserLabel_UnknownKeyNoNotify(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	var notified int
	r.SetOnChange(func() { notified++ })
	if r.SetUserLabel("missing:key:here:agent", "x") {
		t.Fatalf("SetUserLabel returned true for unknown key")
	}
	if notified != 0 {
		t.Fatalf("onChange fired %d times for unknown-key SetUserLabel", notified)
	}
}

// TestSetUserLabel_NoOpOnSameValue pins the R176-PERF-P1 contract: calling
// SetUserLabel with a label that equals the current value must skip the
// dirty flag + storeGen bump + onChange broadcast. A dashboard that sends a
// blur-without-edit otherwise triggers a 2-5 ms fsync on the next
// saveIfDirty tick and a sessions_update fanout to every connected client.
func TestSetUserLabel_NoOpOnSameValue(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	sk := "feishu:direct:test_user:default"
	injectSession(r, sk, nil)

	// First mutation primes state and fires one notify.
	var notified int
	r.SetOnChange(func() { notified++ })
	if !r.SetUserLabel(sk, "custom") {
		t.Fatalf("SetUserLabel returned false on registered session")
	}
	if notified != 1 {
		t.Fatalf("notified after first SetUserLabel = %d, want 1", notified)
	}
	genAfterFirst := r.storeGen.Load()
	r.mu.RLock()
	dirtyAfterFirst := r.storeDirty
	r.mu.RUnlock()
	if !dirtyAfterFirst {
		t.Fatalf("storeDirty should be true after first SetUserLabel")
	}
	// Simulate a saveIfDirty cycle clearing the flag.
	r.mu.Lock()
	r.storeDirty = false
	r.mu.Unlock()

	// Second mutation with the SAME value must be a pure no-op: no notify,
	// no dirty flip, no storeGen bump.
	if !r.SetUserLabel(sk, "custom") {
		t.Fatalf("SetUserLabel returned false on same-value store")
	}
	if notified != 1 {
		t.Errorf("onChange fired on same-value SetUserLabel: notified = %d, want 1", notified)
	}
	r.mu.RLock()
	dirtyAfterSecond := r.storeDirty
	r.mu.RUnlock()
	if dirtyAfterSecond {
		t.Errorf("storeDirty flipped on same-value SetUserLabel")
	}
	if got := r.storeGen.Load(); got != genAfterFirst {
		t.Errorf("storeGen advanced on same-value SetUserLabel: got %d, want %d", got, genAfterFirst)
	}

	// Third mutation with a DIFFERENT value must resume the full dirty +
	// notify path — the no-op fast path must not starve legitimate writes.
	if !r.SetUserLabel(sk, "renamed") {
		t.Fatalf("SetUserLabel returned false on follow-up distinct-value store")
	}
	if notified != 2 {
		t.Errorf("onChange fired %d times after distinct-value SetUserLabel, want 2", notified)
	}
	r.mu.RLock()
	dirtyAfterThird := r.storeDirty
	r.mu.RUnlock()
	if !dirtyAfterThird {
		t.Errorf("storeDirty should be true after distinct-value SetUserLabel")
	}
	if got := r.storeGen.Load(); got == genAfterFirst {
		t.Errorf("storeGen did not advance on distinct-value SetUserLabel: got %d, want > %d", got, genAfterFirst)
	}
	if got := r.GetSession(sk).UserLabel(); got != "renamed" {
		t.Errorf("UserLabel = %q, want %q", got, "renamed")
	}
}

// TestBumpVersion_NotifiesAndIncrements locks in R68-GO-M1: BumpVersion
// must both (a) advance storeGen so the dashboard's poll-time version gate
// sees a mutation, and (b) invoke the onChange callback so connected
// WebSocket clients receive an immediate sessions_update push. Prior to
// this fix, callers (project favorite toggle) only saw a refresh on the
// next 5s poll tick despite the bump.
func TestBumpVersion_NotifiesAndIncrements(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	var notified int
	r.SetOnChange(func() { notified++ })
	before := r.Version()
	r.BumpVersion()
	if after := r.Version(); after <= before {
		t.Errorf("Version did not advance: before=%d after=%d", before, after)
	}
	if notified == 0 {
		t.Errorf("expected onChange to fire on BumpVersion")
	}
}
