package transcribe

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNew_NilLimiterFailsClosed pins #2235: a Handler built without a Limiter
// must NOT silently disable per-IP rate limiting. Pre-fix New stored the nil
// Limiter directly and the handler's `limiter != nil && ...` guard skipped
// throttling entirely. New now substitutes a deny-all limiter so a missing
// limiter rejects every request (429) instead of failing open.
func TestNew_NilLimiterFailsClosed(t *testing.T) {
	t.Parallel()

	h := New(Deps{
		Transcriber: stubTranscriberSanitize{out: "ok"},
		// Limiter intentionally omitted (nil).
		SemCap: 1,
	})

	req := newTranscribeRequestForSanitize(t, oggMagicForSanitizeTest, "audio/ogg")
	rec := httptest.NewRecorder()
	h.HandleTranscribe(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("nil-limiter handler should fail closed with 429, got %d body=%q",
			rec.Code, rec.Body.String())
	}
}

// TestNew_RealLimiterAllows confirms the fail-closed default does not affect
// the normal path: a permissive limiter lets the request through to the
// transcriber (200).
func TestNew_RealLimiterAllows(t *testing.T) {
	t.Parallel()

	h := New(Deps{
		Transcriber: stubTranscriberSanitize{out: "hello"},
		Limiter:     allowAllLimiter{},
		SemCap:      1,
	})

	req := newTranscribeRequestForSanitize(t, oggMagicForSanitizeTest, "audio/ogg")
	rec := httptest.NewRecorder()
	h.HandleTranscribe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("permissive-limiter handler should return 200, got %d body=%q",
			rec.Code, rec.Body.String())
	}
}

type allowAllLimiter struct{}

func (allowAllLimiter) Allow(string) bool               { return true }
func (allowAllLimiter) AllowRequest(*http.Request) bool { return true }
