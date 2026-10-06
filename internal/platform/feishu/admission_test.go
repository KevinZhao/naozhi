package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/naozhi/naozhi/internal/platform"
)

// mediaStub serves the Feishu endpoints the inbound media paths touch and
// counts what each one was asked for.
type mediaStub struct {
	downloads atomic.Int64
	replies   atomic.Int64
}

func (s *mediaStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/resources/"):
		s.downloads.Add(1)
		if r.URL.Query().Get("type") == "audio" {
			_, _ = w.Write([]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00voice"))
			return
		}
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"))
	case r.URL.Path == "/open-apis/im/v1/messages":
		s.replies.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_reply"}}`))
	default:
		http.NotFound(w, r)
	}
}

type countingTranscriber struct{ calls atomic.Int64 }

func (c *countingTranscriber) Transcribe(context.Context, []byte, string) (string, error) {
	c.calls.Add(1)
	return "transcribed", nil
}

// handled collects what reached the MessageHandler.
type handled struct {
	mu   sync.Mutex
	msgs []platform.IncomingMessage
}

func (h *handled) handler(_ context.Context, msg platform.IncomingMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, msg)
}

func (h *handled) all() []platform.IncomingMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]platform.IncomingMessage(nil), h.msgs...)
}

func newMediaFeishu(t *testing.T, cfg Config, admit platform.AdmitFunc) (*Feishu, *mediaStub, *countingTranscriber) {
	t.Helper()
	stub := &mediaStub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	tr := &countingTranscriber{}
	f := New(cfg, tr)
	t.Cleanup(func() { _ = f.Stop() })
	f.baseURL = srv.URL
	f.accessToken = "t_cached"
	f.tokenExpiry = time.Now().Add(time.Hour)
	if admit != nil {
		f.SetAdmission(admit)
	}
	return f, stub, tr
}

// refuseRecording refuses every sender and records whom it was asked about.
func refuseRecording(asked *[]string, mu *sync.Mutex) platform.AdmitFunc {
	return func(_ context.Context, msg platform.IncomingMessage) bool {
		mu.Lock()
		defer mu.Unlock()
		*asked = append(*asked, msg.UserID)
		return false
	}
}

var mediaMsg = platform.IncomingMessage{Platform: "feishu", UserID: "ou_eve", ChatID: "oc_1", ChatType: "direct"}

// A refused voice sender costs nothing: no download, no Transcribe call, and
// no download/transcribe failure reply that would confirm the bot is online.
func TestHandleAudio_RefusedSenderCostsNothing(t *testing.T) {
	t.Parallel()
	var asked []string
	var mu sync.Mutex
	f, stub, tr := newMediaFeishu(t, Config{}, refuseRecording(&asked, &mu))
	var h handled

	f.handleAudio(context.Background(), h.handler, mediaMsg, "om_1", "file_1")

	if n := stub.downloads.Load(); n != 0 {
		t.Errorf("refused sender caused %d downloads", n)
	}
	if n := tr.calls.Load(); n != 0 {
		t.Errorf("refused sender caused %d Transcribe calls", n)
	}
	if n := stub.replies.Load(); n != 0 {
		t.Errorf("refused sender got %d replies from the adapter", n)
	}
	if len(h.all()) != 0 {
		t.Error("refused voice message reached the handler")
	}
	if len(asked) != 1 || asked[0] != "ou_eve" {
		t.Errorf("admission asked about %q, want [ou_eve]", asked)
	}
}

func TestHandleAudio_AdmittedSenderIsTranscribed(t *testing.T) {
	t.Parallel()
	for name, admit := range map[string]platform.AdmitFunc{
		"admits":  func(context.Context, platform.IncomingMessage) bool { return true },
		"not set": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f, stub, tr := newMediaFeishu(t, Config{}, admit)
			var h handled
			f.handleAudio(context.Background(), h.handler, mediaMsg, "om_1", "file_1")
			if stub.downloads.Load() != 1 || tr.calls.Load() != 1 {
				t.Errorf("downloads=%d transcribes=%d, want 1/1", stub.downloads.Load(), tr.calls.Load())
			}
			if got := h.all(); len(got) != 1 || got[0].Text != "transcribed" {
				t.Errorf("handler got %+v, want the transcribed text", got)
			}
		})
	}
}

func TestHandleImage_Admission(t *testing.T) {
	t.Parallel()
	var asked []string
	var mu sync.Mutex
	f, stub, _ := newMediaFeishu(t, Config{}, refuseRecording(&asked, &mu))
	var h handled
	f.handleImage(context.Background(), h.handler, mediaMsg, "om_1", "img_1")
	if n := stub.downloads.Load(); n != 0 || len(h.all()) != 0 {
		t.Errorf("refused image: downloads=%d handled=%d, want 0/0", n, len(h.all()))
	}

	f.SetAdmission(func(context.Context, platform.IncomingMessage) bool { return true })
	f.handleImage(context.Background(), h.handler, mediaMsg, "om_1", "img_1")
	if got := h.all(); stub.downloads.Load() != 1 || len(got) != 1 || len(got[0].Images) != 1 {
		t.Errorf("admitted image: downloads=%d handled=%+v, want one message with the image", stub.downloads.Load(), got)
	}
}

func buildV2MediaBody(eventID, msgType, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_id": eventID, "event_type": "im.message.receive_v1", "token": "test_token"},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_eve"}},
			"message": map[string]any{
				"message_id": "om_" + eventID, "chat_id": "oc_1", "chat_type": "p2p",
				"message_type": msgType, "content": content,
			},
		},
	})
	return b
}

// The webhook transport routes image and voice messages through the same
// admission check before it fetches anything.
func TestWebhook_MediaAdmission(t *testing.T) {
	t.Parallel()
	var asked []string
	var mu sync.Mutex
	f, stub, tr := newMediaFeishu(t, Config{ConnectionMode: "webhook", VerificationToken: "test_token"},
		refuseRecording(&asked, &mu))
	var h handled
	mux := http.NewServeMux()
	f.registerWebhook(mux, h.handler)

	for _, body := range [][]byte{
		buildV2MediaBody("ev_img", "image", `{"image_key":"img_1"}`),
		buildV2MediaBody("ev_audio", "audio", `{"file_key":"file_1"}`),
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(body))
		if w.Code != http.StatusOK {
			t.Fatalf("webhook status = %d, want 200", w.Code)
		}
	}
	f.dispatch.Wait()

	if stub.downloads.Load() != 0 || tr.calls.Load() != 0 || stub.replies.Load() != 0 || len(h.all()) != 0 {
		t.Errorf("refused media: downloads=%d transcribes=%d replies=%d handled=%d, want all 0",
			stub.downloads.Load(), tr.calls.Load(), stub.replies.Load(), len(h.all()))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Errorf("admission asked %d times, want once per media message", len(asked))
	}
}

func sdkMediaEvent(eventID, msgType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: eventID}},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strPtr("ou_eve")}},
			Message: &larkim.EventMessage{
				MessageId: strPtr("om_" + eventID), ChatId: strPtr("oc_1"), ChatType: strPtr("p2p"),
				MessageType: strPtr(msgType), Content: strPtr(content),
			},
		},
	}
}

// The websocket transport routes image and voice messages through the same
// admission check before it fetches anything.
func TestWebSocket_MediaAdmission(t *testing.T) {
	t.Parallel()
	var asked []string
	var mu sync.Mutex
	f, stub, tr := newMediaFeishu(t, Config{}, refuseRecording(&asked, &mu))
	var h handled

	for _, ev := range []*larkim.P2MessageReceiveV1{
		sdkMediaEvent("ev_img", "image", `{"image_key":"img_1"}`),
		sdkMediaEvent("ev_audio", "audio", `{"file_key":"file_1"}`),
	} {
		pe, ok := f.parseSDKEvent(ev)
		if !ok || pe.MediaType == "" {
			t.Fatalf("parseSDKEvent(%s) = %+v, %v; want a media event", *ev.Event.Message.MessageType, pe, ok)
		}
		f.routeParsed(context.Background(), h.handler, pe)
	}
	f.dispatch.Wait()

	if stub.downloads.Load() != 0 || tr.calls.Load() != 0 || stub.replies.Load() != 0 || len(h.all()) != 0 {
		t.Errorf("refused media: downloads=%d transcribes=%d replies=%d handled=%d, want all 0",
			stub.downloads.Load(), tr.calls.Load(), stub.replies.Load(), len(h.all()))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Errorf("admission asked %d times, want once per media message", len(asked))
	}
}
