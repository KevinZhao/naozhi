package feishu

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// captureDefaultSlog redirects the default slog logger to an in-memory buffer
// for the duration of fn, restoring the previous default afterwards. Because it
// mutates the process-global default logger, callers MUST NOT run in parallel.
func captureDefaultSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestWebhook_InsecureModeEmitsRuntimeError pins #1724: when a feishu webhook
// runs in token-only mode (encrypt_key absent, allow_insecure_webhook opted
// in), the FIRST live delivery must emit a SECURITY error so operators get a
// traffic-correlated signal rather than only the easy-to-miss startup line.
func TestWebhook_InsecureModeEmitsRuntimeError(t *testing.T) {
	f := New(Config{
		AppID: "id", AppSecret: "secret",
		ConnectionMode:       "webhook",
		VerificationToken:    "test_token",
		AllowInsecureWebhook: true,
	}, nil)
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := buildV2MessageBody("ev_insecure_1", "oc_chat1", "p2p", "hello")

	out := captureDefaultSlog(t, func() {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(body))
	})

	if !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, "SECURITY") ||
		!strings.Contains(out, "verification_token-only") {
		t.Fatalf("expected a SECURITY ERROR on first insecure-mode delivery, got log = %q", out)
	}
}

// TestWebhook_InsecureModeRuntimeErrorOnce pins the sync.Once bound: a flood of
// webhook deliveries must emit the SECURITY error at most once per process so
// an attacker cannot amplify it into a log-spam DoS.
func TestWebhook_InsecureModeRuntimeErrorOnce(t *testing.T) {
	f := New(Config{
		AppID: "id", AppSecret: "secret",
		ConnectionMode:       "webhook",
		VerificationToken:    "test_token",
		AllowInsecureWebhook: true,
	}, nil)
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	out := captureDefaultSlog(t, func() {
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, buildTokenRequest(buildV2MessageBody("ev_flood", "oc_chat1", "p2p", "hi")))
		}
	})

	if n := strings.Count(out, "verification_token-only mode (no encrypt_key/HMAC)"); n != 1 {
		t.Fatalf("expected exactly 1 SECURITY error across 5 deliveries, got %d; log = %q", n, out)
	}
}

// TestWebhook_SecureModeNoRuntimeError pins the negative: a webhook configured
// with an encrypt_key (HMAC-verified, secure posture) must NOT emit the
// insecure-mode runtime error even though deliveries are processed.
func TestWebhook_SecureModeNoRuntimeError(t *testing.T) {
	const ek = "secure_encrypt_key"
	f := New(Config{
		AppID: "id", AppSecret: "secret",
		ConnectionMode: "webhook",
		EncryptKey:     ek,
	}, nil)
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := buildV2MessageBody("ev_secure_1", "oc_chat1", "p2p", "hello")
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonce := "secure-nonce-1724"

	out := captureDefaultSlog(t, func() {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildSignedRequest(t, body, ts, nonce, ek))
	})

	if strings.Contains(out, "verification_token-only mode (no encrypt_key/HMAC)") {
		t.Fatalf("secure encrypt_key mode must not emit insecure-mode runtime error; log = %q", out)
	}
}
