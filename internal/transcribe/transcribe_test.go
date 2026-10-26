package transcribe

import (
	"context"
	"errors"
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

// TestBuildInput covers single-language, multi-language and the
// R202606c-CR-001 degrade cases. A LanguageCode that contains a comma (so
// isMultiLang() returns true) but normalizes to a single entry — e.g. a
// trailing comma "zh-CN," or a leading comma ",en-US" — must NOT set
// IdentifyMultipleLanguages with a one-element LanguageOptions. AWS
// Transcribe Streaming returns a 400 BadRequestException when
// IdentifyMultipleLanguages=true is paired with fewer than two languages,
// which would fail every transcription. The builder must degrade to
// single-LanguageCode using the surviving entry.
func TestBuildInput(t *testing.T) {
	tests := []struct {
		name          string
		lang          string
		wantMultiLang bool   // isMultiLang() precondition
		wantLangCode  string // single-language LanguageCode, "" for multi
		wantOptions   string // multi-language LanguageOptions, "" for nil
		wantPreferred string
	}{
		{"single language", "zh-CN", false, "zh-CN", "", ""},
		{"default", "", false, "zh-CN", "", ""},
		{"two languages", "zh-CN,en-US", true, "", "zh-CN,en-US", "zh-CN"},
		{"spaces normalized", "zh-CN, en-US , ja-JP", true, "", "zh-CN,en-US,ja-JP", "zh-CN"},
		{"degrade: trailing comma", "zh-CN,", true, "zh-CN", "", ""},
		{"degrade: leading comma", ",en-US", true, "en-US", "", ""},
		{"degrade: double comma around one", ",,ja-JP,,", true, "ja-JP", "", ""},
		{"degrade: spaces and commas", " zh-CN , ", true, "zh-CN", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newWithClient(&mockTranscribeAPI{}, Config{LanguageCode: tt.lang})
			// The degrade cases DO trip isMultiLang (they contain a comma),
			// which is exactly why the degrade path matters.
			if got := svc.isMultiLang(); got != tt.wantMultiLang {
				t.Fatalf("isMultiLang() = %v, want %v for %q", got, tt.wantMultiLang, tt.lang)
			}
			input := svc.buildInput(types.MediaEncodingPcm, 16000)

			if input.LanguageCode != types.LanguageCode(tt.wantLangCode) {
				t.Errorf("LanguageCode = %q, want %q", input.LanguageCode, tt.wantLangCode)
			}
			if tt.wantOptions == "" {
				if input.IdentifyMultipleLanguages || input.LanguageOptions != nil {
					t.Errorf("IdentifyMultipleLanguages = %v, LanguageOptions = %v; want false, nil for a single language",
						input.IdentifyMultipleLanguages, input.LanguageOptions)
				}
			} else {
				if !input.IdentifyMultipleLanguages {
					t.Error("IdentifyMultipleLanguages should be true")
				}
				if input.LanguageOptions == nil || *input.LanguageOptions != tt.wantOptions {
					t.Errorf("LanguageOptions = %v, want %q", input.LanguageOptions, tt.wantOptions)
				}
			}
			if input.PreferredLanguage != types.LanguageCode(tt.wantPreferred) {
				t.Errorf("PreferredLanguage = %q, want %q", input.PreferredLanguage, tt.wantPreferred)
			}
		})
	}
}
