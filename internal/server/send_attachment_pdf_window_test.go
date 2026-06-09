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

// TestParseAttachmentFile_JFIF_PDFMarkerBeyond16K confirms the window stays
// bounded: a %PDF- marker beyond 16 KB is NOT scanned (constant-bounded cost),
// so a legitimately large image is not penalised by a full-body scan. This
// documents the deliberate boundary rather than asserting a security gap — a
// real nested-PDF exploit needs the magic early enough for a PDF reader to
// find it, which the 16 KB window covers.
func TestParseAttachmentFile_JFIF_PDFMarkerBeyond16K(t *testing.T) {
	body := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00,
		0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
	}
	body = append(body, make([]byte, 20*1024)...)
	body = append(body, []byte("\n%PDF-1.7\ntrailing\n")...)

	fh := makeMultipartFile(t, "big.jpg", "image/jpeg", body)
	// Marker is past the 16 KB scan window, so it is accepted as an image.
	att, err := parseAttachmentFile(fh, true)
	if err != nil {
		t.Fatalf("image with %%PDF- past 16KB window should pass, got: %v", err)
	}
	if att.MimeType != "image/jpeg" {
		t.Errorf("MimeType=%q want image/jpeg", att.MimeType)
	}
}
