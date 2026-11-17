package transcribe

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// MediaRecorder in Chrome / Firefox records "audio/webm;codecs=opus" and the
// browser writes that type, parameters included, as the part's Content-Type.
func TestHandleTranscribe_DeclaredTypeWithParams(t *testing.T) {
	h := &Handler{
		transcriber: stubTranscriberSanitize{out: "ok"},
		sem:         make(chan struct{}, 1),
	}
	for _, declared := range []string{
		"audio/webm;codecs=opus",
		"audio/ogg;codecs=opus",
		"audio/ogg; codecs=opus",
		"Audio/OGG",
	} {
		t.Run(declared, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleTranscribe(rec, newTranscribeRequestForSanitize(t, oggMagicForSanitizeTest, declared))
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
	// Parameters do not get a type off the list through.
	for _, declared := range []string{"text/plain;codecs=opus", "audio/webm;;", ";codecs=opus"} {
		t.Run(declared, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleTranscribe(rec, newTranscribeRequestForSanitize(t, oggMagicForSanitizeTest, declared))
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("want 415, got %d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}
