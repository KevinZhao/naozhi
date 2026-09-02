package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestHandleSend_PostTakeAllFailureFlagsFilesConsumed pins F3: once TakeAll
// has consumed the pre-uploaded attachments, any later rejection must say so
// (`files_consumed: true`) so the dashboard drops its stale file chips instead
// of retrying ids that no longer exist ("file not found or expired" — the very
// symptom #2418 fixed). Rejections BEFORE TakeAll leave the files in place and
// must NOT carry the flag.
func TestHandleSend_PostTakeAllFailureFlagsFilesConsumed(t *testing.T) {
	const token = "test-bearer-token"
	owner := bearerOwner(token)
	store := newUploadStore()
	seed := func() string {
		fid, err := store.Put(owner, cli.Attachment{Kind: cli.KindImageInline, Data: []byte("png"), MimeType: "image/png", OrigName: "a.png"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return fid
	}
	h := &SendHandler{uploadStore: store}

	// Pre-TakeAll rejection: text too long → file survives, no flag.
	fid := seed()
	w := postSendJSON(t, h, token, map[string]any{"key": "feishu:p2p:u1", "text": strings.Repeat("x", maxWSSendTextBytes+1), "file_ids": []string{fid}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), "files_consumed") {
		t.Fatalf("pre-TakeAll 400 must not claim files were consumed: %s", w.Body.String())
	}
	if store.Peek(fid, owner) == nil {
		t.Fatal("pre-TakeAll rejection must leave the upload in the store")
	}

	// Post-TakeAll rejection: remote node + attachment → 400 with the flag,
	// and the store entry is gone (so a blind retry would 400 again).
	fid = seed()
	w = postSendJSON(t, h, token, map[string]any{"key": "feishu:p2p:u1", "text": "hi", "node": "remote1", "file_ids": []string{fid}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	var body struct {
		Error         string `json:"error"`
		FilesConsumed bool   `json:"files_consumed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if !body.FilesConsumed {
		t.Fatalf("post-TakeAll 400 must set files_consumed: %s", w.Body.String())
	}
	if body.Error != "files not supported for remote nodes" {
		t.Fatalf("error = %q", body.Error)
	}
	if store.Peek(fid, owner) != nil {
		t.Fatal("TakeAll should have consumed the upload (flag would otherwise be a lie)")
	}
}
