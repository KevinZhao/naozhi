package server

import (
	"bytes"
	"testing"
)

// jpegPrefix is a minimal JFIF SOI+APP0 header so http.DetectContentType
// reports image/jpeg from the leading bytes.
var jpegPrefix = []byte{
	0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10,
	'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00,
	0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
}

// TestPdfNestedInImage_R20260613SEC4 exercises pdfNestedInImage directly
// after the R20260613-SEC-4 fix: full-buffer scan instead of a 16 KB window.

// TestPdfNestedInImage_Empty confirms an empty buffer returns false.
func TestPdfNestedInImage_Empty(t *testing.T) {
	t.Parallel()
	if pdfNestedInImage(nil) {
		t.Error("nil: got true, want false")
	}
	if pdfNestedInImage([]byte{}) {
		t.Error("empty: got true, want false")
	}
}

// TestPdfNestedInImage_NoPDF confirms a plain JPEG buffer (no %PDF-) returns false.
func TestPdfNestedInImage_NoPDF(t *testing.T) {
	t.Parallel()
	data := append(bytes.Clone(jpegPrefix), make([]byte, 1024)...)
	if pdfNestedInImage(data) {
		t.Error("clean jpeg: got true, want false")
	}
}

// TestPdfNestedInImage_PDFAtStart confirms a buffer starting with %PDF- is detected.
func TestPdfNestedInImage_PDFAtStart(t *testing.T) {
	t.Parallel()
	data := []byte("%PDF-1.7 malicious")
	if !pdfNestedInImage(data) {
		t.Error("PDF at start: got false, want true")
	}
}

// TestPdfNestedInImage_PDFEarlyInBuffer confirms detection when %PDF- is
// within the first few bytes.
func TestPdfNestedInImage_PDFEarlyInBuffer(t *testing.T) {
	t.Parallel()
	data := append(bytes.Clone(jpegPrefix), []byte("%PDF-1.4\n")...)
	if !pdfNestedInImage(data) {
		t.Error("PDF early: got false, want true")
	}
}

// TestPdfNestedInImage_PDFExactlyAt16KB confirms %PDF- placed exactly at the
// 16 KB boundary (which the old window would miss) is now caught.
// R20260613-SEC-4.
func TestPdfNestedInImage_PDFExactlyAt16KB(t *testing.T) {
	t.Parallel()
	const oldWindow = 16 * 1024
	// Build a buffer whose %PDF- starts at exactly oldWindow (byte 16384).
	data := make([]byte, oldWindow)
	copy(data, jpegPrefix)
	data = append(data, []byte("%PDF-1.7 bypass")...)
	if !pdfNestedInImage(data) {
		t.Errorf("PDF at exactly 16KB offset: got false, want true (R20260613-SEC-4 full-buffer scan)")
	}
}

// TestPdfNestedInImage_PDFBeyond16KB confirms %PDF- placed well past 16 KB
// (old window would have missed it) is caught by the full-buffer scan.
// R20260613-SEC-4.
func TestPdfNestedInImage_PDFBeyond16KB(t *testing.T) {
	t.Parallel()
	// Place %PDF- at ~20 KB — unambiguously past the old 16 KB window.
	data := make([]byte, 20*1024)
	copy(data, jpegPrefix)
	data = append(data, []byte("%PDF-1.7 deep-bypass")...)
	if !pdfNestedInImage(data) {
		t.Errorf("PDF at ~20KB: got false, want true (R20260613-SEC-4 full-buffer scan)")
	}
}

// TestPdfNestedInImage_AllBytesLeAfterMS_BelowThreshold verifies that a
// buffer whose only non-zero content precedes the %PDF- prefix returns false
// when there is no signature present at all.
func TestPdfNestedInImage_LargeCleanBuffer(t *testing.T) {
	t.Parallel()
	// 100 KB of zeros — no %PDF- anywhere.
	data := make([]byte, 100*1024)
	copy(data, jpegPrefix)
	if pdfNestedInImage(data) {
		t.Error("100KB clean buffer: got true, want false")
	}
}

// TestPdfNestedInImage_BoundaryExactlyAtAfterMS checks that a %PDF- that
// starts at the very last 5 bytes of the buffer is still detected (no
// off-by-one at the end).
func TestPdfNestedInImage_PDFAtVeryEnd(t *testing.T) {
	t.Parallel()
	data := make([]byte, 100)
	copy(data[95:], []byte("%PDF-"))
	if !pdfNestedInImage(data) {
		t.Error("PDF at very end: got false, want true")
	}
}
