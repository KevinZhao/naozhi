package platform

import "testing"

func TestSniffImageMIME(t *testing.T) {
	t.Parallel()
	if mime, ok := SniffImageMIME([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")); mime != "image/png" || !ok {
		t.Errorf("png: SniffImageMIME = (%q, %v), want (image/png, true)", mime, ok)
	}
	if mime, ok := SniffImageMIME([]byte("BM\x00\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00")); mime != "image/bmp" || ok {
		t.Errorf("bmp: SniffImageMIME = (%q, %v), want (image/bmp, false)", mime, ok)
	}
}
