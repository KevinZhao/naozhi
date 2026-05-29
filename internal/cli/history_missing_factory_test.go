package cli

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// countingHandler counts slog records whose message contains a substring.
type countingHandler struct {
	mu    sync.Mutex
	want  string
	count int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.want) {
		h.mu.Lock()
		h.count++
		h.mu.Unlock()
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// TestNewHistorySource_MissingFactoryWarnsOncePerBackend pins R249-ARCH-9
// (#975): a non-empty BackendID with no registered factory (the symptom of a
// dropped wireup blank-import) must surface a one-time Warn instead of
// silently degrading to Noop on every history page.
func TestNewHistorySource_MissingFactoryWarnsOncePerBackend(t *testing.T) {
	h := &countingHandler{want: "no history factory registered"}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Unique unregistered backend id so the package-level dedup map state
	// from other tests / runs cannot mask this assertion.
	const backendID = "missing-factory-975-test"
	w := &Wrapper{BackendID: backendID}

	for i := 0; i < 5; i++ {
		src := w.NewHistorySource(&fakeHistorySession{}, HistoryWiring{})
		if src == nil {
			t.Fatal("NewHistorySource must never return nil")
		}
	}

	h.mu.Lock()
	got := h.count
	h.mu.Unlock()
	if got != 1 {
		t.Errorf("missing-factory warn fired %d times across 5 calls, want exactly 1", got)
	}
}

// TestNewHistorySource_EmptyBackendNeverWarns guards the router-default case:
// an empty BackendID is legitimate ("" = router default, never reaches a real
// backend) and must NOT emit the missing-factory warning.
func TestNewHistorySource_EmptyBackendNeverWarns(t *testing.T) {
	h := &countingHandler{want: "no history factory registered"}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &Wrapper{BackendID: ""}
	_ = w.NewHistorySource(&fakeHistorySession{}, HistoryWiring{})

	h.mu.Lock()
	got := h.count
	h.mu.Unlock()
	if got != 0 {
		t.Errorf("empty backend warned %d times, want 0", got)
	}
}
