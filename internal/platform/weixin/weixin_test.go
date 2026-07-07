package weixin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	if w.Name() != "weixin" {
		t.Errorf("Name() = %q, want %q", w.Name(), "weixin")
	}
	if w.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", w.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok", MaxReplyLen: 2000})
	if w.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", w.MaxReplyLength())
	}
}

func TestExtractText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  weixinMessage
		want string
	}{
		{
			name: "text message",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeText, TextItem: &textItem{Text: "hello"}},
				},
			},
			want: "hello",
		},
		{
			name: "no text item",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeImage},
				},
			},
			want: "",
		},
		{
			name: "empty item list",
			msg:  weixinMessage{},
			want: "",
		},
		{
			name: "nil text item",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeText, TextItem: nil},
				},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractText(tt.msg)
			if got != tt.want {
				t.Errorf("extractText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStartStop(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return empty response for getUpdates
		json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})

	handler := func(_ context.Context, _ platform.IncomingMessage) {}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Double start should fail
	if err := w.Start(handler); err == nil {
		t.Fatal("expected error on double Start()")
	}

	if err := w.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// TestStartStop_JoinsPollLoop regresses R202606j-CR-001+CR-004: Start must
// publish the lifecycle handles (cancel/handler) under startMu so a racing
// Stop snapshots a non-nil cancel, and Stop must join the pollLoop goroutine
// (w.pollWg.Wait) so no poll goroutine outlives Stop's return. We assert this
// by tracking pollLoop's entry/exit via a poll handler that bumps a counter:
// after Stop returns, the loop goroutine must have observed ctx cancellation
// and exited, which we verify by confirming the server stops being polled.
func TestStartStop_JoinsPollLoop(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		// Short long-poll so Stop's cancel is observed promptly.
		time.Sleep(20 * time.Millisecond)
		json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	handler := func(_ context.Context, _ platform.IncomingMessage) {}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Let the loop poll at least once so it is genuinely in-flight.
	deadline := time.After(2 * time.Second)
	for polls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for first poll")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Stop must not panic and must block until pollLoop has joined.
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	// After Stop returns, pollWg has drained — no further polls may occur.
	// Snapshot the count, wait past one full poll cycle, and assert it is
	// unchanged (the loop goroutine has exited rather than issuing a new poll).
	before := polls.Load()
	time.Sleep(150 * time.Millisecond)
	if after := polls.Load(); after != before {
		t.Errorf("pollLoop still polling after Stop: before=%d after=%d", before, after)
	}
}

// TestStop_BeforeStart verifies Stop is a safe no-op when Start never ran:
// the nil-cancel snapshot guard (mirroring slack) must not panic or block.
func TestStop_BeforeStart(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop() before Start should be a no-op, got: %v", err)
	}
}

func TestReply_NoContextToken(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	_, err := w.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "user123",
		Text:   "hello",
	})
	if err == nil {
		t.Fatal("expected error when no context_token cached")
	}
}

func TestReply_WithContextToken(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		var req sendMessageReq
		json.NewDecoder(r.Body).Decode(&req)

		if req.Msg.ToUserID != "user123" {
			http.Error(w, "bad to_user_id", 400)
			return
		}
		if req.Msg.ContextToken == "" {
			http.Error(w, "missing context_token", 400)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	// Pre-cache a context token
	w.contextTokens.Store("user123", &tokenEntry{token: "ctx-tok-abc", updatedNs: time.Now().UnixNano()})

	msgID, err := w.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "user123",
		Text:   "hi there",
	})
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if msgID == "" {
		t.Error("expected non-empty msgID")
	}
	if !called.Load() {
		t.Error("sendMessage was not called")
	}
}

func TestPollLoop_ReceivesMessages(t *testing.T) {
	t.Parallel()
	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			// First poll: return a message
			json.NewEncoder(w).Encode(getUpdatesResp{
				Ret: 0,
				Msgs: []weixinMessage{
					{
						MessageID:    42,
						FromUserID:   "alice",
						MessageType:  msgTypeUser,
						ContextToken: "ctx-1",
						ItemList: []messageItem{
							{Type: msgItemTypeText, TextItem: &textItem{Text: "hello bot"}},
						},
					},
				},
				GetUpdatesBuf: "cursor-1",
			})
		} else {
			// Subsequent polls: empty, slow to simulate long-poll
			time.Sleep(100 * time.Millisecond)
			json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, GetUpdatesBuf: "cursor-1"})
		}
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})

	var received atomic.Int32
	var receivedMsg platform.IncomingMessage
	var mu sync.Mutex

	handler := func(_ context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		receivedMsg = msg
		mu.Unlock()
		received.Add(1)
	}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer w.Stop()

	// Wait for message to be received
	deadline := time.After(3 * time.Second)
	for received.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for message")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	mu.Lock()
	msg := receivedMsg
	mu.Unlock()

	if msg.Platform != "weixin" {
		t.Errorf("Platform = %q, want %q", msg.Platform, "weixin")
	}
	if msg.UserID != "alice" {
		t.Errorf("UserID = %q, want %q", msg.UserID, "alice")
	}
	if msg.Text != "hello bot" {
		t.Errorf("Text = %q, want %q", msg.Text, "hello bot")
	}
	if msg.ChatType != "direct" {
		t.Errorf("ChatType = %q, want %q", msg.ChatType, "direct")
	}
	// #2116: EventID must be namespaced (platform:user:message_id), never a
	// bare integer, so it cannot collide with another platform/user's event in
	// the shared cross-platform Dedup.
	if want := "weixin:alice:42"; msg.EventID != want {
		t.Errorf("EventID = %q, want %q", msg.EventID, want)
	}

	// Verify context_token was cached
	ct, ok := w.contextTokens.Load("alice")
	if !ok {
		t.Fatalf("context_token not cached")
	}
	entry, isEntry := ct.(*tokenEntry)
	if !isEntry || entry.token != "ctx-1" {
		t.Errorf("context_token not cached, got %v", ct)
	}
}

// TestPollLoop_OversizedContextTokenObservable covers #2238: an oversized
// context_token (> cap) is dropped (so the message still flows), but the drop
// is now logged at WARN with the length (never the token) so the otherwise
// silent "this user's replies fail forever" condition is diagnosable.
func TestPollLoop_OversizedContextTokenObservable(t *testing.T) {
	const cap = 512
	oversized := strings.Repeat("A", cap+1)

	var logBuf strings.Builder
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncWriter{w: &logBuf, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pollCount.Add(1) == 1 {
			json.NewEncoder(w).Encode(getUpdatesResp{
				Ret: 0,
				Msgs: []weixinMessage{
					{
						MessageID:    7,
						FromUserID:   "bob",
						MessageType:  msgTypeUser,
						ContextToken: oversized,
						ItemList: []messageItem{
							{Type: msgItemTypeText, TextItem: &textItem{Text: "hi"}},
						},
					},
				},
				GetUpdatesBuf: "c1",
			})
			return
		}
		time.Sleep(100 * time.Millisecond)
		json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, GetUpdatesBuf: "c1"})
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	var received atomic.Int32
	if err := w.Start(func(_ context.Context, _ platform.IncomingMessage) { received.Add(1) }); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer w.Stop()

	deadline := time.After(3 * time.Second)
	for received.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for message")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Oversized token must NOT be cached.
	if _, ok := w.contextTokens.Load("bob"); ok {
		t.Error("oversized context_token should not be cached")
	}

	logMu.Lock()
	out := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(out, "context_token exceeds cap") {
		t.Errorf("expected WARN about oversized context_token, got log: %q", out)
	}
	if strings.Contains(out, oversized) {
		t.Error("oversized token value must never be logged")
	}
}

// TestPollLoop_SemaphoreFullDropSanitizesUser regresses R202606g-SEC-1: the
// semaphore-full drop WARN logs the attacker-influenced FromUserID, which a
// hostile iLink relay can stuff with C0/C1/bidi/newline bytes to poison the
// operator's structured logs. The drop-path log MUST route from through
// osutil.SanitizeForLog like every other from-bearing log in this file.
func TestPollLoop_SemaphoreFullDropSanitizesUser(t *testing.T) {
	var logBuf strings.Builder
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncWriter{w: &logBuf, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// from with embedded control bytes (NUL, BEL, ESC, newline). This is the
	// 21st message in the poll, so with 20 handler slots held by the earlier
	// blocking handlers it lands on the semaphore-full drop path.
	const poisonFrom = "evil\x00\x07\x1b\nuser"

	// Block the first weixinHookConcurrency handlers so the sem saturates.
	release := make(chan struct{})
	var startedHandlers atomic.Int32

	msgs := make([]weixinMessage, 0, weixinHookConcurrency+1)
	for i := 0; i < weixinHookConcurrency; i++ {
		msgs = append(msgs, weixinMessage{
			MessageID:   i + 1,
			FromUserID:  "filler-" + strconv.Itoa(i),
			MessageType: msgTypeUser,
			ItemList:    []messageItem{{Type: msgItemTypeText, TextItem: &textItem{Text: "hi"}}},
		})
	}
	// The overflow message carries the poison from.
	msgs = append(msgs, weixinMessage{
		MessageID:   weixinHookConcurrency + 1,
		FromUserID:  poisonFrom,
		MessageType: msgTypeUser,
		ItemList:    []messageItem{{Type: msgItemTypeText, TextItem: &textItem{Text: "overflow"}}},
	})

	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if pollCount.Add(1) == 1 {
			json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, Msgs: msgs, GetUpdatesBuf: "c1"})
			return
		}
		time.Sleep(100 * time.Millisecond)
		json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, GetUpdatesBuf: "c1"})
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	handler := func(_ context.Context, _ platform.IncomingMessage) {
		startedHandlers.Add(1)
		<-release // hold the semaphore slot until the test releases it
	}
	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		close(release)
		w.Stop()
	}()

	// Wait until all 20 slots are occupied so the 21st is forced onto the drop
	// path and its WARN is emitted.
	deadline := time.After(3 * time.Second)
	for startedHandlers.Load() < int32(weixinHookConcurrency) {
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d/%d handlers started", startedHandlers.Load(), weixinHookConcurrency)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Give the drop-path WARN a moment to flush.
	dropDeadline := time.After(2 * time.Second)
	for {
		logMu.Lock()
		out := logBuf.String()
		logMu.Unlock()
		if strings.Contains(out, "semaphore full") {
			break
		}
		select {
		case <-dropDeadline:
			t.Fatalf("timeout waiting for semaphore-full WARN; log: %q", out)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	logMu.Lock()
	out := logBuf.String()
	logMu.Unlock()

	// No raw control byte from the poison from may survive into the log line.
	for _, b := range []byte{0x00, 0x07, 0x1b, '\n'} {
		// '\n' is a legitimate record separator in the text handler output, so
		// only assert on the genuinely dangerous C0/C1/ESC bytes here.
		if b == '\n' {
			continue
		}
		if strings.IndexByte(out, b) >= 0 {
			t.Errorf("semaphore-full log still contains raw control byte 0x%02x: %q", b, out)
		}
	}
	// The raw poison user string must not appear verbatim (its control bytes
	// would have been replaced with '_' by SanitizeForLog).
	if strings.Contains(out, poisonFrom) {
		t.Errorf("raw unsanitized from leaked into log: %q", out)
	}
	// The sanitized form keeps the printable skeleton.
	if !strings.Contains(out, "evil") || !strings.Contains(out, "user") {
		t.Errorf("expected sanitized from skeleton in log, got: %q", out)
	}
}

// syncWriter serializes concurrent writes from the poll goroutine's slog calls.
type syncWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestPollLoop_FallbackMessageID covers #2117: when the iLink upstream omits
// message_id (decodes to 0), the adapter must still populate
// IncomingMessage.MessageID from a per-message distinguisher (Seq, else
// CreateTimeMs). Otherwise the dispatch-side fallback dedup key degenerates to
// from+minute and two distinct messages in the same wall-clock minute collide.
func TestPollLoop_FallbackMessageID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		msg       weixinMessage
		wantEvent string
		wantMsgID string
	}{
		{
			name: "seq when no message_id",
			msg: weixinMessage{
				MessageID:   0,
				Seq:         7,
				FromUserID:  "bob",
				MessageType: msgTypeUser,
				ItemList:    []messageItem{{Type: msgItemTypeText, TextItem: &textItem{Text: "hi"}}},
			},
			wantEvent: "",
			wantMsgID: "seq:7",
		},
		{
			name: "create_time_ms when no message_id or seq",
			msg: weixinMessage{
				MessageID:    0,
				Seq:          0,
				CreateTimeMs: 1700000000123,
				FromUserID:   "carol",
				MessageType:  msgTypeUser,
				ItemList:     []messageItem{{Type: msgItemTypeText, TextItem: &textItem{Text: "hi"}}},
			},
			wantEvent: "",
			wantMsgID: "ts:1700000000123",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pollCount := atomic.Int32{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if pollCount.Add(1) == 1 {
					json.NewEncoder(w).Encode(getUpdatesResp{
						Ret:           0,
						Msgs:          []weixinMessage{tc.msg},
						GetUpdatesBuf: "cursor-1",
					})
					return
				}
				time.Sleep(100 * time.Millisecond)
				json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, GetUpdatesBuf: "cursor-1"})
			}))
			defer srv.Close()

			w := New(Config{Token: "tok", BaseURL: srv.URL})
			var received atomic.Int32
			var receivedMsg platform.IncomingMessage
			var mu sync.Mutex
			handler := func(_ context.Context, msg platform.IncomingMessage) {
				mu.Lock()
				receivedMsg = msg
				mu.Unlock()
				received.Add(1)
			}
			if err := w.Start(handler); err != nil {
				t.Fatalf("Start() error: %v", err)
			}
			defer w.Stop()

			deadline := time.After(3 * time.Second)
			for received.Load() == 0 {
				select {
				case <-deadline:
					t.Fatal("timeout waiting for message")
				default:
					time.Sleep(20 * time.Millisecond)
				}
			}
			mu.Lock()
			msg := receivedMsg
			mu.Unlock()

			if msg.EventID != tc.wantEvent {
				t.Errorf("EventID = %q, want %q", msg.EventID, tc.wantEvent)
			}
			if msg.MessageID != tc.wantMsgID {
				t.Errorf("MessageID = %q, want %q", msg.MessageID, tc.wantMsgID)
			}
		})
	}
}

func TestPollLoop_SkipsBotMessages(t *testing.T) {
	t.Parallel()
	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(getUpdatesResp{
				Ret: 0,
				Msgs: []weixinMessage{
					{
						MessageID:   1,
						FromUserID:  "bot",
						MessageType: msgTypeBOT, // bot message, should be skipped
						ItemList: []messageItem{
							{Type: msgItemTypeText, TextItem: &textItem{Text: "bot reply"}},
						},
					},
				},
			})
		} else {
			time.Sleep(100 * time.Millisecond)
			json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
		}
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	var received atomic.Int32
	handler := func(_ context.Context, _ platform.IncomingMessage) {
		received.Add(1)
	}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer w.Stop()

	// Wait a bit — no message should be received
	time.Sleep(500 * time.Millisecond)
	if received.Load() != 0 {
		t.Errorf("received %d messages, expected 0 (bot messages should be skipped)", received.Load())
	}
}

func TestAPIClient_GetUpdates(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/getupdates" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("AuthorizationType") != "ilink_bot_token" {
			http.Error(w, "bad auth type", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(getUpdatesResp{
			Ret:           0,
			GetUpdatesBuf: "new-cursor",
		})
	}))
	defer srv.Close()

	api := newAPIClient(srv.URL, "test-token")
	resp, err := api.getUpdates(context.Background(), "")
	if err != nil {
		t.Fatalf("getUpdates error: %v", err)
	}
	if resp.Ret != 0 {
		t.Errorf("ret = %d, want 0", resp.Ret)
	}
	if resp.GetUpdatesBuf != "new-cursor" {
		t.Errorf("cursor = %q, want %q", resp.GetUpdatesBuf, "new-cursor")
	}
}

func TestAPIClient_SendMessage(t *testing.T) {
	t.Parallel()
	var received sendMessageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/sendmessage" {
			http.Error(w, "not found", 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	api := newAPIClient(srv.URL, "test-token")
	err := api.sendMessage(context.Background(), "user1", "hello", "ctx-tok")
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if received.Msg.ToUserID != "user1" {
		t.Errorf("to_user_id = %q, want %q", received.Msg.ToUserID, "user1")
	}
	if received.Msg.ContextToken != "ctx-tok" {
		t.Errorf("context_token = %q, want %q", received.Msg.ContextToken, "ctx-tok")
	}
	if len(received.Msg.ItemList) != 1 || received.Msg.ItemList[0].TextItem.Text != "hello" {
		t.Errorf("unexpected item_list: %+v", received.Msg.ItemList)
	}
}

// TestAPIClient_SendMessage_SanitizesErrMsg ensures the untrusted relay-supplied
// errmsg is stripped of control characters before being embedded in the error
// returned to callers/logs. R100110-LEAK-12.
func TestAPIClient_SendMessage_SanitizesErrMsg(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// errmsg carries injected control chars (CR, LF, NUL); marshal so the
		// raw bytes reach sendMessage's error path unsanitized.
		body, _ := json.Marshal(map[string]any{
			"ret":     1,
			"errcode": 42,
			"errmsg":  "bad\r\ninjected\x00tail",
		})
		w.Write(body)
	}))
	defer srv.Close()

	api := newAPIClient(srv.URL, "test-token")
	err := api.sendMessage(context.Background(), "user1", "hello", "ctx-tok")
	if err == nil {
		t.Fatal("expected error for ret != 0, got nil")
	}
	msg := err.Error()
	for _, c := range msg {
		if c < 0x20 || c == 0x7f {
			t.Fatalf("error message contains unsanitized control char %#x: %q", c, msg)
		}
	}
	if !strings.Contains(msg, "ret=1") || !strings.Contains(msg, "errcode=42") {
		t.Errorf("error message missing ret/errcode context: %q", msg)
	}
}

func TestEditMessage_Noop(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	err := w.EditMessage(context.Background(), "any-id", "new text")
	if err != nil {
		t.Errorf("EditMessage should be no-op, got error: %v", err)
	}
}

func TestRandomWechatUIN(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		v := randomWechatUIN()
		if v == "" {
			t.Fatal("empty UIN")
		}
		if seen[v] {
			t.Errorf("duplicate UIN: %s", v)
		}
		seen[v] = true
	}
}

// TestValidateBaseURLScheme covers the R235-SEC-1 / R214-SEC-1 (#417)
// transport gate: https is allowed, loopback http is allowed (dev mocks),
// and non-loopback http is rejected because the no-HMAC long-poll body has
// no authenticity anchor other than TLS.
func TestValidateBaseURLScheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty defaults to https", "", false},
		{"https ok", "https://ilinkai.weixin.qq.com", false},
		{"http localhost ok", "http://localhost:8080", false},
		{"http loopback ip ok", "http://127.0.0.1:9000", false},
		{"http ipv6 loopback ok", "http://[::1]:9000", false},
		{"http public host rejected", "http://evil.example.com", true},
		{"http public ip rejected", "http://203.0.113.5", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBaseURLScheme(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBaseURLScheme(%q) err=%v, wantErr=%v", tc.url, err, tc.wantErr)
			}
		})
	}
}

// TestBaseURLIsTLS locks in the R214-SEC-1 (#417) startup-posture classifier:
// only an approved loopback http:// relay reports non-TLS; https and the
// empty default report TLS.
func TestBaseURLIsTLS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		url    string
		wantTL bool
	}{
		{"empty default https", "", true},
		{"https", "https://ilinkai.weixin.qq.com", true},
		{"loopback http", "http://127.0.0.1:9000", false},
		{"localhost http", "http://localhost:8080", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := New(Config{Token: "tok", BaseURL: tc.url})
			if got := w.baseURLIsTLS(); got != tc.wantTL {
				t.Errorf("baseURLIsTLS(%q) = %v, want %v", tc.url, got, tc.wantTL)
			}
		})
	}
}
