package transcribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"

	"github.com/naozhi/naozhi/internal/osutil"
)

// maxTranscriptBytes caps a returned transcript (real-world is sub-KB) so a
// runaway server stream cannot fan unbounded text into IM message buffers.
const maxTranscriptBytes = 16 * 1024

// Service transcribes audio bytes to text.
type Service interface {
	Transcribe(ctx context.Context, data []byte, mimeType string) (string, error)
}

// Config for Amazon Transcribe Streaming. A comma-separated LanguageCode
// ("zh-CN,en-US") enables automatic multi-language identification.
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
	// Magic bytes win over the mimeType hint.
	detected := DetectFormat(data)
	if detected != "" {
		mimeType = detected
	}

	if isSupportedByStreaming(mimeType) {
		return s.streamFromBuffer(ctx, data, mimeType)
	}

	// Unsupported formats: ffmpeg → PCM → Transcribe concurrently.
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
	// If collectTranscripts returns early the deferred stream.Close would race
	// an in-flight Writer.Send; gate the Close on the sender's completion.
	senderDone := make(chan struct{})
	defer func() {
		<-senderDone
		stream.Close()
	}()

	go func() {
		defer close(senderDone)
		const chunkSize = 16 * 1024
		for i := 0; i < len(data); i += chunkSize {
			end := min(i+chunkSize, len(data))
			if err := stream.Writer.Send(ctx, &types.AudioStreamMemberAudioEvent{
				Value: types.AudioEvent{AudioChunk: data[i:end]},
			}); err != nil {
				slog.Debug("transcribe send chunk failed", "err", err)
				// break (not return): Writer.Close must run on every exit path
				// so the SDK signals EOF and collectTranscripts' Reader.Err
				// surfaces instead of hanging until ctx cancellation.
				break
			}
		}
		stream.Writer.Close()
	}()

	return collectTranscripts(stream)
}

// streamFromFFmpeg starts ffmpeg PCM conversion and streams output directly
// to Transcribe; conversion and upload run concurrently via pipe.
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

	return pumpPCMToTranscribe(ctx, pcm, resp.GetStream())
}

// pumpPCMToTranscribe streams ffmpeg PCM output into the Transcribe event
// stream and collects the transcript. Split out so the sender goroutine and
// ffmpeg-error propagation are unit-testable with a mocked event stream.
func pumpPCMToTranscribe(ctx context.Context, pcm *pcmStream, stream *transcribestreaming.StartStreamTranscriptionEventStream) (string, error) {
	// Wait for the sender goroutine before stream.Close() (see streamFromBuffer).
	senderDone := make(chan struct{})
	defer func() {
		<-senderDone
		stream.Close()
	}()

	// The sender goroutine solely owns pcm.Close() and carries ffmpeg's exit
	// error over a buffered (cap-1) channel: the send never blocks and the
	// receive happens-after it. Discarding it would turn a transcode failure
	// into a silent ("", nil) success (#1781).
	ffmpegErrCh := make(chan error, 1)

	go func() {
		defer close(senderDone)
		defer func() { ffmpegErrCh <- pcm.Close() }()
		buf := make([]byte, 16*1024)
		for {
			n, readErr := pcm.Read(buf)
			if n > 0 {
				// buf is reused; the SDK serializing AudioChunk synchronously
				// is not part of its public contract, so copy.
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
				if !errors.Is(readErr, io.EOF) {
					slog.Debug("ffmpeg read failed", "err", readErr)
				}
				break
			}
		}
		stream.Writer.Close()
	}()

	transcript, err := collectTranscripts(stream)
	if err != nil {
		// Join the ffmpeg exit error so an operator sees when the conversion
		// (e.g. unsupported codec) was the real cause; senderDone first, race-free.
		<-senderDone
		if ffmpegErr := <-ffmpegErrCh; ffmpegErr != nil {
			return "", errors.Join(err, fmt.Errorf("audio convert: %w", ffmpegErr))
		}
		return "", err
	}
	// A non-empty transcript means usable PCM reached Transcribe before ffmpeg
	// died; surface the convert error only when nothing was transcribed (#1781).
	if transcript == "" {
		if ffmpegErr := <-ffmpegErrCh; ffmpegErr != nil {
			return "", fmt.Errorf("audio convert: %w", ffmpegErr)
		}
	}
	return transcript, nil
}

// collectTranscripts reads final transcript results from the stream. Responses
// flow into IM messages and slog attributes, so the joined transcript passes
// through osutil.SanitizeForLog to strip bidi / LS-PS / C0-C1 control runes a
// crafted upstream could use to flip log rendering (CJK / emoji preserved).
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

	joined := strings.TrimSpace(strings.Join(parts, " "))
	return osutil.SanitizeForLog(joined, maxTranscriptBytes), nil
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
		// Strip spaces and drop empty segments so stray commas (",en-US",
		// "zh-CN,", ",,") do not leave PreferredLanguage = "" (AWS 400).
		raw := strings.Split(s.cfg.LanguageCode, ",")
		parts := raw[:0]
		for _, p := range raw {
			if t := strings.TrimSpace(p); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			// Fall back to single-LanguageCode with the raw string (AWS errors clearly).
			input.IdentifyMultipleLanguages = false
			input.LanguageCode = types.LanguageCode(s.cfg.LanguageCode)
			return input
		}
		if len(parts) == 1 {
			// AWS rejects IdentifyMultipleLanguages=true with fewer than two
			// LanguageOptions (400), so degrade to single-LanguageCode.
			input.IdentifyMultipleLanguages = false
			input.LanguageCode = types.LanguageCode(parts[0])
			return input
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
	// RIFF is shared by WAV, AVI, WEBP, AIFF-RIFF; only the WAVE subtype is
	// audio, otherwise a WEBP image or AVI video would be mislabelled.
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return "audio/wav"
	}
	return ""
}
