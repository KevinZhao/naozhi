package feishu

import (
	"strings"
	"testing"
)

// TestAudioMagicOK_AcceptedFormats pins the magic-byte allowlist for audio
// payloads coming through downloadResource (R175-P2). Defence-in-depth on top
// of http.DetectContentType: even if the sniffer reports audio/*, the first
// bytes must match one of the families we actually expect a Feishu voice
// message to carry.
func TestAudioMagicOK_AcceptedFormats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix []byte
		want   bool
	}{
		// Ogg (the overwhelmingly common Feishu voice container).
		{"ogg", []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00"), true},
		// MP3 with ID3v2 tag.
		{"mp3_id3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), true},
		// MP3 raw frame sync (0xFFFB is common MPEG-1 Layer III).
		{"mp3_frame_fffb", []byte{0xFF, 0xFB, 0x90, 0x64, 0, 0, 0, 0, 0, 0}, true},
		{"mp3_frame_fff3", []byte{0xFF, 0xF3, 0x82, 0xC4, 0, 0, 0, 0, 0, 0}, true},
		{"mp3_frame_fff2", []byte{0xFF, 0xF2, 0x82, 0xC4, 0, 0, 0, 0, 0, 0}, true},
		// WAV (RIFF...WAVE).
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), true},
		// MP4/M4A (ftyp box with M4A brand at offset 4).
		{"m4a_ftyp", []byte("\x00\x00\x00\x20ftypM4A "), true},
		{"m4a_ftyp_mp42", []byte("\x00\x00\x00\x20ftypmp42"), true},
		// FLAC.
		{"flac", []byte("fLaC\x00\x00\x00\x22"), true},

		// Explicitly rejected: known non-audio families sometimes handed over
		// by a compromised proxy or SSRF-able endpoint.
		{"png_image", []byte("\x89PNG\r\n\x1a\n\x00\x00"), false},
		{"pdf", []byte("%PDF-1.4\n%\xe2"), false},
		{"html", []byte("<!doctype html>"), false},
		{"gzip", []byte{0x1F, 0x8B, 0x08, 0, 0, 0, 0, 0, 0, 0}, false},
		{"zip", []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"), false},
		{"elf", []byte{0x7F, 'E', 'L', 'F', 0, 0, 0, 0, 0, 0}, false},
		{"shell_script", []byte("#!/bin/sh\n"), false},

		// ID3-prefix attack: ASCII "ID3" but version byte outside v2/3/4
		// range must not be admitted as MP3.
		{"id3_bad_version", []byte("ID3\x05\x00\x00\x00\x00\x00\x00"), false},
		{"id3_ascii_text", []byte("ID3my playlist\n"), false},

		// ftyp brands not on the accept list — QuickTime / Flash Video are
		// syntactically ftyp containers but fall outside what we want to feed
		// into the transcribe pipeline.
		{"ftyp_qt_rejected", []byte("\x00\x00\x00\x20ftypqt  "), false},
		{"ftyp_f4v_rejected", []byte("\x00\x00\x00\x20ftypf4v "), false},

		// Formats Feishu is not expected to emit — rejected by design so a
		// crafted audio/* payload cannot broaden the Whisper/ffmpeg surface.
		{"amr_rejected", []byte("#!AMR\n\x00\x00\x00\x00\x00"), false},
		{"aac_adts_rejected", []byte{0xFF, 0xF1, 0x50, 0x80, 0, 0, 0, 0, 0, 0}, false},

		// Edge cases.
		{"empty", []byte{}, false},
		{"short_ogg_prefix", []byte("Og"), false},
		{"riff_but_not_wave", []byte("RIFF\x00\x00\x00\x00AVI LIST"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := audioMagicOK(tc.prefix)
			if got != tc.want {
				t.Errorf("audioMagicOK(%q) = %v, want %v", previewBytes(tc.prefix), got, tc.want)
			}
		})
	}
}

func previewBytes(b []byte) string {
	if len(b) > 16 {
		b = b[:16]
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			sb.WriteString(".")
		}
	}
	return sb.String()
}
