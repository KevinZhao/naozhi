package transcribe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"ogg magic bytes", []byte("OggS\x00rest"), "audio/ogg"},
		{"flac magic bytes", []byte("fLaC\x00rest"), "audio/flac"},
		{"amr magic bytes", []byte("#!AMR\nrest"), "audio/amr"},
		{"mp4 ftyp at offset 4", []byte("\x00\x00\x00\x1cftypM4A "), "audio/mp4"},
		{"wav riff header", []byte("RIFF\x00\x00\x00\x00WAVE"), "audio/wav"},
		{"wav riff with payload", []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00"), "audio/wav"},
		// RIFF is a generic container header; AVI / WEBP / AIFF-RIFF share
		// the same prefix. DetectFormat must not return audio/wav for them
		// — any non-WAVE RIFF falls through to "" so the caller can rely on
		// the mimeType hint rather than a misclassified audio tag.
		{"riff avi rejected", []byte("RIFF\x00\x00\x00\x00AVI LIST"), ""},
		{"riff webp rejected", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), ""},
		{"riff too short for wave check", []byte("RIFF\x00\x00\x00\x00"), ""},
		{"empty data", []byte{}, ""},
		{"short data 1 byte", []byte{0x42}, ""},
		{"short data 3 bytes", []byte{0x01, 0x02, 0x03}, ""},
		{"unknown magic", []byte("ZZZZ1234"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectFormat(tt.data)
			if got != tt.want {
				t.Errorf("DetectFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveEncoding(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
		wantEnc  types.MediaEncoding
		wantRate int32
	}{
		{"audio/ogg", "audio/ogg", types.MediaEncodingOggOpus, 48000},
		{"ogg with codec param", "audio/ogg; codecs=opus", types.MediaEncodingOggOpus, 48000},
		{"audio/flac", "audio/flac", types.MediaEncodingFlac, 16000},
		{"audio/pcm", "audio/pcm", types.MediaEncodingPcm, 16000},
		{"unknown falls back to pcm", "audio/wav", types.MediaEncodingPcm, 16000},
		{"empty string falls back", "", types.MediaEncodingPcm, 16000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, rate := resolveEncoding(tt.mimeType)
			if enc != tt.wantEnc {
				t.Errorf("encoding = %v, want %v", enc, tt.wantEnc)
			}
			if rate != tt.wantRate {
				t.Errorf("sampleRate = %d, want %d", rate, tt.wantRate)
			}
		})
	}
}

func TestIsSupportedByStreaming(t *testing.T) {
	tests := []struct {
		mimeType string
		want     bool
	}{
		{"audio/ogg", true},
		{"audio/flac", true},
		{"audio/pcm", true},
		{"audio/amr", false},
		{"audio/mp4", false},
		{"audio/wav", false},
		{"", false},
		{"audio/ogg; codecs=opus", true},
	}
	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			if got := isSupportedByStreaming(tt.mimeType); got != tt.want {
				t.Errorf("isSupportedByStreaming(%q) = %v, want %v", tt.mimeType, got, tt.want)
			}
		})
	}
}

// mockTranscribeAPI implements transcribeAPI for testing.
type mockTranscribeAPI struct {
	err error
}

func (m *mockTranscribeAPI) StartStreamTranscription(
	ctx context.Context,
	params *transcribestreaming.StartStreamTranscriptionInput,
	optFns ...func(*transcribestreaming.Options),
) (*transcribestreaming.StartStreamTranscriptionOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return nil, errors.New("not implemented")
}

func TestStreamTranscribe_StartError(t *testing.T) {
	wantErr := errors.New("service unavailable")
	svc := newWithClient(&mockTranscribeAPI{err: wantErr}, Config{})

	_, err := svc.streamFromBuffer(context.Background(), []byte("fake-audio"), "audio/ogg")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapped %v", err, wantErr)
	}
}

func TestNewWithClient_DefaultLanguageCode(t *testing.T) {
	svc := newWithClient(&mockTranscribeAPI{}, Config{})
	if svc.cfg.LanguageCode != "zh-CN" {
		t.Errorf("default LanguageCode = %q, want %q", svc.cfg.LanguageCode, "zh-CN")
	}

	svc2 := newWithClient(&mockTranscribeAPI{}, Config{LanguageCode: "en-US"})
	if svc2.cfg.LanguageCode != "en-US" {
		t.Errorf("LanguageCode = %q, want %q", svc2.cfg.LanguageCode, "en-US")
	}
}

func TestIsMultiLang(t *testing.T) {
	tests := []struct {
		name string
		lang string
		want bool
	}{
		{"single language", "zh-CN", false},
		{"two languages", "zh-CN,en-US", true},
		{"three languages", "zh-CN,en-US,ja-JP", true},
		{"default", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{LanguageCode: tt.lang}
			if cfg.LanguageCode == "" {
				cfg.LanguageCode = "zh-CN"
			}
			svc := newWithClient(&mockTranscribeAPI{}, cfg)
			if got := svc.isMultiLang(); got != tt.want {
				t.Errorf("isMultiLang() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildInput_SingleLanguage(t *testing.T) {
	svc := newWithClient(&mockTranscribeAPI{}, Config{LanguageCode: "zh-CN"})
	input := svc.buildInput(types.MediaEncodingPcm, 16000)

	if input.LanguageCode != "zh-CN" {
		t.Errorf("LanguageCode = %q, want %q", input.LanguageCode, "zh-CN")
	}
	if input.IdentifyMultipleLanguages {
		t.Error("IdentifyMultipleLanguages should be false for single language")
	}
	if input.LanguageOptions != nil {
		t.Error("LanguageOptions should be nil for single language")
	}
}

func TestBuildInput_MultiLanguage(t *testing.T) {
	svc := newWithClient(&mockTranscribeAPI{}, Config{LanguageCode: "zh-CN,en-US"})
	input := svc.buildInput(types.MediaEncodingOggOpus, 48000)

	if !input.IdentifyMultipleLanguages {
		t.Error("IdentifyMultipleLanguages should be true")
	}
	if input.LanguageOptions == nil || *input.LanguageOptions != "zh-CN,en-US" {
		t.Errorf("LanguageOptions = %v, want %q", input.LanguageOptions, "zh-CN,en-US")
	}
	if input.PreferredLanguage != "zh-CN" {
		t.Errorf("PreferredLanguage = %q, want %q", input.PreferredLanguage, "zh-CN")
	}
	if input.LanguageCode != "" {
		t.Errorf("LanguageCode should be empty for multi-lang, got %q", input.LanguageCode)
	}
}

func TestBuildInput_MultiLanguage_Spaces(t *testing.T) {
	svc := newWithClient(&mockTranscribeAPI{}, Config{LanguageCode: "zh-CN, en-US , ja-JP"})
	input := svc.buildInput(types.MediaEncodingPcm, 16000)

	if *input.LanguageOptions != "zh-CN,en-US,ja-JP" {
		t.Errorf("LanguageOptions not normalized: got %q", *input.LanguageOptions)
	}
	if input.PreferredLanguage != "zh-CN" {
		t.Errorf("PreferredLanguage = %q, want %q", input.PreferredLanguage, "zh-CN")
	}
}

// TestLookupFFmpeg_NoProcessWideCache pins R240-SEC-9 (#1050): the previous
// implementation memoised the first exec.LookPath result for the lifetime
// of the process via sync.Once, so a startup-time PATH state stayed pinned
// even after the operator (or an attacker) removed the offending entry.
//
// This test mutates PATH between two lookupFFmpeg calls and asserts the
// second call observes the new state. The check only runs when ffmpeg is
// actually installed on the host (otherwise both sides return
// ErrFFmpegNotFound and the assertion is vacuous); the post-fix-only
// branch below ALSO covers the cache-removed contract: clearing PATH
// must produce a fresh "not found" answer immediately, not the cached
// success.
func TestLookupFFmpeg_NoProcessWideCache(t *testing.T) {
	originalPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", originalPATH) })

	// First lookup with the host's normal PATH. Capture the outcome so the
	// "PATH cleared" assertion below knows whether ffmpeg is available at
	// all on this CI worker.
	firstPath, firstErr := lookupFFmpeg()

	// Clear PATH and call again. With the sync.Once cache the function
	// would still report the previous success path; without the cache
	// (post-fix) it must report "ffmpeg not found".
	if err := os.Setenv("PATH", ""); err != nil {
		t.Fatalf("setenv PATH=\"\": %v", err)
	}
	clearedPath, clearedErr := lookupFFmpeg()
	if clearedErr == nil {
		t.Errorf("lookupFFmpeg with empty PATH returned %q, nil — expected an error "+
			"because the previous implementation cached the first PATH-resolved "+
			"binary process-wide and ignored later PATH changes (R240-SEC-9 / #1050)",
			clearedPath)
	}

	// Restore PATH and assert we again see whatever the host has —
	// re-resolving must surface a fresh outcome each time.
	os.Setenv("PATH", originalPATH)
	secondPath, secondErr := lookupFFmpeg()
	if firstErr == nil {
		// Host has ffmpeg: re-resolution must still find it after PATH
		// was bounced through empty. With the cache removed every call
		// is a fresh exec.LookPath, so success must reappear.
		if secondErr != nil {
			t.Errorf("lookupFFmpeg after PATH restore returned err %v; "+
				"expected a fresh resolution to find the same ffmpeg as the "+
				"first lookup (%q)", secondErr, firstPath)
		}
		if secondPath == "" {
			t.Errorf("lookupFFmpeg after PATH restore returned empty path; " +
				"expected re-resolution to find a binary on the restored PATH")
		}
	}
}

// TestLookupFFmpeg_OverrideEnv pins the NAOZHI_FFMPEG_PATH operator-override
// branch (#1050 follow-on): a non-empty env var bypasses PATH resolution
// entirely. A working override must succeed even when PATH is empty; a
// pointing-at-nothing override must error loudly rather than silently fall
// back to PATH (a misconfigured override should be a loud failure, not a
// degraded one).
func TestLookupFFmpeg_OverrideEnv(t *testing.T) {
	originalPATH := os.Getenv("PATH")
	originalOverride := os.Getenv(ffmpegPathEnv)
	t.Cleanup(func() {
		os.Setenv("PATH", originalPATH)
		os.Setenv(ffmpegPathEnv, originalOverride)
	})

	// Find a real executable to point the override at. Use /bin/sh which
	// every POSIX host has — we are testing the resolution flow, not the
	// downstream ffmpeg behaviour. lookupFFmpeg only verifies the binary
	// resolves and is executable; the actual ffmpeg invocation is gated
	// elsewhere.
	shellPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH; cannot exercise override branch: %v", err)
	}

	// Override set + PATH cleared: must succeed via override.
	os.Setenv(ffmpegPathEnv, shellPath)
	if err := os.Setenv("PATH", ""); err != nil {
		t.Fatalf("setenv PATH=\"\": %v", err)
	}
	got, err := lookupFFmpeg()
	if err != nil {
		t.Fatalf("override %q with empty PATH: lookupFFmpeg err=%v; "+
			"expected the override to bypass PATH resolution entirely",
			shellPath, err)
	}
	if got != shellPath {
		t.Errorf("override returned %q, want %q (must echo the env override "+
			"verbatim, not re-resolve through PATH)", got, shellPath)
	}

	// Override pointing at a missing path must error, not fall back to PATH.
	os.Setenv(ffmpegPathEnv, "/nonexistent/ffmpeg-fake")
	os.Setenv("PATH", originalPATH)
	if _, err := lookupFFmpeg(); err == nil {
		t.Error("override pointing at missing binary returned nil err; " +
			"expected an explicit failure rather than silent PATH fallback")
	}

	// Empty override falls back to PATH (regression guard for "" handling).
	os.Setenv(ffmpegPathEnv, "")
	if _, err := lookupFFmpeg(); err != nil && originalPATH != "" {
		// PATH lookup may legitimately fail on hosts without ffmpeg — only
		// assert when the cleanup PATH actually contains it.
		if _, lookErr := exec.LookPath("ffmpeg"); lookErr == nil {
			t.Errorf("empty override + PATH-with-ffmpeg returned err %v; "+
				"expected fallback to PATH lookup", err)
		}
	}
}
