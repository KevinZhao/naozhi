package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// bearerOwner mirrors uploadOwner's Bearer branch: sha256(token)[:16] hex.
// Keeping the upload owner deterministic lets the test Put a file under the
// exact bucket handleSend will resolve from the same Bearer header.
func bearerOwner(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// TestHandleSend_ValidatesBeforeTakingAttachments pins R20260610-085718-LB-9
// (#2014): pure-input validation (key / text cap) must run BEFORE
// uploadStore.TakeAll consumes the pre-uploaded attachments. Before the fix,
// an oversized-text request that also carried a valid file_id returned 400
// only after TakeAll had already deleted the store entry — silently destroying
// the upload and forcing the user to re-upload the whole batch (violating the
// TakeAll R37-CONCUR4 "retry the whole batch" contract).
//
// We assert both halves: the request is rejected (400 text too long) AND the
// pre-uploaded file survives in the store so the client's retry succeeds.
func TestHandleSend_ValidatesBeforeTakingAttachments(t *testing.T) {
	const token = "test-bearer-token"
	owner := bearerOwner(token)

	store := newUploadStore()
	fid, err := store.Put(owner, cli.ImageData{
		Kind:     cli.KindImageInline,
		Data:     []byte("png-bytes"),
		MimeType: "image/png",
		OrigName: "shot.png",
	})
	if err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	h := &SendHandler{uploadStore: store}

	body, _ := json.Marshal(map[string]any{
		"key":      "feishu:p2p:u1",
		"text":     strings.Repeat("x", maxWSSendTextBytes+1), // trips the text-too-long 400
		"file_ids": []string{fid},
	})
	r := httptest.NewRequest("POST", "/api/send", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	h.handleSend(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (text too long)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "text too long") {
		t.Fatalf("body = %q, want a 'text too long' rejection", w.Body.String())
	}

	// The attachment must still be retrievable — the 400 must not have
	// consumed it. Peek (ownership-checked, non-destructive) proves the slot
	// survived for the client's retry.
	if got := store.Peek(fid, owner); got == nil {
		t.Errorf("file_id %q was consumed by the 400 path — TakeAll ran before validation (#2014 regressed)", fid)
	}
}
