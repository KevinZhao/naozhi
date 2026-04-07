package transcribe

import (
	"context"
	"fmt"
	"io"
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
// LanguageCode accepts a single BCP-47 code (e.g. "zh-CN") or a comma-separated
// list (e.g. "zh-CN,en-US") to enable automatic multi-language identification.
type Config struct {
	Region       string // default: us-east-1
	LanguageCode string // BCP-47, default: zh-CN; comma-separated for multi-language
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

	// Supported formats can be sent directly
	if isSupportedByStreaming(mimeType) {
		return s.streamFromBuffer(ctx, data, mimeType)
	}

	// Unsupported formats: stream through ffmpeg → PCM → Transcribe concurrently
	return s.streamFromFFmpeg(ctx, data)
}

// streamFromBuffer sends pre-loaded audio data to Transcribe.
func (s *awsService) streamFromBuffer(ctx context.Context, data []byte, mimeType string) (string, error) {
	encoding, sampleRate := resolveEncoding(mimeType)

	resp, err := s.client.StartStreamTranscription(ctx, s.buildInput(encoding, sampleRate))
	if err != nil {
		return "", fmt.Errorf("start stream: %w", err)
	}

	stream := resp.GetStream()
	defer stream.Close()

	go func() {
		const chunkSize = 16 * 1024
		for i := 0; i < len(data); i += chunkSize {
			end := min(i+chunkSize, len(data))
			if err := stream.Writer.Send(ctx, &types.AudioStreamMemberAudioEvent{
				Value: types.AudioEvent{AudioChunk: data[i:end]},
			}); err != nil {
				slog.Debug("transcribe send chunk failed", "err", err)
				break
			}
		}
		stream.Writer.Close()
	}()

	return collectTranscripts(stream)
}

// streamFromFFmpeg starts ffmpeg PCM conversion and streams output directly to Transcribe.
// Conversion and upload run concurrently via pipe.
func (s *awsService) streamFromFFmpeg(ctx context.Context, data []byte) (string, error) {
	pcm, err := startPCMStream(ctx, data)
	if err != nil {
		return "", fmt.Errorf("audio convert: %w", err)
	}

	resp, err := s.client.StartStreamTranscription(ctx, s.buildInput(types.MediaEncodingPcm, 16000))
	if err != nil {
		_ = pcm.Close()
		return "", fmt.Errorf("start stream: %w", err)
	}

	stream := resp.GetStream()
	defer stream.Close()

	// Read from ffmpeg stdout → send to Transcribe, concurrently with ffmpeg
	go func() {
		defer pcm.Close()
		buf := make([]byte, 16*1024)
		for {
			n, readErr := pcm.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if sendErr := stream.Writer.Send(ctx, &types.AudioStreamMemberAudioEvent{
					Value: types.AudioEvent{AudioChunk: chunk},
				}); sendErr != nil {
					slog.Debug("transcribe send chunk failed", "err", sendErr)
					break
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					slog.Debug("ffmpeg read failed", "err", readErr)
				}
				break
			}
		}
		stream.Writer.Close()
	}()

	return collectTranscripts(stream)
}

// collectTranscripts reads final transcript results from the stream.
func collectTranscripts(stream *transcribestreaming.StartStreamTranscriptionEventStream) (string, error) {
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

// isMultiLang returns true when the config specifies multiple languages.
func (s *awsService) isMultiLang() bool {
	return strings.Contains(s.cfg.LanguageCode, ",")
}

// buildInput creates the StartStreamTranscriptionInput with the correct
// language configuration (single LanguageCode vs multi-language identification).
func (s *awsService) buildInput(encoding types.MediaEncoding, sampleRate int32) *transcribestreaming.StartStreamTranscriptionInput {
	input := &transcribestreaming.StartStreamTranscriptionInput{
		MediaEncoding:        encoding,
		MediaSampleRateHertz: aws.Int32(sampleRate),
	}
	if s.isMultiLang() {
		input.IdentifyMultipleLanguages = true
		// Normalize: strip spaces around each language code
		parts := strings.Split(s.cfg.LanguageCode, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		input.LanguageOptions = aws.String(strings.Join(parts, ","))
		input.PreferredLanguage = types.LanguageCode(parts[0])
	} else {
		input.LanguageCode = types.LanguageCode(s.cfg.LanguageCode)
	}
	return input
}

// resolveEncoding maps MIME type to Transcribe encoding and sample rate.
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
	case "audio/pcm":
		return types.MediaEncodingPcm, 16000
	default:
		return types.MediaEncodingPcm, 16000
	}
}

// isSupportedByStreaming checks if a MIME type is directly supported by Transcribe Streaming.
func isSupportedByStreaming(mimeType string) bool {
	base := mimeType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "audio/ogg", "audio/flac", "audio/pcm":
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
	if data[0] == 'O' && data[1] == 'g' && data[2] == 'g' && data[3] == 'S' {
		return "audio/ogg"
	}
	if data[0] == 'f' && data[1] == 'L' && data[2] == 'a' && data[3] == 'C' {
		return "audio/flac"
	}
	if len(data) >= 5 && string(data[:5]) == "#!AMR" {
		return "audio/amr"
	}
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		return "audio/mp4"
	}
	if len(data) >= 4 && string(data[:4]) == "RIFF" {
		return "audio/wav"
	}
	return ""
}
