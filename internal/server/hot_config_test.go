package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ApplyHotConfig replaces the dispatcher's access policy for the next
// message: a sender refused under the startup policy is served after a reload
// that admits them, and vice versa.
func TestApplyHotConfig_SwapsAccessPolicy(t *testing.T) {
	plat := newParityPlatform(false)
	srv, _ := buildServerWithHandlers(ServerOptions{
		Addr:      ":0",
		Router:    session.NewRouter(session.RouterConfig{}),
		Platforms: map[string]platform.Platform{parityPlatformName: plat},
		Backend:   "claude",
		IMAccess: &imauth.Policy{Rules: map[string]imauth.Rule{
			parityPlatformName: {Allowed: map[string]struct{}{"alice": {}}},
		}},
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	handler := srv.dispatcher.BuildHandler()
	n := 0
	send := func(user, text string) {
		n++
		handler(context.Background(), platform.IncomingMessage{
			Platform: parityPlatformName, EventID: "e" + strconv.Itoa(n), UserID: user,
			ChatID: parityChatID + "-" + user, ChatType: "direct", Text: text,
		})
	}
	send("eve", "/help")
	if got := plat.allReplies(); len(got) != 1 || !strings.Contains(got[0], "ID: eve") {
		t.Fatalf("before reload: replies = %q, want the access refusal", got)
	}

	srv.ApplyHotConfig(HotConfig{Access: &imauth.Policy{Rules: map[string]imauth.Rule{
		parityPlatformName: {Allowed: map[string]struct{}{"eve": {}}},
	}}})
	send("eve", "/help")
	if got := plat.allReplies(); len(got) != 2 || !strings.Contains(got[1], "可用命令") {
		t.Fatalf("after reload admitting eve: replies = %q, want /help output", got)
	}
	// alice lost her entry; the policy is now closed to her.
	send("alice", "/help")
	if got := plat.allReplies(); len(got) != 3 || !strings.Contains(got[2], "ID: alice") {
		t.Fatalf("after reload dropping alice: replies = %q, want a refusal", got)
	}

	// nil policy reopens the platform.
	srv.ApplyHotConfig(HotConfig{})
	send("mallory", "/help")
	if got := plat.allReplies(); len(got) != 4 || !strings.Contains(got[3], "可用命令") {
		t.Fatalf("after clearing the policy: replies = %q", got)
	}
}

// A live fingerprint, when wired, is what /health reports; a reload's Set is
// visible on the next probe.
func TestHandleHealth_LiveConfigFingerprint(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	fp := NewConfigFingerprint("aaaa", time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC))
	hs.healthH.configLive = fp
	hs.healthH.configSHA256 = "stale-static-value"

	probe := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		hs.healthH.handleHealth(w, req)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	if b := probe(); b["config_sha256"] != "aaaa" || b["config_loaded_at"] != "2026-10-06T01:00:00Z" {
		t.Fatalf("initial = %v / %v", b["config_sha256"], b["config_loaded_at"])
	}
	fp.Set("bbbb", time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC))
	if b := probe(); b["config_sha256"] != "bbbb" || b["config_loaded_at"] != "2026-10-06T02:00:00Z" {
		t.Fatalf("after Set = %v / %v", b["config_sha256"], b["config_loaded_at"])
	}
	var nilFP *ConfigFingerprint
	if sha, at := nilFP.Get(); sha != "" || !at.IsZero() {
		t.Fatal("nil fingerprint should read as unknown")
	}
}
