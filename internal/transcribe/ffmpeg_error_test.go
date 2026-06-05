package transcribe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
)

// fakeAudioWriter is a no-op AudioStreamWriter. The SDK contract requires Close
// to be safe under concurrent/repeated calls; a no-op trivially satisfies it.
type fakeAudioWriter struct{}

func (*fakeAudioWriter) Send(context.Context, types.AudioStream) error { return nil }
func (*fakeAudioWriter) Close() error                                  { return nil }
func (*fakeAudioWriter) Err() error                                    { return nil }

// fakeTranscriptReader replays a fixed set of final transcript results and then
// closes its events channel, exactly like the AWS reader once the service ends
// the stream cleanly (Err() == nil).
type fakeTranscriptReader struct {
	events chan types.TranscriptResultStream
}

func newFakeTranscriptReader(transcripts []string) *fakeTranscriptReader {
	ch := make(chan types.TranscriptResultStream, len(transcripts))
	for _, t := range transcripts {
		tr := t
		ch <- &types.TranscriptResultStreamMemberTranscriptEvent{
			Value: types.TranscriptEvent{
				Transcript: &types.Transcript{
					Results: []types.Result{{
						IsPartial:    false,
						Alternatives: []types.Alternative{{Transcript: &tr}},
					}},
				},
			},
		}
	}
	close(ch)
	return &fakeTranscriptReader{events: ch}
}

func (r *fakeTranscriptReader) Events() <-chan types.TranscriptResultStream { return r.events }
func (r *fakeTranscriptReader) Close() error                                { return nil }
func (r *fakeTranscriptReader) Err() error                                  { return nil }

func newMockStream(transcripts []string) *transcribestreaming.StartStreamTranscriptionEventStream {
	return transcribestreaming.NewStartStreamTranscriptionEventStream(
		func(es *transcribestreaming.StartStreamTranscriptionEventStream) {
			es.Writer = &fakeAudioWriter{}
			es.Reader = newFakeTranscriptReader(transcripts)
		},
	)
}

// fakeFFmpeg writes a tiny shell script that prints `stdout` to stdout, `stderr`
// to stderr, then exits with `code`, and points NAOZHI_FFMPEG_PATH at it for the
// duration of the test. This lets startPCMStream spawn a real child process
// whose exit status pcm.Close()/Wait() observes — exercising the #1781 path
// (ffmpeg non-zero exit) without depending on a host ffmpeg or crafted audio.
func fakeFFmpeg(t *testing.T, stdout, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-ffmpeg shell script is POSIX-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-ffmpeg.sh")
	// Drain stdin so the bytes.Reader feeding pipe:0 does not block on a full
	// pipe (real ffmpeg consumes its input); ignore the data, then emit the
	// scripted output and exit code.
	body := "#!/bin/sh\ncat >/dev/null 2>&1\n"
	if stdout != "" {
		body += "printf '%s' " + shQuote(stdout) + "\n"
	}
	if stderr != "" {
		body += "printf '%s' " + shQuote(stderr) + " 1>&2\n"
	}
	body += "exit " + itoa(code) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv(ffmpegPathEnv, script)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestStreamFromFFmpeg_ConvertErrorSurfaces pins #1781: when ffmpeg exits
// non-zero and Transcribe yields no text, Transcribe must return a non-nil
// "audio convert" error rather than the silent ("", nil) success the discarded
// pcm.Close() used to produce.
func TestStreamFromFFmpeg_ConvertErrorSurfaces(t *testing.T) {
	fakeFFmpeg(t, "", "Invalid data found when processing input", 1)

	pcm, err := startPCMStream(context.Background(), []byte("not-real-audio"))
	if err != nil {
		t.Fatalf("startPCMStream: %v", err)
	}

	got, err := pumpPCMToTranscribe(context.Background(), pcm, newMockStream(nil))
	if err == nil {
		t.Fatalf("expected non-nil error for failed ffmpeg convert, got (%q, nil) — "+
			"the ffmpeg exit error must not be discarded (#1781)", got)
	}
	if got != "" {
		t.Errorf("transcript = %q, want empty on convert failure", got)
	}
	if !strings.Contains(err.Error(), "audio convert") {
		t.Errorf("error = %v, want it wrapped with %q", err, "audio convert")
	}
	// The underlying ffmpeg exit error must be preserved in the chain.
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) {
		t.Errorf("error = %v, want an *exec.ExitError in the chain (%%w wrapping)", err)
	}
}

// TestStreamFromFFmpeg_SuccessNoError is the negative control: ffmpeg exits 0
// and Transcribe returns text, so Transcribe must return that text with no
// error. Guards against the fix over-reporting errors on the happy path.
func TestStreamFromFFmpeg_SuccessNoError(t *testing.T) {
	// PCM payload is irrelevant; the mock reader supplies the transcript.
	fakeFFmpeg(t, "fake-pcm-bytes", "", 0)

	pcm, err := startPCMStream(context.Background(), []byte("audio"))
	if err != nil {
		t.Fatalf("startPCMStream: %v", err)
	}

	got, err := pumpPCMToTranscribe(context.Background(), pcm, newMockStream([]string{"你好", "世界"}))
	if err != nil {
		t.Fatalf("unexpected error on clean convert: %v", err)
	}
	if got != "你好 世界" {
		t.Errorf("transcript = %q, want %q", got, "你好 世界")
	}
}

// TestStreamFromFFmpeg_PartialResultPreferred pins the deliberate policy choice:
// if ffmpeg exits non-zero but usable PCM already produced a transcript, prefer
// the partial result over the convert error (a benign late ffmpeg hiccup must
// not discard a good transcription).
func TestStreamFromFFmpeg_PartialResultPreferred(t *testing.T) {
	fakeFFmpeg(t, "some-pcm", "late failure", 1)

	pcm, err := startPCMStream(context.Background(), []byte("audio"))
	if err != nil {
		t.Fatalf("startPCMStream: %v", err)
	}

	got, err := pumpPCMToTranscribe(context.Background(), pcm, newMockStream([]string{"partial"}))
	if err != nil {
		t.Fatalf("expected partial transcript preferred over convert error, got err: %v", err)
	}
	if got != "partial" {
		t.Errorf("transcript = %q, want %q", got, "partial")
	}
}

// TestCollectTranscripts_ReaderErr keeps the reader-error path covered: a reader
// that reports Err() must surface a "stream read" error regardless of ffmpeg.
func TestCollectTranscripts_ReaderErr(t *testing.T) {
	stream := transcribestreaming.NewStartStreamTranscriptionEventStream(
		func(es *transcribestreaming.StartStreamTranscriptionEventStream) {
			es.Writer = &fakeAudioWriter{}
			es.Reader = &errReader{err: errors.New("boom")}
		},
	)
	if _, err := collectTranscripts(stream); err == nil || !strings.Contains(err.Error(), "stream read") {
		t.Fatalf("collectTranscripts err = %v, want wrapped %q", err, "stream read")
	}
}

type errReader struct {
	err error
}

func (r *errReader) Events() <-chan types.TranscriptResultStream {
	ch := make(chan types.TranscriptResultStream)
	close(ch)
	return ch
}
func (r *errReader) Close() error { return nil }
func (r *errReader) Err() error   { return r.err }
