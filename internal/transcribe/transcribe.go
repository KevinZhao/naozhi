package transcribe

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
)

// Service transcribes audio bytes to text.
type Service interface {
	Transcribe(ctx context.Context, data []byte, mimeType string) (string, error)
}

// Config for Amazon Transcribe Streaming.
type Config struct {
	Region       string // default: us-east-1
	LanguageCode string // BCP-47, default: zh-CN
}

// transcribeAPI is the subset of the client we use (testable).
type transcribeAPI interface {
	StartStreamTranscription(ctx context.Context, params *transcribestreaming.StartStreamTranscriptionInput, optFns ...func(*transcribestreaming.Options)) (*transcribestreaming.StartStreamTranscriptionOutput, error)
}

type awsService struct {
	client transcribeAPI
	cfg    Config
}

// New creates a Service backed by Amazon Transcribe Streaming.
func New(ctx context.Context, cfg Config) (Service, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.LanguageCode == "" {
		cfg.LanguageCode = "zh-CN"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := transcribestreaming.NewFromConfig(awsCfg)
	return &awsService{client: client, cfg: cfg}, nil
}

// newWithClient creates a Service with an injected API client (for testing).
func newWithClient(client transcribeAPI, cfg Config) *awsService {
	if cfg.LanguageCode == "" {
		cfg.LanguageCode = "zh-CN"
	}
	return &awsService{client: client, cfg: cfg}
}

func (s *awsService) Transcribe(ctx context.Context, data []byte, mimeType string) (string, error) {
	// Detect real format from magic bytes, fallback to mimeType hint
	detected := DetectFormat(data)
	if detected != "" {
		mimeType = detected
	}

	// Convert unsupported formats to OGG_OPUS via ffmpeg
	if !isSupportedByStreaming(mimeType) {
		converted, err := ConvertToOgg(ctx, data)
		if err != nil {
			return "", fmt.Errorf("audio convert: %w", err)
		}
		data = converted
		mimeType = "audio/ogg"
	}

	return s.streamTranscribe(ctx, data, mimeType)
}

func (s *awsService) streamTranscribe(ctx context.Context, data []byte, mimeType string) (string, error) {
	encoding, sampleRate := resolveEncoding(mimeType)

	resp, err := s.client.StartStreamTranscription(ctx,
		&transcribestreaming.StartStreamTranscriptionInput{
			LanguageCode:         types.LanguageCode(s.cfg.LanguageCode),
			MediaEncoding:        encoding,
			MediaSampleRateHertz: aws.Int32(sampleRate),
		})
	if err != nil {
		return "", fmt.Errorf("start stream: %w", err)
	}

	stream := resp.GetStream()
	defer stream.Close()

	// Send audio chunks in background
	go func() {
		const chunkSize = 64 * 1024
		for i := 0; i < len(data); i += chunkSize {
			end := min(i+chunkSize, len(data))
			if err := stream.Writer.Send(ctx, &types.AudioStreamMemberAudioEvent{
				Value: types.AudioEvent{AudioChunk: data[i:end]},
			}); err != nil {
				slog.Debug("transcribe send chunk failed", "err", err)
				stream.Writer.Close()
				return
			}
		}
		stream.Writer.Close()
	}()

	// Collect final (non-partial) transcripts
	var parts []string
	for event := range stream.Reader.Events() {
		if te, ok := event.(*types.TranscriptResultStreamMemberTranscriptEvent); ok {
			for _, r := range te.Value.Transcript.Results {
				if !r.IsPartial && len(r.Alternatives) > 0 && r.Alternatives[0].Transcript != nil {
					parts = append(parts, *r.Alternatives[0].Transcript)
				}
			}
		}
	}
	if err := stream.Reader.Err(); err != nil {
		return "", fmt.Errorf("stream read: %w", err)
	}

	return strings.TrimSpace(strings.Join(parts, " ")), nil
}

// resolveEncoding maps MIME type to Transcribe encoding and sample rate.
// Only called for supported formats (after detection/conversion).
func resolveEncoding(mimeType string) (types.MediaEncoding, int32) {
	base := mimeType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "audio/ogg":
		return types.MediaEncodingOggOpus, 48000
	case "audio/flac":
		return types.MediaEncodingFlac, 16000
	default:
		// Fallback to OGG_OPUS (caller should have converted)
		return types.MediaEncodingOggOpus, 48000
	}
}

// isSupportedByStreaming checks if a MIME type is directly supported.
func isSupportedByStreaming(mimeType string) bool {
	base := mimeType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "audio/ogg", "audio/flac":
		return true
	default:
		return false
	}
}

// DetectFormat detects audio format from magic bytes.
// Returns MIME type or empty string if unknown.
func DetectFormat(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	// OGG: "OggS"
	if data[0] == 'O' && data[1] == 'g' && data[2] == 'g' && data[3] == 'S' {
		return "audio/ogg"
	}
	// FLAC: "fLaC"
	if data[0] == 'f' && data[1] == 'L' && data[2] == 'a' && data[3] == 'C' {
		return "audio/flac"
	}
	// AMR: "#!AMR"
	if len(data) >= 5 && string(data[:5]) == "#!AMR" {
		return "audio/amr"
	}
	// MP4/M4A: ftyp box at offset 4
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		return "audio/mp4"
	}
	// RIFF/WAV
	if len(data) >= 4 && string(data[:4]) == "RIFF" {
		return "audio/wav"
	}
	return ""
}
