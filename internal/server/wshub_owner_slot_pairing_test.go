package server

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// tokenOwnerKey mirrors handleAuth's R247-SEC-16 derivation so tests can
// assert the per-owner counter is keyed by the same value.
func tokenOwnerKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// TestHandleAuth_TokenRekeysOwnerSlot pins #1775: in token mode the conn is
// upgraded with an empty uploadOwner, so reserveOwnerSlot at upgrade time is a
// no-op (owner == ""). When handleAuth derives the real owner from the token it
// must re-key the reservation — release(old="") then reserve(new) — so that the
// per-owner counter is incremented under the SAME owner that the teardown path
// (releaseOwnerSlot(c.uploadOwnerKey())) later decrements. Before the fix the
// slot was reserved against "" but released against the token-derived owner,
// driving the per-owner counter negative on disconnect (or, over reconnects,
// wedging the cap with phantom slots).
func TestHandleAuth_TokenRekeysOwnerSlot(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()

	c := &wsClient{
		send:          make(chan []byte, 4),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	// Simulate the upgrade-time reservation: token-mode pre-auth owner is "".
	if !hub.reserveOwnerSlot(c.uploadOwnerKey()) {
		t.Fatal("upgrade-time reserve for empty owner must succeed")
	}

	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})

	owner := tokenOwnerKey("secret")
	if c.uploadOwnerKey() != owner {
		t.Fatalf("uploadOwner = %q, want %q", c.uploadOwnerKey(), owner)
	}
	hub.connCountByOwnerMu.Lock()
	got := hub.connCountByOwner[owner]
	hub.connCountByOwnerMu.Unlock()
	if got != 1 {
		t.Fatalf("after auth re-key, per-owner count = %d, want 1 (reserve must follow the owner)", got)
	}

	// Teardown path releases against the CURRENT owner; the counter must
	// settle back to zero (entry removed) rather than going negative.
	hub.releaseOwnerSlot(c.uploadOwnerKey())
	hub.connCountByOwnerMu.Lock()
	_, ok := hub.connCountByOwner[owner]
	hub.connCountByOwnerMu.Unlock()
	if ok {
		t.Fatalf("per-owner entry should be gone after the paired release, map still holds %q", owner)
	}
}

// TestHandleAuth_TokenRekey_ReserveFailRejects pins the #1775 cap-exhausted
// branch: if the token-derived owner is already at maxConnsPerOwner, handleAuth
// must refuse the auth (no authOk), and must leave the per-owner counter intact
// for the genuinely-held slots (no leak of the just-released "" slot, no phantom
// increment under the capped owner).
func TestHandleAuth_TokenRekey_ReserveFailRejects(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()

	owner := tokenOwnerKey("secret")
	// Saturate the owner's per-owner cap with held slots from other conns.
	for i := 0; i < maxConnsPerOwner; i++ {
		if !hub.reserveOwnerSlot(owner) {
			t.Fatalf("pre-fill reserve %d should succeed", i)
		}
	}

	c := &wsClient{
		send:          make(chan []byte, 4),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	// Upgrade-time reservation against the empty pre-auth owner.
	if !hub.reserveOwnerSlot(c.uploadOwnerKey()) {
		t.Fatal("empty-owner reserve must succeed")
	}

	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})

	if c.authenticated.Load() {
		t.Fatal("auth must be refused when the per-owner cap is exhausted")
	}
	// Owner must not have changed (the reservation could not be re-keyed).
	if c.uploadOwnerKey() != "" {
		t.Fatalf("uploadOwner should remain empty on reject, got %q", c.uploadOwnerKey())
	}
	hub.connCountByOwnerMu.Lock()
	got := hub.connCountByOwner[owner]
	hub.connCountByOwnerMu.Unlock()
	if got != maxConnsPerOwner {
		t.Fatalf("per-owner count = %d, want %d (reject must not perturb held slots)", got, maxConnsPerOwner)
	}

	// The empty-owner slot we held at "upgrade time" must still be claimable
	// for release without underflow (handleAuth re-claimed it on reject).
	hub.releaseOwnerSlot(c.uploadOwnerKey())
}

// TestRekeyOwnerSlot_ConcurrentUnregisterNoLeak pins R20260605B-CORR-4
// (#1808): the handleAuth re-key (release(old) → reserve(new) →
// setUploadOwner(new)) used to run as three separate steps with no shared
// lock, so a concurrent writePump-triggered unregister reading
// c.uploadOwnerKey() in the window after reserve(new) but before
// setUploadOwner(new) released the wrong owner ("") and leaked newOwner's
// slot. rekeyOwnerSlot now performs the swap under connCountByOwnerMu and
// gates on c.done; releaseOwnerSlotForClient reads the owner key under the
// same lock. For EVERY interleaving the per-owner counter must settle to
// exactly zero — no phantom slot survives. Run under -race.
func TestRekeyOwnerSlot_ConcurrentUnregisterNoLeak(t *testing.T) {
	owner := tokenOwnerKey("secret")
	for iter := 0; iter < 200; iter++ {
		hub, _ := newTestHub("secret")

		c := &wsClient{
			send:          make(chan []byte, 4),
			done:          make(chan struct{}),
			subscriptions: make(map[string]func()),
			subGen:        make(map[string]uint64),
		}
		// Upgrade-time reservation against the empty pre-auth owner, then
		// register so unregister's `removed` gate fires exactly once.
		if !hub.reserveOwnerSlot(c.uploadOwnerKey()) {
			t.Fatal("empty-owner reserve must succeed")
		}
		hub.register(c)

		var wg sync.WaitGroup
		wg.Add(2)
		// readPump arm: re-key to the token-derived owner.
		go func() {
			defer wg.Done()
			hub.rekeyOwnerSlot(c, "", owner)
		}()
		// writePump teardown arm: close done (as the pump defer does) then
		// unregister, which releases the slot via releaseOwnerSlotForClient.
		go func() {
			defer wg.Done()
			c.closeDone()
			hub.unregister(c)
		}()
		wg.Wait()

		// Whichever order won, no live connection remains, so the per-owner
		// counter for BOTH the empty and the token owner must be back to zero
		// (the map drops entries that reach 0).
		hub.connCountByOwnerMu.Lock()
		gotNew := hub.connCountByOwner[owner]
		gotEmpty := hub.connCountByOwner[""]
		hub.connCountByOwnerMu.Unlock()
		if gotNew != 0 {
			hub.Shutdown()
			t.Fatalf("iter %d: per-owner count for newOwner = %d, want 0 (slot leaked)", iter, gotNew)
		}
		if gotEmpty != 0 {
			hub.Shutdown()
			t.Fatalf("iter %d: per-owner count for empty owner = %d, want 0", iter, gotEmpty)
		}
		hub.Shutdown()
	}
}

// TestRekeyOwnerSlot_SkipsWhenConnDone pins the done-gate: if the connection
// has already torn down (c.done closed and its slot released by unregister)
// when the delayed handleAuth re-key runs, rekeyOwnerSlot must NOT reserve a
// fresh slot for the dead conn — that slot would never be released because
// unregister's once-only `removed` gate has already passed.
func TestRekeyOwnerSlot_SkipsWhenConnDone(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()

	c := &wsClient{
		send:          make(chan []byte, 4),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	if !hub.reserveOwnerSlot(c.uploadOwnerKey()) {
		t.Fatal("empty-owner reserve must succeed")
	}
	hub.register(c)
	// Full teardown first: closes done and releases the empty-owner slot.
	c.closeDone()
	hub.unregister(c)

	owner := tokenOwnerKey("secret")
	if hub.rekeyOwnerSlot(c, "", owner) {
		t.Fatal("rekeyOwnerSlot must return false once the conn is done")
	}
	hub.connCountByOwnerMu.Lock()
	got := hub.connCountByOwner[owner]
	hub.connCountByOwnerMu.Unlock()
	if got != 0 {
		t.Fatalf("rekey reserved a phantom slot for a dead conn: count = %d, want 0", got)
	}
}

// TestUploadOwnerAtomic_NoRace pins #1776: handleAuth writes c.uploadOwner from
// the readPump while releaseOwnerSlot/allowSendForOwner read it from the
// writePump-driven teardown / send path. With the field as a plain string this
// is a data race; the atomic.Pointer makes the read/write safe. Run under
// `go test -race ./internal/server/...` — the race detector flags the regression.
func TestUploadOwnerAtomic_NoRace(t *testing.T) {
	hub, _ := newTestHub("")
	defer hub.Shutdown()

	c := &wsClient{
		send:          make(chan []byte, 4),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	c.setUploadOwner("initial-owner")

	var wg sync.WaitGroup
	wg.Add(3)

	// Writer: mimics handleAuth re-keying the owner under the readPump.
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			c.setUploadOwner("owner-" + string(rune('A'+i%26)))
		}
	}()
	// Reader 1: mimics writePump teardown calling releaseOwnerSlot.
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			hub.releaseOwnerSlot(c.uploadOwnerKey())
		}
	}()
	// Reader 2: mimics readPump send path calling allowSendForOwner.
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			hub.allowSendForOwner(c.uploadOwnerKey())
		}
	}()
	wg.Wait()
}
