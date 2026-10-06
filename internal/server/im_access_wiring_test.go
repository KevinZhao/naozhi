package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ServerOptions.IMAccess reaches the dispatcher the IM adapters call: a
// stranger's direct message is refused at the handler the server hands out.
func TestServerOptions_IMAccessReachesDispatcher(t *testing.T) {
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
	srv.dispatcher.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: parityPlatformName, EventID: "e1", UserID: "eve",
		ChatID: parityChatID, ChatType: "direct", Text: "hello",
	})
	if got := plat.allReplies(); len(got) != 1 || !strings.Contains(got[0], "ID: eve") {
		t.Fatalf("replies = %q, want the access refusal", got)
	}
}

// ServerOptions.IMRateLimit reaches the same dispatcher: a sender past the
// burst gets the "too fast" reply instead of a command answer.
func TestServerOptions_IMRateLimitReachesDispatcher(t *testing.T) {
	plat := newParityPlatform(false)
	srv, _ := buildServerWithHandlers(ServerOptions{
		Addr:        ":0",
		Router:      session.NewRouter(session.RouterConfig{}),
		Platforms:   map[string]platform.Platform{parityPlatformName: plat},
		Backend:     "claude",
		IMRateLimit: dispatch.RateLimit{MsgsPerMin: 1, Burst: 1},
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	h := srv.dispatcher.BuildHandler()
	for _, ev := range []string{"e1", "e2"} {
		h(context.Background(), platform.IncomingMessage{
			Platform: parityPlatformName, EventID: ev, UserID: "alice",
			ChatID: parityChatID, ChatType: "direct", Text: "/help",
		})
	}
	if got := plat.allReplies(); len(got) != 2 || !strings.Contains(got[1], "消息过于频繁") {
		t.Fatalf("replies = %q, want /help's answer then the rate-limit reply", got)
	}
}

// ServerOptions.IMBudget reaches the same dispatcher: with the machine's
// daily cap spent, a message gets the budget refusal instead of a turn.
func TestServerOptions_IMBudgetReachesDispatcher(t *testing.T) {
	plat := newParityPlatform(false)
	idx := budget.NewIndex(time.UTC, nil)
	idx.Add(costledger.Entry{TS: time.Now(), Unit: costledger.UnitUSD, Amount: 1})
	srv, _ := buildServerWithHandlers(ServerOptions{
		Addr:      ":0",
		Router:    session.NewRouter(session.RouterConfig{}),
		Platforms: map[string]platform.Platform{parityPlatformName: plat},
		Backend:   "claude",
		IMBudget:  budget.NewGate(budget.Limits{DailyUSD: 1}, idx),
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	srv.dispatcher.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: parityPlatformName, EventID: "e1", UserID: "alice",
		ChatID: parityChatID, ChatType: "direct", Text: "hello",
	})
	if got := plat.allReplies(); len(got) != 1 || !strings.Contains(got[0], "今日费用预算已用尽") {
		t.Fatalf("replies = %q, want the budget refusal", got)
	}
}

// The same gate answers the dashboard's /api/cost/budget.
func TestServerOptions_IMBudgetReachesCostAPI(t *testing.T) {
	idx := budget.NewIndex(time.UTC, nil)
	idx.Add(costledger.Entry{TS: time.Now(), Unit: costledger.UnitUSD, Amount: 0.5})
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:     ":0",
		Router:   session.NewRouter(session.RouterConfig{}),
		Backend:  "claude",
		IMBudget: budget.NewGate(budget.Limits{DailyUSD: 2}, idx),
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	rec := httptest.NewRecorder()
	hs.costH.HandleBudget(rec, httptest.NewRequest(http.MethodGet, "/api/cost/budget", nil))
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `"limit":2`) {
		t.Fatalf("status %d body %s, want the machine-wide cap", rec.Code, body)
	}
}

// ServerOptions.IMGroupScope reaches the same dispatcher: with the chat
// scope a message in a thread runs on the channel's session, not the
// thread's.
func TestServerOptions_IMGroupScopeReachesDispatcher(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv, _ := buildServerWithHandlers(ServerOptions{
		Addr:         ":0",
		Router:       router,
		Platforms:    map[string]platform.Platform{parityPlatformName: newParityPlatform(false)},
		Backend:      "claude",
		IMGroupScope: dispatch.GroupScopeChat,
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	ran := make(chan string, 2)
	for _, key := range []string{"parity:group:chat1:general", "parity:group:chat1#tT1:general"} {
		proc := session.NewTestProcess()
		proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
			ran <- key
			return &clievent.SendResult{Text: "ok"}, nil
		}
		router.InjectSession(key, proc)
	}
	srv.dispatcher.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: parityPlatformName, EventID: "e1", UserID: "alice", ChatID: parityChatID,
		ChatType: "group", MentionMe: true, ThreadID: "T1", Text: "hello",
	})
	select {
	case key := <-ran:
		if key != "parity:group:chat1:general" {
			t.Errorf("the thread's message ran on %q, want the channel's session", key)
		}
	default:
		t.Fatal("the message reached no session")
	}
}
