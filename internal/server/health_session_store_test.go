package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// session_store follows the /health wire contract: absent while every store
// file is writable, so a monitor can key on its presence alone (#2972).
func TestSessionStoreHealthProbe_OmitsWhenNothingIsBlocked(t *testing.T) {
	var auth healthAuthSection
	sessionStoreHealthProbe(nil)(&auth)
	if auth.SessionStore != nil {
		t.Errorf("session_store = %+v with no router, want omitted", auth.SessionStore)
	}
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	sessionStoreHealthProbe(r)(&auth)
	if auth.SessionStore != nil {
		t.Errorf("session_store = %+v with no store path, want omitted", auth.SessionStore)
	}
}

// The section's keys are what an operator greps for; pin them.
func TestHealthSessionStore_WireShape(t *testing.T) {
	auth := healthAuthSection{SessionStore: &healthSessionStore{Blocked: []session.StoreBlock{{
		Path: "/data/sessions.json", Label: "session store", Reason: "r", Since: time.Unix(0, 0).UTC(), NeedRestart: true,
	}}}}
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		SessionStore struct {
			Blocked []map[string]any `json:"blocked"`
		} `json:"session_store"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.SessionStore.Blocked) != 1 {
		t.Fatalf("blocked = %v", got.SessionStore.Blocked)
	}
	b := got.SessionStore.Blocked[0]
	for _, k := range []string{"path", "label", "reason", "since", "need_restart"} {
		if _, ok := b[k]; !ok {
			t.Errorf("session_store.blocked[0] lacks %q: %v", k, b)
		}
	}
}
