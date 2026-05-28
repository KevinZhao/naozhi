package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// fakeTokenErr implements TokenInvalidatedError so we can drive
// ReplyWithRetry's token-rotation extra-retry path without depending on
// an adapter package.
type fakeTokenErr struct {
	msg        string
	tokenInval bool
}

func (e *fakeTokenErr) Error() string            { return e.msg }
func (e *fakeTokenErr) IsTokenInvalidated() bool { return e.tokenInval }

// TestIsTokenInvalidated covers the chain-walking behaviour matching
// IsPermanent — direct, wrapped, and nil cases.
func TestIsTokenInvalidated(t *testing.T) {
	t.Parallel()
	tokInvRoot := &fakeTokenErr{msg: "token rejected", tokenInval: true}
	plain := errors.New("network blip")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain_error", plain, false},
		{"token_invalidated_direct", tokInvRoot, true},
		{"token_invalidated_wrapped", fmt.Errorf("outer: %w", tokInvRoot), true},
		{"non_invalidated_carrier", &fakeTokenErr{msg: "other", tokenInval: false}, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTokenInvalidated(tc.err); got != tc.want {
				t.Fatalf("IsTokenInvalidated(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// scriptedPlat returns a configurable sequence of (msgID, err) pairs from
// successive Reply calls. attempt counter is exposed for assertions.
type scriptedPlat struct {
	fakePlat
	results []scriptedResult
	calls   int
}

type scriptedResult struct {
	id  string
	err error
}

func (s *scriptedPlat) Reply(_ context.Context, _ OutgoingMessage) (string, error) {
	if s.calls >= len(s.results) {
		return "", fmt.Errorf("scriptedPlat: unexpected call %d", s.calls+1)
	}
	r := s.results[s.calls]
	s.calls++
	return r.id, r.err
}

func (s *scriptedPlat) Name() string                                      { return "scripted" }
func (s *scriptedPlat) RegisterRoutes(_ *http.ServeMux, _ MessageHandler) {}
func (s *scriptedPlat) EditMessage(_ context.Context, _, _ string) error  { return nil }
func (s *scriptedPlat) MaxReplyLength() int                               { return DefaultMaxReplyLen }

// TestReplyWithRetry_TokenInvalidatedGrantsExtraRetry verifies that a
// TokenInvalidatedError on attempt 1 expands the budget by one — so a
// 3-attempt config that hits token rotation on attempt 1 and an
// upstream-not-yet-active token on attempt 2 still gets a 4th attempt
// to land the freshly-propagated token. Issue #1339.
func TestReplyWithRetry_TokenInvalidatedGrantsExtraRetry(t *testing.T) {
	t.Parallel()
	tokenErr := &fakeTokenErr{msg: "tenant token invalid", tokenInval: true}
	transientErr := errors.New("upstream token-not-yet-active")
	sp := &scriptedPlat{
		results: []scriptedResult{
			{"", tokenErr},     // attempt 1: token-invalidate -> grant +1, signal rotation
			{"", transientErr}, // attempt 2: still bad (token race)
			{"", transientErr}, // attempt 3: still bad
			{"msg-ok", nil},    // attempt 4 (extra-budget): success
		},
	}

	// Use minimal-budget retry so timing stays small. JitterBackoff
	// minimum at 500ms*0.75 = 375ms; 3 inter-attempt waits = ~1.7s
	// plus a 50ms rotation delay. Still under any sensible CI budget.
	id, err := ReplyWithRetry(context.Background(), sp, OutgoingMessage{ChatID: "c1"}, 3)
	if err != nil {
		t.Fatalf("expected success on extra-budget attempt; got err=%v", err)
	}
	if id != "msg-ok" {
		t.Fatalf("expected msg-ok, got %q", id)
	}
	if sp.calls != 4 {
		t.Fatalf("expected 4 calls (3 base + 1 extra granted), got %d", sp.calls)
	}
}

// TestReplyWithRetry_TokenInvalidatedExtraGrantedAtMostOnce verifies
// that even repeated token-invalidate failures cannot keep extending
// the budget — the bonus is one-shot per ReplyWithRetry invocation.
// Without this guard a misbehaving upstream returning IsTokenExpired
// every call would loop indefinitely. Issue #1339.
func TestReplyWithRetry_TokenInvalidatedExtraGrantedAtMostOnce(t *testing.T) {
	t.Parallel()
	tokenErr := &fakeTokenErr{msg: "tenant token invalid", tokenInval: true}
	sp := &scriptedPlat{
		results: []scriptedResult{
			{"", tokenErr}, // 1 base
			{"", tokenErr}, // 2 base
			{"", tokenErr}, // 3 base
			{"", tokenErr}, // 4 extra — should be the last call
		},
	}

	_, err := ReplyWithRetry(context.Background(), sp, OutgoingMessage{ChatID: "c1"}, 3)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if sp.calls != 4 {
		t.Fatalf("expected exactly 4 calls (3 base + 1 one-shot bonus), got %d", sp.calls)
	}
}

// TestReplyWithRetry_NoTokenSignalUsesBaseBudget verifies the regression
// guard: a plain error (no token-invalidate signal) must NOT extend the
// retry budget — otherwise issue #1339's fix would silently inflate
// every retry path. Issue #1339.
func TestReplyWithRetry_NoTokenSignalUsesBaseBudget(t *testing.T) {
	t.Parallel()
	plain := errors.New("transient 5xx")
	sp := &scriptedPlat{
		results: []scriptedResult{
			{"", plain},
			{"", plain},
			{"", plain},
		},
	}

	_, err := ReplyWithRetry(context.Background(), sp, OutgoingMessage{ChatID: "c1"}, 3)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if sp.calls != 3 {
		t.Fatalf("expected exactly 3 calls (no token signal -> no extension), got %d", sp.calls)
	}
}

// TestReplyWithRetry_PermanentBeatsTokenInvalidated verifies that a
// PermanentError on the same attempt as a token-invalidate signal still
// short-circuits — permanent classification wins. Practical case: an
// adapter could conceivably return an error type that satisfies both
// interfaces (e.g. "app disabled AND token rotated" combined-cause
// envelope); we want the no-retry semantic to win. Issue #1339.
func TestReplyWithRetry_PermanentBeatsTokenInvalidated(t *testing.T) {
	t.Parallel()
	combo := &combinedErr{msg: "app disabled, token also rotated"}
	sp := &scriptedPlat{
		results: []scriptedResult{
			{"", combo},
		},
	}

	_, err := ReplyWithRetry(context.Background(), sp, OutgoingMessage{ChatID: "c1"}, 3)
	if err == nil {
		t.Fatal("expected error from permanent failure")
	}
	if sp.calls != 1 {
		t.Fatalf("expected 1 call (permanent short-circuit), got %d", sp.calls)
	}
}

type combinedErr struct{ msg string }

func (e *combinedErr) Error() string            { return e.msg }
func (e *combinedErr) IsPermanent() bool        { return true }
func (e *combinedErr) IsTokenInvalidated() bool { return true }
