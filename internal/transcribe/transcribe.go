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

// maxTranscriptBytes caps the byte length of a returned transcript. AWS
// Transcribe streaming sessions are bounded to 4hr but per-message IM voice
// is ≤ 5min; 16KB is well above any plausible Mandarin/English transcription
// at that duration (≈ 200 chars/sec spoken × 5min × 4 byte/rune ≈ 240KB
// upper bound, real-world is sub-KB). Bound the field so a runaway server
// stream cannot fan out unbounded text into IM message buffers. R247-SEC-18.
const maxTranscriptBytes = 16 * 1024

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
	// R186-RELY-H1 / R178-T: the sender goroutine uses stream.Writer; if
	// collectTranscripts returns early (Reader.Err, partial result, ctx
	// cancellation) the deferred stream.Close would race with an in-flight
	// Writer.Send. Gate the Close on the sender's completion instead.
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
				// R242-GO-15: break → fall-through to Writer.Close()
				// below is intentional. Even on send error we still
				// need to close the writer so the AWS SDK signals EOF
				// to the service and collectTranscripts' Reader.Err
				// surfaces a server-side error (otherwise the events
				// channel hangs until ctx cancellation). Do NOT swap
				// `break` for `return` — Writer.Close must run on
				// every exit path of this goroutine.
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

	return pumpPCMToTranscribe(ctx, pcm, resp.GetStream())
}

// pumpPCMToTranscribe streams ffmpeg PCM output into the Transcribe event
// stream and collects the resulting transcript. Split out of streamFromFFmpeg
// so the sender goroutine + ffmpeg-error propagation logic is unit-testable
// with a mocked event stream (the SDK output's eventStream field is unexported,
// so the seam has to live below resp.GetStream()).
func pumpPCMToTranscribe(ctx context.Context, pcm *pcmStream, stream *transcribestreaming.StartStreamTranscriptionEventStream) (string, error) {
	// R186-RELY-H1 / R178-T: see streamFromBuffer; wait for sender goroutine
	// before stream.Close() to avoid use-after-close on Writer.Send.
	senderDone := make(chan struct{})
	defer func() {
		<-senderDone
		stream.Close()
	}()

	// pcm.Close() reaps ffmpeg and returns its non-zero exit error. The Close
	// runs inside the sender goroutine (its sole owner), so carry the result
	// back over a buffered (cap-1) channel: the send never blocks even if the
	// reader bails, and the receive below happens-after that send — so reading
	// the ffmpeg error is race-free without a second join. #1781: previously
	// the error was discarded (`defer pcm.Close()`), so a transcode failure
	// surfaced as ("", nil) — a silent success masking a conversion error.
	ffmpegErrCh := make(chan error, 1)

	// Read from ffmpeg stdout → send to Transcribe, concurrently with ffmpeg
	go func() {
		defer close(senderDone)
		defer func() { ffmpegErrCh <- pcm.Close() }()
		buf := make([]byte, 16*1024)
		for {
			n, readErr := pcm.Read(buf)
			if n > 0 {
				// The chunk copy is required because `buf` is reused across
				// iterations; AWS Go SDK v2 currently serializes AudioChunk
				// synchronously inside Send, but that behaviour is not part
				// of the public contract. Removing the copy to "optimize"
				// would introduce a race if the SDK ever adds async
				// buffering. R187-RELY-L1.
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
		return "", err
	}
	// #1781: a non-empty transcript means usable PCM still reached Transcribe
	// before ffmpeg died, so prefer the partial result over the error; only
	// surface the convert error when nothing was transcribed (otherwise we'd
	// discard a good result for a benign late ffmpeg hiccup).
	if transcript == "" {
		if ffmpegErr := <-ffmpegErrCh; ffmpegErr != nil {
			return "", fmt.Errorf("audio convert: %w", ffmpegErr)
		}
	}
	return transcript, nil
}

// collectTranscripts reads final transcript results from the stream.
//
// R247-SEC-18: AWS Transcribe responses flow into IM message paths and slog
// attributes. A crafted/exploited upstream could embed bidi (U+202A..E /
// U+2066..9), LS/PS (U+2028/9), or C0/C1 control runes that flip log
// rendering or split slog lines. Pipe the joined transcript through
// osutil.SanitizeForLog (same scrub used by cron sanitiseRunResult) before
// returning. SanitizeForLog preserves CJK / emoji / valid printable
// codepoints — only the documented injection-class runes become "_".
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
		// Normalize: strip spaces around each language code and drop empty
		// segments so leading/trailing commas (",en-US" / "zh-CN,") or doubles
		// (",,") do not leave PreferredLanguage = "" and trip AWS API 400.
		// R185-REL-M1.
		raw := strings.Split(s.cfg.LanguageCode, ",")
		parts := raw[:0]
		for _, p := range raw {
			if t := strings.TrimSpace(p); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			// Degrade gracefully: multi-lang config that resolved to zero
			// entries falls back to single-LanguageCode with the raw string
			// (AWS will return a clearer "invalid LanguageCode" if still bad).
			input.IdentifyMultipleLanguages = false
			input.LanguageCode = types.LanguageCode(s.cfg.LanguageCode)
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
	// RIFF is a generic container header shared by WAV, AVI, WEBP, AIFF-RIFF
	// and others. Only the WAVE subtype (bytes 8..12) is an audio stream we
	// want to admit as audio/wav — returning audio/wav for a WEBP image or
	// AVI video would have that asset mislabelled downstream (Whisper/
	// ffmpeg spends cycles on an audio decode that will never produce
	// speech). Require the 4-byte subtype explicitly.
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return "audio/wav"
	}
	return ""
}
