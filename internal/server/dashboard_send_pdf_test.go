package server

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
)

// makeMultipartFile builds a *multipart.FileHeader in-memory so tests can
// exercise parseAttachmentFile without spinning up an HTTP server. The
// tempfile path is created under t.TempDir so `t.Cleanup` frees it
// automatically.
func makeMultipartFile(t *testing.T, filename, contentType string, body []byte) *multipart.FileHeader {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	mr := multipart.NewReader(&buf, mw.Boundary())
	form, err := mr.ReadForm(int64(len(body)) + 4096)
	if err != nil {
		t.Fatalf("ReadForm: %v", err)
	}
	t.Cleanup(func() { _ = form.RemoveAll() })

	fhs := form.File["file"]
	if len(fhs) != 1 {
		t.Fatalf("expected 1 file header, got %d", len(fhs))
	}
	return fhs[0]
}

// pdfMagic returns a minimal byte sequence http.DetectContentType will
// sniff as application/pdf. We don't need a parseable PDF — only the magic.
func pdfMagic() []byte {
	// "%PDF-1.4\n" followed by a tiny body is enough for DetectContentType.
	return []byte("%PDF-1.4\nfake body bytes for sniffing\n")
}

func TestParseAttachmentFile_PDF_Accepted(t *testing.T) {
	data := pdfMagic()
	fh := makeMultipartFile(t, "report.pdf", "application/pdf", data)

	att, err := parseAttachmentFile(fh, true)
	if err != nil {
		t.Fatalf("parseAttachmentFile: %v", err)
	}
	if att.Kind != cli.KindFileRef {
		t.Errorf("Kind=%q want %q", att.Kind, cli.KindFileRef)
	}
	if att.MimeType != "application/pdf" {
		t.Errorf("MimeType=%q", att.MimeType)
	}
	if att.OrigName != "report.pdf" {
		t.Errorf("OrigName=%q", att.OrigName)
	}
	if int(att.Size) != len(data) {
		t.Errorf("Size=%d want %d", att.Size, len(data))
	}
	if !bytes.Equal(att.Data, data) {
		t.Errorf("Data mismatch")
	}
}

func TestParseAttachmentFile_PDF_NotAllowed(t *testing.T) {
	data := pdfMagic()
	fh := makeMultipartFile(t, "x.pdf", "application/pdf", data)

	_, err := parseAttachmentFile(fh, false)
	if err == nil || !strings.Contains(err.Error(), "upload") {
		t.Errorf("expected reject when allowPDF=false, got %v", err)
	}
}

func TestParseAttachmentFile_PDF_MagicMismatch(t *testing.T) {
	// Claim PDF but ship JPEG bytes. Defence against rename/spoof.
	body := []byte("\xff\xd8\xff\xe0fake jpeg bytes")
	fh := makeMultipartFile(t, "spoof.pdf", "application/pdf", body)

	_, err := parseAttachmentFile(fh, true)
	if err == nil || !strings.Contains(err.Error(), "PDF") {
		t.Errorf("expected magic mismatch reject, got %v", err)
	}
}

func TestParseAttachmentFile_Image_StillWorks(t *testing.T) {
	// 8-byte PNG signature + minimal IHDR so DetectContentType returns
	// image/png. The real raster data is unnecessary — sniff is header-based.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}
	fh := makeMultipartFile(t, "img.png", "image/png", png)

	att, err := parseAttachmentFile(fh, true)
	if err != nil {
		t.Fatalf("parseAttachmentFile image: %v", err)
	}
	if att.Kind != cli.KindImageInline {
		t.Errorf("Kind=%q want %q", att.Kind, cli.KindImageInline)
	}
	if att.MimeType != "image/png" {
		t.Errorf("MimeType=%q", att.MimeType)
	}
}

func TestSanitizeClientFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"report.pdf", "report.pdf"},
		{"合同.pdf", "合同.pdf"},                    // utf-8 preserved
		{"../etc/passwd", "_.._etc_passwd"[1:]}, // path separator collapsed; no leading '.' stripping (we don't care)
		{"a/b\\c.pdf", "a_b_c.pdf"},
		{"\x00\x01evil\x7f.pdf", "evil.pdf"}, // control chars stripped
	}
	for _, c := range cases {
		got := sanitizeClientFilename(c.in)
		if got != c.want {
			t.Errorf("sanitizeClientFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Long-name truncation.
	long := strings.Repeat("a", 500) + ".pdf"
	got := sanitizeClientFilename(long)
	if len([]rune(got)) > 120 {
		t.Errorf("expected <=120 runes, got %d", len([]rune(got)))
	}
}

func TestPersistFileRefs_WritesToWorkspace(t *testing.T) {
	ws := t.TempDir()
	atts := []cli.Attachment{{
		Kind:     cli.KindFileRef,
		Data:     pdfMagic(),
		MimeType: "application/pdf",
		OrigName: "report.pdf",
		Size:     int64(len(pdfMagic())),
	}}

	resolved, rollback, perr := persistFileRefs(ws, atts, "dash:direct:alice:general", "alice")
	if perr != nil {
		t.Fatalf("persistFileRefs: %+v", perr)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved len=%d", len(resolved))
	}
	got := resolved[0]
	if got.WorkspacePath == "" {
		t.Error("WorkspacePath empty")
	}
	if got.Data != nil {
		t.Error("Data should be cleared after persist to release memory")
	}
	// File exists on disk at the declared relative path.
	abs := filepath.Join(ws, filepath.FromSlash(got.WorkspacePath))
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("file missing at %s: %v", abs, err)
	}

	// Rollback removes it.
	rollback()
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("rollback did not remove file, stat err=%v", err)
	}
}

// TestPersistFileRefs_InlineImage_PersistsCopy verifies the "view
// original" path: inline images are best-effort persisted so the
// dashboard lightbox can load the full-size image over HTTP. The
// inline Data MUST remain populated — the CLI still needs to receive
// the bytes in the user message content block.
func TestPersistFileRefs_InlineImage_PersistsCopy(t *testing.T) {
	ws := t.TempDir()
	atts := []cli.Attachment{{
		Kind:     cli.KindImageInline,
		Data:     []byte("PNG-bytes"),
		MimeType: "image/png",
		OrigName: "photo.png",
	}}
	resolved, rb, perr := persistFileRefs(ws, atts, "dash:direct:alice:general", "alice")
	if perr != nil {
		t.Fatalf("persistFileRefs: %+v", perr)
	}
	defer rb()
	if len(resolved) != 1 {
		t.Fatalf("resolved len=%d", len(resolved))
	}
	got := resolved[0]
	if got.Kind != cli.KindImageInline {
		t.Errorf("Kind=%q want %q", got.Kind, cli.KindImageInline)
	}
	// Data MUST survive the persist — the CLI user-message content block
	// carries inline bytes. Unlike file_ref we do not clear it after write.
	if !bytes.Equal(got.Data, []byte("PNG-bytes")) {
		t.Errorf("Data corrupted after inline persist: %q", got.Data)
	}
	if got.WorkspacePath == "" {
		t.Fatal("WorkspacePath empty — lightbox would fall back to thumbnail")
	}
	if !strings.HasPrefix(got.WorkspacePath, ".naozhi/attachments/") {
		t.Errorf("WorkspacePath=%q not under attachment dir", got.WorkspacePath)
	}
	// File exists on disk at the declared relative path.
	abs := filepath.Join(ws, filepath.FromSlash(got.WorkspacePath))
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(b, []byte("PNG-bytes")) {
		t.Errorf("on-disk bytes mismatch: %q", b)
	}
}

// TestPersistFileRefs_InlineImage_PersistFailureIsNonFatal — if the
// workspace write fails for an inline image, we still forward the
// attachment to the CLI (the user's message must not be blocked by a
// cosmetic lightbox degradation).
func TestPersistFileRefs_InlineImage_UnknownMimeSkipsPersist(t *testing.T) {
	ws := t.TempDir()
	atts := []cli.Attachment{{
		Kind:     cli.KindImageInline,
		Data:     []byte("garbage"),
		MimeType: "image/tiff", // not in allowlist
	}}
	resolved, rb, perr := persistFileRefs(ws, atts, "k", "o")
	if perr != nil {
		t.Fatalf("persist unexpectedly failed: %+v", perr)
	}
	defer rb()
	if resolved[0].WorkspacePath != "" {
		t.Errorf("WorkspacePath should stay empty for unsupported mime, got %q", resolved[0].WorkspacePath)
	}
	// Data untouched — the CLI will still receive the inline bytes.
	if !bytes.Equal(resolved[0].Data, []byte("garbage")) {
		t.Errorf("Data corrupted on non-persist path")
	}
}

// TestHasPersistableAttachment covers the hot-path gate in
// dashboard_send.go / wshub.go. Any image_inline with bytes and a
// recognised MIME must trigger persist, because the lightbox relies
// on that workspace copy.
func TestHasPersistableAttachment(t *testing.T) {
	cases := []struct {
		name string
		atts []cli.Attachment
		want bool
	}{
		{"empty", nil, false},
		{"text-only", []cli.Attachment{}, false},
		{"image-png", []cli.Attachment{{
			Kind: cli.KindImageInline, Data: []byte("x"), MimeType: "image/png",
		}}, true},
		{"image-jpeg", []cli.Attachment{{
			Kind: cli.KindImageInline, Data: []byte("x"), MimeType: "image/jpeg",
		}}, true},
		{"image-unknown-mime", []cli.Attachment{{
			Kind: cli.KindImageInline, Data: []byte("x"), MimeType: "image/tiff",
		}}, false},
		{"file_ref", []cli.Attachment{{
			Kind: cli.KindFileRef, MimeType: "application/pdf",
		}}, true},
	}
	for _, c := range cases {
		if got := hasPersistableAttachment(c.atts); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPersistFileRefs_MixedImageAndPDF(t *testing.T) {
	ws := t.TempDir()
	atts := []cli.Attachment{
		{Kind: cli.KindImageInline, Data: []byte("png"), MimeType: "image/png"},
		{Kind: cli.KindFileRef, Data: pdfMagic(), MimeType: "application/pdf", OrigName: "x.pdf"},
	}
	resolved, rb, perr := persistFileRefs(ws, atts, "k", "o")
	if perr != nil {
		t.Fatalf("%+v", perr)
	}
	defer rb()

	if resolved[0].Kind != cli.KindImageInline || !bytes.Equal(resolved[0].Data, []byte("png")) {
		t.Error("image passthrough corrupted")
	}
	if resolved[1].Kind != cli.KindFileRef || resolved[1].WorkspacePath == "" {
		t.Error("PDF not persisted")
	}
}

func TestPersistFileRefs_EmptyWorkspace(t *testing.T) {
	atts := []cli.Attachment{{Kind: cli.KindFileRef, Data: pdfMagic(), MimeType: "application/pdf"}}
	_, _, perr := persistFileRefs("", atts, "k", "o")
	if perr == nil {
		t.Fatal("expected error for empty workspace")
	}
	if perr.status != 400 {
		t.Errorf("status=%d want 400", perr.status)
	}
}

func TestPersistFileRefs_UnsupportedMime(t *testing.T) {
	ws := t.TempDir()
	atts := []cli.Attachment{{
		Kind: cli.KindFileRef, Data: []byte("x"),
		MimeType: "application/msword",
	}}
	_, _, perr := persistFileRefs(ws, atts, "k", "o")
	if perr == nil {
		t.Fatal("expected error for unsupported mime")
	}
	// No files left behind
	root := filepath.Join(ws, attachment.Dir)
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		// It is acceptable for the date directory to exist (MkdirAll is
		// idempotent) but it must contain no files.
		for _, e := range entries {
			sub := filepath.Join(root, e.Name())
			if items, _ := os.ReadDir(sub); len(items) > 0 {
				t.Errorf("unexpected files under %s: %v", sub, items)
			}
		}
	}
}
