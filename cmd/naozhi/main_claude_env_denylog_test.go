package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// recordingHandler captures Warn-level messages keyed by the "key" attr so
// tests can pin the exact deny wording.
type recordingHandler struct {
	mu   sync.Mutex
	msgs map[string]string // env key -> log message
}

func (h *recordingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	var key string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "key" {
			key = a.Value.String()
			return false
		}
		return true
	})
	h.mu.Lock()
	h.msgs[key] = r.Message
	h.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

// TestFilterClaudeEnv_DenyLogWording pins the warn wording per denied key.
// filterClaudeEnv derives the sentence from the key's namespace; if a future
// envpolicy Table edit denies a settings key outside AWS_/CLAUDE_, this test
// forces the wording (and the heuristic) to be revisited rather than silently
// mislabelling the log.
func TestFilterClaudeEnv_DenyLogWording(t *testing.T) {
	h := &recordingHandler{msgs: map[string]string{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := filterClaudeEnv(map[string]string{
		"AWS_PROFILE":                    "admin",
		"AWS_SHARED_CREDENTIALS_FILE":    "/x",
		"CLAUDE_CODE_USE_MOCK_RESPONSES": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"UNRELATED_VAR": "silent-skip", // outside namespaces: no warn at all
	})
	if len(got) != 0 {
		t.Fatalf("all inputs must be dropped, got %v", got)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	want := map[string]string{
		"AWS_PROFILE":                    "claude settings env: refusing to propagate auth-source AWS var",
		"AWS_SHARED_CREDENTIALS_FILE":    "claude settings env: refusing to propagate auth-source AWS var",
		"CLAUDE_CODE_USE_MOCK_RESPONSES": "claude settings env: refusing to propagate CLAUDE_ kill-switch var",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "claude settings env: refusing to propagate CLAUDE_ kill-switch var",
	}
	for key, msg := range want {
		if h.msgs[key] != msg {
			t.Errorf("deny log for %q = %q, want %q", key, h.msgs[key], msg)
		}
	}
	if m, ok := h.msgs["UNRELATED_VAR"]; ok {
		t.Errorf("key outside allowed namespaces must be skipped silently, logged %q", m)
	}
}
