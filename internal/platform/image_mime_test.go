package platform

import "testing"

func TestSupportedImageMIME(t *testing.T) {
	t.Parallel()
	for mime, want := range map[string]bool{
		"image/png":                true,
		"image/jpeg":               true,
		"image/gif":                true,
		"image/webp":               true,
		" Image/PNG; charset=x ":   true,
		"image/bmp":                false,
		"image/x-icon":             false,
		"image/svg+xml":            false,
		"application/octet-stream": false,
		"":                         false,
	} {
		if got := SupportedImageMIME(mime); got != want {
			t.Errorf("SupportedImageMIME(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestIsImageMIME(t *testing.T) {
	t.Parallel()
	for mime, want := range map[string]bool{
		"image/png":       true,
		"image/bmp":       true,
		"IMAGE/x-icon":    true,
		"application/pdf": false,
		"image":           false,
		"":                false,
	} {
		if got := IsImageMIME(mime); got != want {
			t.Errorf("IsImageMIME(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestSniffImageMIME(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		data     []byte
		wantMIME string
		wantOK   bool
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image/png", true},
		{"gif", []byte("GIF89a\x01\x00\x01\x00"), "image/gif", true},
		{"bmp", []byte("BM\x00\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00"), "image/bmp", false},
		{"pdf", []byte("%PDF-1.7\n"), "application/pdf", false},
		{"empty", nil, "", false},
	}
	for _, tc := range cases {
		if mime, ok := SniffImageMIME(tc.data); mime != tc.wantMIME || ok != tc.wantOK {
			t.Errorf("%s: SniffImageMIME = (%q, %v), want (%q, %v)", tc.name, mime, ok, tc.wantMIME, tc.wantOK)
		}
	}
}
