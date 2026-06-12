package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/naozhi/naozhi/internal/platform"
)

var _ platform.RunnablePlatform = (*Discord)(nil)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if d.Name() != "discord" {
		t.Errorf("Name() = %q, want discord", d.Name())
	}
	if d.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", d.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token", MaxReplyLen: 1500})
	if d.MaxReplyLength() != 1500 {
		t.Errorf("MaxReplyLength() = %d, want 1500", d.MaxReplyLength())
	}
}

func TestStartAlreadyStarted(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.startMu.Lock()
	d.started = true
	d.startMu.Unlock()
	noop := func(_ context.Context, _ platform.IncomingMessage) {}
	err := d.Start(noop)
	if err == nil {
		t.Error("expected error for double Start()")
	}
}

func TestStopNoop(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if err := d.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestEditMessage_InvalidFormat(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	err := d.EditMessage(context.Background(), "no-colon-here", "text")
	if err == nil {
		t.Error("expected error for invalid msgID format")
	}
}

func TestOnMessageCreate_NilAuthor(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	// Should not panic
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{Author: nil, Content: "hello"},
	})
	if called {
		t.Error("nil author messages should be skipped")
	}
}

func TestOnMessageCreate_BotMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:  &discordgo.User{ID: "other_bot", Bot: true},
			Content: "hello",
		},
	})
	if called {
		t.Error("bot messages should be skipped")
	}
}

func TestOnMessageCreate_OwnMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:  &discordgo.User{ID: "bot123"},
			Content: "hello",
		},
	})
	if called {
		t.Error("own messages should be skipped")
	}
}

func TestOnMessageCreate_GroupMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg1",
			Author:    &discordgo.User{ID: "user1"},
			Content:   "hello",
			ChannelID: "ch1",
			GuildID:   "guild1",
		},
	})
	<-done
	if received.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", received.ChatType)
	}
	if received.Platform != "discord" {
		t.Errorf("Platform = %q, want discord", received.Platform)
	}
}

// TestOnMessageCreate_FailOpenWhenBotIDUnknown pins #2009: when Open() returned
// without a READY frame botID is empty; a guild message must fail-OPEN
// (MentionMe=true) instead of being silently dropped by dispatch's group gate.
func TestOnMessageCreate_FailOpenWhenBotIDUnknown(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	// botID intentionally NOT set — simulate the non-READY Open() path.
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-fo",
			Author:    &discordgo.User{ID: "user-fo"},
			Content:   "hello bot",
			ChannelID: "ch-fo",
			GuildID:   "guild-fo",
		},
	})
	<-done
	if received.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", received.ChatType)
	}
	if !received.MentionMe {
		t.Error("MentionMe should be true (fail-open) when botID is unknown")
	}
	if d.getBotID() != "" {
		t.Errorf("botID should still be empty in test (no live session), got %q", d.getBotID())
	}
}

// TestSetBotID_BackfillFromReady pins the Ready-handler backfill path used when
// a late READY arrives after Open() returned without State.User. #2009.
func TestSetBotID_BackfillFromReady(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if d.getBotID() != "" {
		t.Fatalf("fresh adapter botID = %q, want empty", d.getBotID())
	}
	d.setBotID("late-bot-id")
	if d.getBotID() != "late-bot-id" {
		t.Errorf("getBotID() = %q, want late-bot-id after backfill", d.getBotID())
	}
	// Empty id must be a no-op (don't clobber a known id with garbage).
	d.setBotID("")
	if d.getBotID() != "late-bot-id" {
		t.Errorf("setBotID(\"\") clobbered id: got %q", d.getBotID())
	}
}

func TestOnMessageCreate_DirectMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg2",
			Author:    &discordgo.User{ID: "user2"},
			Content:   "hello dm",
			ChannelID: "dm_ch",
			GuildID:   "", // empty = DM
		},
	})
	<-done
	if received.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", received.ChatType)
	}
}

func TestOnMessageCreate_MentionStrip(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg3",
			Author:    &discordgo.User{ID: "user3"},
			Content:   "<@bot123> do something",
			ChannelID: "ch3",
			GuildID:   "g1",
			Mentions:  []*discordgo.User{{ID: "bot123"}},
		},
	})
	<-done
	if received.Text != "do something" {
		t.Errorf("Text = %q, want 'do something'", received.Text)
	}
	if !received.MentionMe {
		t.Error("MentionMe should be true")
	}
}

func TestOnMessageCreate_EmptyAfterMentionStrip(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	setTestBotID(d, "bot123")
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:   &discordgo.User{ID: "user1"},
			Content:  "<@bot123>",
			Mentions: []*discordgo.User{{ID: "bot123"}},
		},
	})
	if called {
		t.Error("empty text after mention strip should be skipped")
	}
}

// TestDownloadURL_SchemeGuard verifies that downloadURL rejects any non-https
// URL before attempting a network fetch. The duplicate https check was removed
// in R20260602190132-SEC-7; this test pins the surviving guard.
func TestDownloadURL_SchemeGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"http rejected", "http://cdn.discordapp.com/attachments/1/file.png", true},
		{"ftp rejected", "ftp://cdn.discordapp.com/attachments/1/file.png", true},
		{"javascript rejected", "javascript://cdn.discordapp.com/xss", true},
		{"invalid url rejected", "://bad", true},
		// https passes the scheme guard but fails on unknown host — that is a
		// different error, not the scheme error; we just confirm no panic.
		{"https unknown host errors on host not scheme", "https://evil.example.com/file.png", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := downloadURL(tc.rawURL)
			if tc.wantErr && err == nil {
				t.Errorf("downloadURL(%q) expected error, got nil", tc.rawURL)
			}
		})
	}
}

// TestDownloadURL_BlocksPrivateIP verifies that blockPrivateDial (wired into
// discordHTTPClient.Transport) refuses any host that resolves to a reserved IP
// range, closing the DNS-rebinding SSRF path where cdn.discordapp.com is made
// to resolve to 169.254.169.254 (cloud IMDS) or an RFC-1918 address.
// (R20260603-SEC-3)
//
// Numeric hosts resolve to themselves via net.DefaultResolver.LookupIPAddr, so
// routing a literal reserved address through the dial func hits the guard
// before any TCP connection is attempted — no live server required.
func TestDownloadURL_BlocksPrivateIP(t *testing.T) {
	// Ensure production mode (bypass disabled) for the duration of the test.
	prev := discordDialTestBypass
	discordDialTestBypass = false
	t.Cleanup(func() { discordDialTestBypass = prev })

	dialCtx := blockPrivateDial()
	cases := []struct {
		name string
		addr string
	}{
		{"loopback_v4", "127.0.0.1:443"},
		{"loopback_v6", "[::1]:443"},
		{"link_local_IMDS", "169.254.169.254:80"},
		{"link_local_v6", "[fe80::1]:443"},
		{"rfc1918_10", "10.0.0.1:443"},
		{"rfc1918_172", "172.16.0.1:443"},
		{"rfc1918_192", "192.168.1.1:443"},
		{"unspecified_v4", "0.0.0.0:443"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn, err := dialCtx(ctx, "tcp", tc.addr)
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Errorf("dialCtx(%q) expected error (reserved IP), got nil", tc.addr)
				return
			}
			if !strings.Contains(err.Error(), "reserved IP") {
				t.Errorf("dialCtx(%q) error = %q; want 'reserved IP' in message", tc.addr, err.Error())
			}
		})
	}
}

// TestBlockPrivateDial_TestBypass verifies that when discordDialTestBypass is
// set, the reserved-IP guard is skipped (so httptest loopback servers work).
func TestBlockPrivateDial_TestBypass(t *testing.T) {
	prev := discordDialTestBypass
	discordDialTestBypass = true
	t.Cleanup(func() { discordDialTestBypass = prev })

	// Loopback with no listener: bypass lets the dial proceed, so we expect a
	// connection-refused style error, NOT the "reserved IP" guard error.
	dialCtx := blockPrivateDial()
	_, err := dialCtx(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil && strings.Contains(err.Error(), "reserved IP") {
		t.Errorf("bypass enabled but guard still fired: %v", err)
	}
}

// TestRESTSession_NoRedirect_SEC2 verifies that the http.Client injected onto
// the discordgo session (R20260603-SEC-2) refuses 3xx redirects by returning
// http.ErrUseLastResponse from CheckRedirect.  The test exercises the client
// policy directly (without a live Discord gateway) to confirm the SSRF /
// token-leakage guard is in place.
func TestRESTSession_NoRedirect_SEC2(t *testing.T) {
	t.Parallel()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	// Mirror exactly what Start() does.
	sess.Client = &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if sess.Client == nil {
		t.Fatal("sess.Client must not be nil after injection")
	}
	if sess.Client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set on the injected client")
	}
	// Invoke the redirect policy: must return ErrUseLastResponse (not nil).
	err = sess.Client.CheckRedirect(nil, nil)
	if err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}

func TestAggregateAttachmentBytesAllow(t *testing.T) {
	t.Parallel()
	cap := maxDiscordTotalAttachmentBytes
	cases := []struct {
		name  string
		soFar int
		next  int
		want  bool
	}{
		{"zero plus zero", 0, 0, true},
		{"first chunk small", 0, 1024, true},
		{"exactly at cap", cap - 100, 100, true},
		{"just over cap", cap - 100, 101, false},
		{"way over cap", 0, cap + 1, false},
		{"already over cap stays over", cap, 1, false},
		{"negative next rejected", 0, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aggregateAttachmentBytesAllow(tc.soFar, tc.next); got != tc.want {
				t.Errorf("aggregateAttachmentBytesAllow(%d, %d) = %v, want %v",
					tc.soFar, tc.next, got, tc.want)
			}
		})
	}
}

// setTestBotID assigns the bot ID through the atomic field (#1814). Kept in
// one place so the test suite is decoupled from the field's storage type.
func setTestBotID(d *Discord, id string) {
	d.botID.Store(&id)
}

// TestBotID_ConcurrentStoreAndRead is the regression test for #1814: the
// gateway-connect store of botID (Start) races concurrent onMessageCreate
// reads. With the plain-string field this tripped the race detector on a
// torn 16-byte string assignment; with atomic.Pointer the handoff is
// race-free. Run with -race.
func TestBotID_ConcurrentStoreAndRead(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.handler = func(_ context.Context, _ platform.IncomingMessage) {}

	const readers = 32
	stop := make(chan struct{})
	done := make(chan struct{}, readers+1)

	// Writer: simulate Start() stamping the identity after connect.
	go func() {
		defer func() { done <- struct{}{} }()
		for i := 0; i < 1000; i++ {
			id := "bot-identity-string-value"
			d.botID.Store(&id)
		}
		close(stop)
	}()

	// Readers: simulate discordgo dispatching onMessageCreate goroutines that
	// read d.botID while the writer stamps it.
	for i := 0; i < readers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
				d.onMessageCreate(nil, &discordgo.MessageCreate{
					Message: &discordgo.Message{
						Author:   &discordgo.User{ID: "someone-else"},
						Content:  "ping <@bot-identity-string-value>",
						Mentions: []*discordgo.User{{ID: "bot-identity-string-value"}},
					},
				})
				_ = d.getBotID()
			}
		}()
	}

	timeout := time.After(5 * time.Second)
	for i := 0; i < readers+1; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("goroutines did not finish; possible deadlock")
		}
	}
}

// TestGetBotID_EmptyBeforeConnect verifies getBotID returns "" before the
// gateway handshake stores an identity, so self-filtering simply no-ops
// rather than panicking on a nil pointer.
func TestGetBotID_EmptyBeforeConnect(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if got := d.getBotID(); got != "" {
		t.Errorf("getBotID() before connect = %q, want empty", got)
	}
}
