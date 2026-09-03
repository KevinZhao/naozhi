package transcribe

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #2433 item 7: the dashboard maps HTTP 415 to the "unsupported audio
// format" message; format rejections must therefore be 415, while other
// parameter errors (missing part) stay 400.
func TestHandleTranscribe_UnsupportedFormatIs415(t *testing.T) {
	h := &Handler{
		transcriber: stubTranscriberSanitize{out: "ok"},
		sem:         make(chan struct{}, 1),
	}
	cases := []struct {
		name     string
		audio    []byte
		declared string
	}{
		{"declared mime not allowlisted", oggMagicForSanitizeTest, "text/plain"},
		{"content sniffs as non-audio", []byte("hello, this is text\n"), "audio/ogg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleTranscribe(rec, newTranscribeRequestForSanitize(t, tc.audio, tc.declared))
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("want 415, got %d body=%q", rec.Code, rec.Body.String())
			}
		})
	}

	// Missing audio field is a parameter error, not a format error → 400.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("other", "x"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.HandleTranscribe(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing audio field: want 400, got %d body=%q", rec.Code, rec.Body.String())
	}
}
