package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTranscriberSanitize returns a fixed payload regardless of input —
// used to drive R247-SEC-18 (#516) defence-in-depth assertions on the
// dashboard side. The real AWS Transcriber already sanitises at source
// (internal/transcribe/transcribe.go), but the dashboard wire layer must
// not rely on upstream policy: a future transcriber implementation that
// forgets to scrub bidi / C1 / LS-PS runes must still be unable to land
// log-injection-class bytes into IM dispatch / dashboard JSON. The stub
// returns the payload verbatim so the assertions can verify
// dashboard_transcribe.go's own sanitiser path.
type stubTranscriberSanitize struct {
	out string
}

func (s stubTranscriberSanitize) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return s.out, nil
}

// oggMagicForSanitizeTest is an 8-byte prefix that http.DetectContentType
// identifies as "application/ogg" so the handler accepts our synthetic
// payload past the magic-byte gate without spinning up ffmpeg.
var oggMagicForSanitizeTest = []byte{'O', 'g', 'g', 'S', 0, 0, 0, 0}

func newTranscribeRequestForSanitize(t *testing.T, audio []byte, declaredMIME string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="audio"; filename="x.ogg"`}
	hdr["Content-Type"] = []string{declaredMIME}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(audio)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// TestHandleTranscribe_SanitisesLogInjectionRunes pins R247-SEC-18 (#516):
// the dashboard handler must scrub IsLogInjectionRune codepoints (C1
// controls, bidi overrides/isolates, LS/PS) from the transcribed text
// before httputil.WriteJSON serialises it onto the wire. We feed a stub
// transcriber that returns text containing one rune from each class plus
// an ASCII control byte; the response body must contain no surviving
// bytes from those classes.
func TestHandleTranscribe_SanitisesLogInjectionRunes(t *testing.T) {
	// Construct a payload that includes:
	//   - Bidi override U+202E (RLO)
	//   - Bidi isolate  U+2068 (FSI)
	//   - LS            U+2028
	//   - C1 control    U+0085 (NEL)
	// Plus surrounding ASCII so the fast-path doesn't short-circuit.
	// Use \u escapes so Go source stays ASCII-clean — embedding LS/PS
	// literally would put a real line separator inside the source file
	// which is awkward for editors and diff tools.
	dirty := "hello‮world⁨x yz"
	h := &Handler{
		transcriber: stubTranscriberSanitize{out: dirty},
		sem:         make(chan struct{}, 1),
	}

	req := newTranscribeRequestForSanitize(t, oggMagicForSanitizeTest, "audio/ogg")
	rec := httptest.NewRecorder()
	h.HandleTranscribe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%q", err, rec.Body.String())
	}
	for _, bad := range []rune{'‮', '⁨', ' ', ''} {
		if strings.ContainsRune(resp.Text, bad) {
			t.Fatalf("response text still contains log-injection rune U+%04X: %q", bad, resp.Text)
		}
	}
	// Sanity: the surrounding ASCII content survives so we know we are
	// asserting on the sanitised path rather than an empty / dropped string.
	for _, want := range []string{"hello", "world"} {
		if !strings.Contains(resp.Text, want) {
			t.Fatalf("expected %q to survive sanitisation, got %q", want, resp.Text)
		}
	}
}
