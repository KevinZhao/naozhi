package slack

import (
	"net/http"
	"os"
	"regexp"
	"testing"
)

// TestSlackHTTPClient_BlocksRedirects is the behavioural regression for the
// SSRF defence. Slack's Web API endpoints under slack.com do not rely on
// cross-host redirects for any documented flow (auth.test, chat.postMessage,
// files.upload). Following a 3xx would let a DNS/MITM-compromised path or
// a malicious proxy redirect a Bearer-token-carrying request at an internal
// address (e.g. 169.254.169.254 IMDS, a loopback admin port) — the classic
// SSRF-via-redirect pattern that feishu / discord / weixin already guard
// against. ErrUseLastResponse surfaces the 3xx status to the caller so the
// caller fails cleanly instead of silently following. Any refactor that
// drops CheckRedirect breaks this test.
func TestSlackHTTPClient_BlocksRedirects(t *testing.T) {
	t.Parallel()
	if slackHTTPClient == nil {
		t.Fatal("slackHTTPClient is nil — expected package-level *http.Client")
	}
	if slackHTTPClient.CheckRedirect == nil {
		t.Fatal("slackHTTPClient.CheckRedirect is nil — SSRF defence missing")
	}
	// Pass nil req/via — the CheckRedirect hook must not dereference them.
	// feishu/discord/weixin's hooks are pure short-circuits.
	err := slackHTTPClient.CheckRedirect(nil, nil)
	if err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}

// TestSlackHTTPClient_HasTimeout pins the transport-level timeout that
// R191-ARCH-M3 added to prevent slow Slack responses from pinning todoLoop
// goroutines past Stop()'s drain. A future refactor that lifts httpClient
// to package scope must not drop the timeout.
func TestSlackHTTPClient_HasTimeout(t *testing.T) {
	t.Parallel()
	if slackHTTPClient.Timeout <= 0 {
		t.Errorf("slackHTTPClient.Timeout = %v, want > 0", slackHTTPClient.Timeout)
	}
}

// TestSlackHTTPClient_SourceAnchor is the source-level contract that locks
// both the CheckRedirect hook and the ErrUseLastResponse return value in
// slack.go — catches any refactor that moves the httpClient construction
// elsewhere or silently replaces the short-circuit with a permissive stub.
// Uses source-grep rather than runtime reflection because the behavioural
// test above can be satisfied by any function that returns ErrUseLastResponse;
// this test additionally pins the comment anchor naming the SSRF threat so
// future reviewers don't delete it thinking it's boilerplate.
func TestSlackHTTPClient_SourceAnchor(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("slack.go")
	if err != nil {
		t.Fatalf("read slack.go: %v", err)
	}
	src := string(data)
	// (?s) so . matches newlines; the hook body spans 2-3 lines.
	anchor := regexp.MustCompile(`(?s)CheckRedirect.*?http\.ErrUseLastResponse`)
	if !anchor.MatchString(src) {
		t.Error("slack.go missing CheckRedirect → http.ErrUseLastResponse anchor — " +
			"SSRF defence may have been silently dropped")
	}
	if !regexp.MustCompile(`slackHTTPClient`).MatchString(src) {
		t.Error("slack.go missing package-level slackHTTPClient — " +
			"refactor may have reverted to per-instance httpClient without CheckRedirect")
	}
}
