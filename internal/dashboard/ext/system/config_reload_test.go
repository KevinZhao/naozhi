package system

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/session"
)

func reloadHandlers(t *testing.T, fn ConfigReloader) *Handlers {
	t.Helper()
	return New(Deps{
		Router:       session.NewRouter(session.RouterConfig{MaxProcs: 1, Workspace: t.TempDir()}),
		ConfigReload: fn,
	})
}

func postReload(h *Handlers) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.HandleConfigReload(w, httptest.NewRequest(http.MethodPost, "/api/system/config/reload", strings.NewReader("{}")))
	return w
}

func TestHandleConfigReload_NotWired(t *testing.T) {
	t.Parallel()
	if w := postReload(reloadHandlers(t, nil)); w.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501", w.Code)
	}
}

func TestHandleConfigReload_ReturnsResultAndThrottles(t *testing.T) {
	t.Parallel()
	calls := 0
	h := reloadHandlers(t, func(context.Context) (config.ReloadResult, error) {
		calls++
		return config.ReloadResult{SHA256: "abc", LoadedAt: time.Unix(0, 0).UTC(),
			Applied: []string{"im_access"}, RestartRequired: []string{"cli"}}, nil
	})
	w := postReload(h)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res config.ReloadResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.SHA256 != "abc" || len(res.Applied) != 1 || res.RestartRequired[0] != "cli" {
		t.Fatalf("result = %+v", res)
	}
	// One reload per 10s: the second call is throttled before the reloader runs.
	if w := postReload(h); w.Code != http.StatusTooManyRequests || calls != 1 {
		t.Fatalf("second call: code=%d calls=%d, want 429 and 1", w.Code, calls)
	}
}

func TestHandleConfigReload_LoadErrorIs422(t *testing.T) {
	t.Parallel()
	h := reloadHandlers(t, func(context.Context) (config.ReloadResult, error) {
		return config.ReloadResult{}, errors.New("im_limits.per_chat_daily_usd is negative\x1b[31m")
	})
	w := postReload(h)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "per_chat_daily_usd") || strings.Contains(body, "\x1b") {
		t.Fatalf("body = %q, want the validation text with control bytes stripped", body)
	}
}
