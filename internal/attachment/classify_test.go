package attachment

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/limits"
)

func TestClassifyFile(t *testing.T) {
	t.Parallel()
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
	cases := []struct {
		name     string
		file     string
		data     []byte
		wantMime string
		wantErr  error
	}{
		{"pdf by content", "report.pdf", pdf, "application/pdf", nil},
		{"pdf without extension", "scan", pdf, "application/pdf", nil},
		{"pdf named txt is still a pdf", "notes.txt", pdf, "application/pdf", nil},
		{"spoofed pdf", "report.pdf", []byte("hello, not a pdf"), "", ErrUnsupportedType},
		{"utf8 text", "notes.txt", []byte("第一行\nsecond line\n"), "text/plain", nil},
		{"markdown upper-case ext", "README.MD", []byte("# title\n"), "text/markdown", nil},
		{"csv", "a.csv", []byte("a,b\n1,2\n"), "text/csv", nil},
		{"json", "a.json", []byte(`{"k":1}`), "application/json", nil},
		{"log", "app.log", []byte("INFO ok\n"), "text/plain", nil},
		{"yaml", "c.yaml", []byte("k: v\n"), "application/yaml", nil},
		{"yml", "c.yml", []byte("k: v\n"), "application/yaml", nil},
		{"invalid utf8 named txt", "bin.txt", []byte{0xff, 0xfe, 'a', 'b'}, "", ErrUnsupportedType},
		{"NUL byte named txt", "bin.txt", []byte("ab\x00cd"), "", ErrUnsupportedType},
		{"gzip named txt", "x.txt", []byte{0x1F, 0x8B, 0x08, 0x00}, "", ErrUnsupportedType},
		{"docx (zip)", "a.docx", []byte("PK\x03\x04rest"), "", ErrUnsupportedType},
		{"zip named txt", "a.txt", []byte("PK\x03\x04rest"), "", ErrUnsupportedType},
		{"text with unknown ext", "a.go", []byte("package main\n"), "", ErrUnsupportedType},
		{"text without ext", "Makefile", []byte("all:\n"), "", ErrUnsupportedType},
		{"traversal name keeps only the ext", "../../etc/x.txt", []byte("ok"), "text/plain", nil},
		{"empty", "a.txt", nil, "", ErrEmptyData},
		{"oversize", "big.txt", bytes.Repeat([]byte("a"), limits.MaxFileAttachmentBytes+1), "", ErrTooLarge},
		{"name past the rune cap keeps its ext", strings.Repeat("长", 200) + ".md", []byte("# t\n"), "text/markdown", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mime, err := ClassifyFile(c.file, c.data)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if mime != c.wantMime {
				t.Errorf("mime = %q, want %q", mime, c.wantMime)
			}
		})
	}
}

// TestClassifyFile_AtCapAccepted pins the cap as inclusive.
func TestClassifyFile_AtCapAccepted(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("a"), limits.MaxFileAttachmentBytes)
	if _, err := ClassifyFile("big.txt", data); err != nil {
		t.Fatalf("file at the cap rejected: %v", err)
	}
}

// TestClassifiedMimesPersistable keeps the classifier and the persist
// allowlist from drifting: every MIME type ClassifyFile can return must map
// to an extension Persist accepts.
func TestClassifiedMimesPersistable(t *testing.T) {
	t.Parallel()
	mimes := []string{"application/pdf"}
	for _, m := range textFileMime {
		mimes = append(mimes, m)
	}
	for _, m := range mimes {
		ext := ExtForMime(m)
		if got, err := sanitizeExt(ext); err != nil || got != ext {
			t.Errorf("%s: sanitizeExt(%q) = (%q, %v), want (%q, nil)", m, ext, got, err, ext)
		}
	}
	if ext := ExtForMime("image/png"); ext != "" {
		t.Errorf("ExtForMime(image/png) = %q, want \"\": images are not file refs", ext)
	}
}

func TestSanitizeOrigName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"report.pdf", "report.pdf"},
		{"合同.pdf", "合同.pdf"},
		{"../etc/passwd", ".._etc_passwd"},
		{"a/b\\c.pdf", "a_b_c.pdf"},
		{"\x00\x01evil\x7f.pdf", "evil.pdf"},
		{"x\u202egpj.pdf", "xgpj.pdf"}, // bidi override dropped
	}
	for _, c := range cases {
		if got := SanitizeOrigName(c.in); got != c.want {
			t.Errorf("SanitizeOrigName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("文", 500) + ".pdf"
	if got := SanitizeOrigName(long); len([]rune(got)) != maxOrigNameRunes {
		t.Errorf("long name: %d runes, want %d", len([]rune(got)), maxOrigNameRunes)
	}
}
