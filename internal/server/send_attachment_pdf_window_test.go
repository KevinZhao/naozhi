package server

import (
	"strings"
	"testing"
)

// TestParseAttachmentFile_JFIFWithPDFBody_PastOldWindow pins R20260607-SEC-8
// (#1890): the nested-PDF scan window was 4 KB, which a crafted JPEG could step
// past by padding its JFIF header (e.g. an oversized ICC profile or stacked
// APPn segments) beyond 4 KB so the "%PDF-" payload landed outside the scan
// while http.DetectContentType still sniffed image/jpeg from the leading bytes.
// Widening the window to 16 KB must catch a %PDF- marker placed at ~8 KB.
func TestParseAttachmentFile_JFIFWithPDFBody_PastOldWindow(t *testing.T) {
	// JFIF/JPEG SOI + APP0 so DetectContentType reports image/jpeg.
	body := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00,
		0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
	}
	// Pad with benign filler so the %PDF- marker lands at ~8 KB — comfortably
	// past the old 4 KB window but inside the new 16 KB one.
	body = append(body, make([]byte, 8*1024)...)
	body = append(body, []byte("\n%PDF-1.7\n%\xff\xff\xff\xff\nnested pdf body\n")...)

	fh := makeMultipartFile(t, "trojan.jpg", "image/jpeg", body)
	if _, err := parseAttachmentFile(fh, true); err == nil {
		t.Fatal("expected JFIF+PDF nested-container rejection for marker past 4KB, got nil error")
	} else if !strings.Contains(err.Error(), "PDF") {
		t.Errorf("expected error to mention PDF, got: %v", err)
	}
}

// TestParseAttachmentFile_JFIF_PDFMarkerBeyond16K_NowRejected pins
// R20260613-SEC-4: the old 16 KB window left a bypass open — a crafted JFIF
// with a large APP13/ICC segment could push "%PDF-" past 16 KB while
// http.DetectContentType still reported image/jpeg. The fix switches to a
// full-buffer scan, so a "%PDF-" at any offset (including well beyond 16 KB)
// is now correctly rejected.
func TestParseAttachmentFile_JFIF_PDFMarkerBeyond16K_NowRejected(t *testing.T) {
	body := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00,
		0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
	}
	// Place "%PDF-" well past the old 16 KB boundary (at ~20 KB).
	body = append(body, make([]byte, 20*1024)...)
	body = append(body, []byte("\n%PDF-1.7\ntrailing\n")...)

	fh := makeMultipartFile(t, "big.jpg", "image/jpeg", body)
	// R20260613-SEC-4: full-buffer scan catches the marker at any offset.
	if _, err := parseAttachmentFile(fh, true); err == nil {
		t.Fatal("expected rejection: %%PDF- past old 16KB window must now be caught by full-buffer scan")
	} else if !strings.Contains(err.Error(), "PDF") {
		t.Errorf("expected error mentioning PDF, got: %v", err)
	}
}
