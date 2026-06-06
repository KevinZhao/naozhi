package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// stubVision implements VisionOrienter, returning a canned stream-json
// transcript whose result text is `answer`. err, when set, is returned
// instead (simulating a CLI/timeout failure).
type stubVision struct {
	answer    string
	err       error
	callCount int
	lastModel string
}

func (s *stubVision) RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error) {
	s.callCount++
	s.lastModel = model
	if s.err != nil {
		return nil, s.err
	}
	res := map[string]any{"type": "result", "subtype": "success", "result": s.answer}
	line, _ := json.Marshal(res)
	return append(line, '\n'), nil
}

// orientTestJPEG builds a real w×h JPEG so RotateJPEG can actually decode it.
func orientTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	img.SetRGBA(0, 0, color.RGBA{A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// newOrientTestHandler builds a SendHandler with an in-memory upload store
// and the given orientConfig (nil = feature off).
func newOrientTestHandler(oc *orientConfig) *SendHandler {
	return &SendHandler{
		uploadStore: newUploadStore(),
		orient:      oc,
		// auth nil + Bearer token in the request → deterministic owner.
	}
}

const orientTestToken = "test-token-abc"

func orientReq(id string) *http.Request {
	body, _ := json.Marshal(map[string]string{"id": id})
	r := httptest.NewRequest("POST", "/api/sessions/orient", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+orientTestToken)
	return r
}

// putOwnedImage stores an image under the same owner the Bearer token yields.
func putOwnedImage(t *testing.T, h *SendHandler, data []byte) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+orientTestToken)
	owner, ok := uploadOwner(nil, r, nil, false)
	if !ok {
		t.Fatal("could not derive owner from bearer token")
	}
	id, err := h.uploadStore.Put(owner, cli.ImageData{Kind: cli.KindImageInline, Data: data, MimeType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return id
}

func decodeOrientResp(t *testing.T, w *httptest.ResponseRecorder) (bool, int) {
	t.Helper()
	var resp struct {
		Rotated bool `json:"rotated"`
		Degrees int  `json:"degrees"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp %q: %v", w.Body.String(), err)
	}
	return resp.Rotated, resp.Degrees
}

func TestHandleOrient_RotatesOnActionableVerdict(t *testing.T) {
	stub := &stubVision{answer: "right"} // right -> 270° CW
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: stub, model: "haiku", timeout: time.Second})
	orig := orientTestJPEG(t, 8, 4)
	id := putOwnedImage(t, h, orig)

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	rotated, deg := decodeOrientResp(t, w)
	if !rotated || deg != 270 {
		t.Errorf("expected rotated=true deg=270, got rotated=%v deg=%d", rotated, deg)
	}
	// The response must carry the corrected image inline as a data URL so the
	// client can refresh its preview without a second round-trip.
	var full struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full resp: %v", err)
	}
	if !strings.HasPrefix(full.Image, "data:image/jpeg;base64,") {
		t.Errorf("rotated response must include an inline jpeg data URL, got prefix %.30q", full.Image)
	}
	if stub.lastModel != "haiku" {
		t.Errorf("model passed to runner = %q, want haiku", stub.lastModel)
	}
	// The stored bytes must now decode to a 4×8 image (8×4 rotated 90/270).
	got := h.uploadStore.Peek(id, ownerForToken(t))
	if got == nil {
		t.Fatal("entry vanished after orient")
	}
	img, _, err := image.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("rotated bytes don't decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 4 || b.Dy() != 8 {
		t.Errorf("rotated dims = %dx%d, want 4x8", b.Dx(), b.Dy())
	}
}

func ownerForToken(t *testing.T) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+orientTestToken)
	owner, _ := uploadOwner(nil, r, nil, false)
	return owner
}

// TestHandleOrient_PNGInputBecomesJPEG pins the MIME-desync fix: a PNG upload
// that gets rotated must have its stored mime updated to image/jpeg (the
// rotate re-encodes to JPEG), so downstream consumers don't mislabel bytes.
func TestHandleOrient_PNGInputBecomesJPEG(t *testing.T) {
	stub := &stubVision{answer: "left"} // actionable
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: stub, timeout: time.Second})

	// Store a real PNG under the owner.
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 6, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+orientTestToken)
	owner, _ := uploadOwner(nil, r, nil, false)
	id, _ := h.uploadStore.Put(owner, cli.ImageData{Kind: cli.KindImageInline, Data: pngBuf.Bytes(), MimeType: "image/png"})

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id))

	rotated, _ := decodeOrientResp(t, w)
	if !rotated {
		t.Fatal("PNG should have been rotated")
	}
	got := h.uploadStore.Peek(id, owner)
	if got == nil {
		t.Fatal("entry vanished")
	}
	if got.MimeType != "image/jpeg" {
		t.Errorf("stored mime after rotate = %q, want image/jpeg (re-encode desync)", got.MimeType)
	}
	// The bytes must actually be JPEG (magic FF D8).
	if len(got.Data) < 2 || got.Data[0] != 0xff || got.Data[1] != 0xd8 {
		t.Error("stored bytes after rotate are not JPEG magic")
	}
}

func TestHandleOrient_UpVerdictLeavesImageUntouched(t *testing.T) {
	stub := &stubVision{answer: "up"}
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: stub, timeout: time.Second})
	orig := orientTestJPEG(t, 8, 4)
	id := putOwnedImage(t, h, orig)

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id))

	rotated, _ := decodeOrientResp(t, w)
	if rotated {
		t.Error("'up' verdict must not rotate")
	}
	got := h.uploadStore.Peek(id, ownerForToken(t))
	if !bytes.Equal(got.Data, orig) {
		t.Error("'up' verdict must leave the stored bytes byte-identical")
	}
}

func TestHandleOrient_VisionFailureFailsSafe(t *testing.T) {
	stub := &stubVision{err: context.DeadlineExceeded}
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: stub, timeout: time.Second})
	orig := orientTestJPEG(t, 8, 4)
	id := putOwnedImage(t, h, orig)

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort)", w.Code)
	}
	rotated, _ := decodeOrientResp(t, w)
	if rotated {
		t.Error("a vision failure must fail safe (rotated=false)")
	}
	got := h.uploadStore.Peek(id, ownerForToken(t))
	if !bytes.Equal(got.Data, orig) {
		t.Error("a vision failure must leave the original bytes intact")
	}
}

func TestHandleOrient_FeatureOffIsNoOp(t *testing.T) {
	h := newOrientTestHandler(nil) // feature off
	id := putOwnedImage(t, h, orientTestJPEG(t, 8, 4))

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rotated, _ := decodeOrientResp(t, w); rotated {
		t.Error("feature-off must return rotated=false")
	}
}

func TestHandleOrient_OwnerIsolation(t *testing.T) {
	stub := &stubVision{answer: "right"}
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: stub, timeout: time.Second})
	// Store under alice's token, then request orient with a different token.
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("Authorization", "Bearer alice-token")
	aliceOwner, _ := uploadOwner(nil, r, nil, false)
	id, _ := h.uploadStore.Put(aliceOwner, cli.ImageData{Kind: cli.KindImageInline, Data: orientTestJPEG(t, 8, 4), MimeType: "image/jpeg"})

	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq(id)) // orientReq uses orientTestToken (bob)

	if w.Code != http.StatusNotFound {
		t.Errorf("cross-owner orient must 404, got %d", w.Code)
	}
	if stub.callCount != 0 {
		t.Error("vision runner must not be called for a non-owned id")
	}
}

func TestHandleOrient_MissingIDRejected(t *testing.T) {
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: &stubVision{}, timeout: time.Second})
	r := httptest.NewRequest("POST", "/api/sessions/orient", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+orientTestToken)
	w := httptest.NewRecorder()
	h.handleOrient(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing id must 400, got %d", w.Code)
	}
}

func TestHandleOrient_UnknownIDNotFound(t *testing.T) {
	h := newOrientTestHandler(&orientConfig{enabled: true, runner: &stubVision{answer: "right"}, timeout: time.Second})
	w := httptest.NewRecorder()
	h.handleOrient(w, orientReq("deadbeefdeadbeef"))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown id must 404, got %d", w.Code)
	}
}
