package dispatch

// Tests for passthrough-mode banner and AskUserQuestion bugs:
//   #1957 – merged-follower onEvent(result) must not create thinking banner
//   #1958 – onEvent(assistant) with AskQuestion must fire askQuestionFired
//   #1959 – image send must be suppressed when askQuestionFired is true

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ---------------------------------------------------------------------------
// fakeInterimPlatform implements platform.Platform with
// SupportsInterimMessages() == true (Feishu-like).
// ---------------------------------------------------------------------------
type fakeInterimPlatform struct {
	mu      sync.Mutex
	replies []platform.OutgoingMessage
	edits   []struct{ msgID, text string }
}

func (f *fakeInterimPlatform) Name() string                                               { return "fake-interim" }
func (f *fakeInterimPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (f *fakeInterimPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, msg)
	return "msg-1", nil
}
func (f *fakeInterimPlatform) EditMessage(_ context.Context, msgID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, struct{ msgID, text string }{msgID, text})
	return nil
}
func (f *fakeInterimPlatform) MaxReplyLength() int           { return 4000 }
func (f *fakeInterimPlatform) SupportsInterimMessages() bool { return true }
func (f *fakeInterimPlatform) replyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies)
}

// ---------------------------------------------------------------------------
// #1957: replyTracker.onEvent must not fire the banner when the event is a
// result event (ev.Type == "result", ev.Message == nil).
// Before the fix, result events caused formatEventLine to return "" which
// fell through to "💭 思考中..." → sent.Do fired → orphan banner on platforms
// that support interim edits, since the merged-follower path returned before
// any banner cleanup.
// ---------------------------------------------------------------------------
func TestReplyTracker_ResultEvent_NoBanner(t *testing.T) {
	t.Parallel()

	fp := &fakeInterimPlatform{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1")
	defer tracker.stop()

	// Deliver a result event — the type passthrough fan-out sends to follower slots.
	tracker.onEvent(cli.Event{Type: "result", Result: ""})

	// Give any async Reply goroutine time to run if the fix is absent.
	time.Sleep(60 * time.Millisecond)

	if n := fp.replyCount(); n != 0 {
		t.Errorf("#1957: result event fired %d Reply calls (orphan banner created); want 0", n)
	}
}

// TestReplyTracker_AssistantEvent_FiresBanner pins the positive case:
// an assistant event with thinking content MUST still create the banner.
func TestReplyTracker_AssistantEvent_FiresBanner(t *testing.T) {
	t.Parallel()

	fp := &fakeInterimPlatform{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1")
	defer tracker.stop()

	tracker.onEvent(cli.Event{
		Type: "assistant",
		Message: &cli.AssistantMessage{
			Content: []cli.ContentBlock{{Type: "thinking", Text: "reasoning…"}},
		},
	})

	// Wait up to 2 s for the async Reply goroutine to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fp.replyCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if fp.replyCount() == 0 {
		t.Error("assistant thinking event must fire banner Reply; positive case regressed by #1957 guard")
	}
}

// ---------------------------------------------------------------------------
// #1958: onEvent must set askQuestionFired when it receives an assistant
// event carrying AskQuestion.
// Before the fix, passthrough readLoop only delivered result events to onEvent,
// so AskQuestion (on assistant events) was never seen and askQuestionFired
// was never set → bailout text was not suppressed.
// This unit test verifies onEvent itself; the readLoop integration is in the
// cli package passthrough tests.
// ---------------------------------------------------------------------------
func TestReplyTracker_AskQuestion_SetsFlag(t *testing.T) {
	t.Parallel()

	fp := &fakeInterimPlatform{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1")
	defer tracker.stop()

	if tracker.askQuestionFired.Load() {
		t.Fatal("askQuestionFired should be false before any event")
	}

	// Deliver an assistant event carrying AskQuestion.
	tracker.onEvent(cli.Event{
		Type: "assistant",
		AskQuestion: &cli.AskQuestion{
			ToolUseID: "tu-1",
			Items: []cli.AskQuestionItem{{
				Question: "Which style?",
				Options:  []cli.AskQuestionOpt{{Label: "A"}, {Label: "B"}},
			}},
		},
	})

	if !tracker.askQuestionFired.Load() {
		t.Error("#1958: askQuestionFired not set after delivering AskQuestion event to onEvent")
	}
}

// ---------------------------------------------------------------------------
// #1959: image bubbles from the same reply text must be suppressed when
// askQuestionFired is true. Tests the Dispatcher end-to-end via a fake Send
// that (a) calls onEvent with an AskQuestion assistant event, and (b) returns
// a result text containing a real /tmp image path.
// ---------------------------------------------------------------------------
func TestDispatcher_AskQuestionFired_SuppressesImages(t *testing.T) {
	t.Parallel()

	// Create a real /tmp PNG so ExtractImagePaths passes its stat checks.
	imgFile := makeTmpPNG(t)
	defer os.Remove(imgFile)

	var (
		imageMu    sync.Mutex
		imagesSent int
	)

	// Use a fakePlatform that intercepts image Reply calls.
	fp := &fakePlatform{
		supportsInterim: false,
		replyMsgID:      "msg-1",
	}
	// Override Reply via embedding is not possible here since fakePlatform is
	// a struct. Instead wrap with a thin interceptor via the sendFn closure:
	// we inspect the Dispatcher's internal platform after the call by
	// supplying a platform that records image sends.
	fpImg := &imageCapturePlatform{
		fakePlatform: fp,
		onImage: func() {
			imageMu.Lock()
			imagesSent++
			imageMu.Unlock()
		},
	}

	sendFn := func(
		_ context.Context,
		_ string,
		_ *session.ManagedSession,
		_ string,
		_ []cli.ImageData,
		onEvent cli.EventCallback,
	) (*cli.SendResult, error) {
		// Fire AskQuestion via onEvent so askQuestionFired is set before
		// sendAndReply inspects the tracker.
		if onEvent != nil {
			onEvent(cli.Event{
				Type: "assistant",
				AskQuestion: &cli.AskQuestion{
					ToolUseID: "tu-99",
					Items: []cli.AskQuestionItem{{
						Question: "confirm?",
						Options:  []cli.AskQuestionOpt{{Label: "yes"}},
					}},
				},
			})
		}
		return &cli.SendResult{Text: "see " + imgFile}, nil
	}

	// newTestDispatcher wires a Router and Guard; override the platform map
	// manually so our image-capturing platform is used.
	d := newTestDispatcher(fp, sendFn)
	d.platforms["fake"] = fpImg

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d.sendAndReply(ctx,
		"fake:direct:chat1:general",
		"hello", nil,
		"general", session.AgentOpts{},
		platform.IncomingMessage{
			Platform: "fake", EventID: "e1",
			UserID: "u1", ChatID: "chat1", ChatType: "direct", Text: "hello",
		},
		slog.Default(),
		false,
	)

	imageMu.Lock()
	n := imagesSent
	imageMu.Unlock()

	if n != 0 {
		t.Errorf("#1959: expected 0 image sends when askQuestionFired=true, got %d", n)
	}
}

// imageCapturePlatform wraps fakePlatform to intercept image Reply calls.
type imageCapturePlatform struct {
	*fakePlatform
	onImage func()
}

func (p *imageCapturePlatform) Name() string { return "fake" }
func (p *imageCapturePlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	if len(msg.Images) > 0 && p.onImage != nil {
		p.onImage()
	}
	return p.fakePlatform.Reply(ctx, msg)
}

// makeTmpPNG writes a minimal valid 1×1 PNG to /tmp and returns the path.
// The PNG is small (<10 MB) and passes EvalSymlinks+Stat so ExtractImagePaths
// includes it.
func makeTmpPNG(t *testing.T) string {
	t.Helper()
	// Minimal 1×1 white PNG (RFC 2083-compliant, ~67 bytes).
	png1x1 := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
		0x00, 0x05, 0xFE, 0x02, 0xFE, 0xDC, 0xCC, 0x59,
		0xE7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	f, err := os.CreateTemp("", "naozhi-test-*.png")
	if err != nil {
		t.Fatalf("makeTmpPNG: %v", err)
	}
	if _, err := f.Write(png1x1); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("makeTmpPNG write: %v", err)
	}
	f.Close()
	return f.Name()
}
