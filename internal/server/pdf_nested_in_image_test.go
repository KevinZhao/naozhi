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

// jpegWithPDFAt returns a jpegPrefix-led buffer of size n followed by %PDF-.
func jpegWithPDFAt(n int) []byte {
	data := make([]byte, n)
	copy(data, jpegPrefix)
	return append(data, []byte("%PDF-1.7 bypass")...)
}

// TestPdfNestedInImage: the whole buffer is scanned for %PDF-, not a 16 KB
// window (R20260613-SEC-4).
func TestPdfNestedInImage(t *testing.T) {
	t.Parallel()
	atEnd := make([]byte, 100)
	copy(atEnd[95:], "%PDF-")
	cleanLarge := make([]byte, 100*1024)
	copy(cleanLarge, jpegPrefix)
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"nil", nil, false},
		{"empty", []byte{}, false},
		{"clean jpeg", append(bytes.Clone(jpegPrefix), make([]byte, 1024)...), false},
		{"100KB clean buffer", cleanLarge, false},
		{"PDF at start", []byte("%PDF-1.7 malicious"), true},
		{"PDF right after the header", append(bytes.Clone(jpegPrefix), []byte("%PDF-1.4\n")...), true},
		{"PDF exactly at 16KB", jpegWithPDFAt(16 * 1024), true},
		{"PDF at 20KB", jpegWithPDFAt(20 * 1024), true},
		{"PDF in the last 5 bytes", atEnd, true},
	} {
		if got := pdfNestedInImage(tc.data); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
