package project

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An overwrite that fails mid-copy must leave the original as it was. Here
// the part is one byte past the size cap but inside the body cap, so the
// handler opens the target and only then hits the limit: truncating in place
// left a 256 MiB stub where the original had been.
func TestHandleFilesUpload_OverwriteFailureKeepsOriginal(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 256 MiB through the handler")
	}
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"exists.txt": "precious original"})
	target := filepath.Join(projDir, "exists.txt")
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("project", proj)
		fw, err := mw.CreateFormFile("file", "exists.txt")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		chunk := bytes.Repeat([]byte("A"), 1<<20)
		for i := 0; i < int(maxUploadFileBytes>>20); i++ {
			if _, err := fw.Write(chunk); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if _, err := fw.Write([]byte("A")); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/upload?overwrite=1", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.HandleFilesUpload(w, req)
	_ = pr.Close()

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d (%s)", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious original" {
		t.Errorf("original replaced by %d bytes after a failed overwrite", len(got))
	}
	entries, _ := os.ReadDir(projDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".naozhi-upload-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// A successful overwrite keeps the file's mode, as truncating in place did.
func TestHandleFilesUpload_OverwriteKeepsMode(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"run.sh": "old"})
	target := filepath.Join(projDir, "run.sh")
	if err := os.Chmod(target, 0o750); err != nil {
		t.Fatal(err)
	}
	w := doUpload(t, h, "overwrite=1", proj, "", "run.sh", []byte("new"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 750", fi.Mode().Perm())
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
}
