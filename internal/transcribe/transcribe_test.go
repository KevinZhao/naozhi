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
