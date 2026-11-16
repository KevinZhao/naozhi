package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeILink serves get_qrcode_status from a script: the i-th poll gets
// replies[i] (the last one repeats) and its verify_code query is recorded.
type fakeILink struct {
	mu          sync.Mutex
	replies     []setupStatusResp
	verifyCodes []string
}

func (f *fakeILink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ilink/bot/get_qrcode_status" || r.URL.Query().Get("qrcode") != "qr-1" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	n := len(f.verifyCodes)
	f.verifyCodes = append(f.verifyCodes, r.URL.Query().Get("verify_code"))
	reply := f.replies[min(n, len(f.replies)-1)]
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeILink) codes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.verifyCodes...)
}

func newTestPoller(srv *httptest.Server, stdin string) (*qrPoller, *bytes.Buffer) {
	var out bytes.Buffer
	return &qrPoller{
		client:   srv.Client(),
		baseURL:  srv.URL,
		interval: time.Millisecond,
		in:       bufio.NewReader(strings.NewReader(stdin)),
		out:      &out,
	}, &out
}

var testQRConfirmed = setupStatusResp{Status: "confirmed", BotToken: "tok", ILinkUserID: "u@im.wechat"}

func TestQRPoller_Statuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		replies   []setupStatusResp
		stdin     string
		wantErr   string   // "" means the poll must end confirmed
		wantCodes []string // verify_code of each poll, when checked
		wantOut   string
	}{
		{
			name:    "wait then scaned then confirmed",
			replies: []setupStatusResp{{Status: "wait"}, {Status: "scaned"}, testQRConfirmed},
			wantOut: ".",
		},
		{
			name:    "expired",
			replies: []setupStatusResp{{Status: "expired"}},
			wantErr: "二维码已过期",
		},
		{
			name:    "confirmed without token",
			replies: []setupStatusResp{{Status: "confirmed"}},
			wantErr: "没有返回 token",
		},
		{
			name:      "need_verifycode sends the typed number until accepted",
			replies:   []setupStatusResp{{Status: "need_verifycode"}, {Status: "need_verifycode"}, {Status: "scaned"}, testQRConfirmed},
			stdin:     "12\n34\n",
			wantCodes: []string{"", "12", "34", ""},
			wantOut:   "数字不匹配",
		},
		{
			// openclaw-weixin prompts for any status containing need_verifycode.
			name:      "confirmed_but_need_verifycode asks for the number too",
			replies:   []setupStatusResp{{Status: "confirmed_but_need_verifycode"}, {Status: "scaned"}, testQRConfirmed},
			stdin:     "56\n",
			wantCodes: []string{"", "56", ""},
			wantOut:   "输入手机微信上显示的数字",
		},
		{
			name:    "need_verifycode with no input",
			replies: []setupStatusResp{{Status: "need_verifycode"}},
			wantErr: "可交互的终端",
		},
		{
			name:    "verify_code_blocked",
			replies: []setupStatusResp{{Status: "verify_code_blocked"}},
			wantErr: "verify_code_blocked",
		},
		{
			name:    "unknown status is reported",
			replies: []setupStatusResp{{Status: "brand_new"}, testQRConfirmed},
			wantOut: `未识别的扫码状态 "brand_new"`,
		},
		{
			name:    "scaned_but_redirect without redirect_host is reported",
			replies: []setupStatusResp{{Status: "scaned_but_redirect"}, testQRConfirmed},
			wantOut: `未识别的扫码状态 "scaned_but_redirect"`,
		},
		{
			// A path in redirect_host must not be followed: polling stays here.
			name:    "scaned_but_redirect with a path is not followed",
			replies: []setupStatusResp{{Status: "scaned_but_redirect", RedirectHost: "127.0.0.1:1/x?"}, testQRConfirmed},
			wantOut: `未识别的扫码状态 "scaned_but_redirect"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeILink{replies: tt.replies}
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			p, out := newTestPoller(srv, tt.stdin)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			got, err := p.poll(ctx, "qr-1")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("poll error = %v, want one containing %q", err, tt.wantErr)
				}
			} else if err != nil || got.BotToken != "tok" {
				t.Fatalf("poll = %+v, %v; want the confirmed reply", got, err)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output %q does not contain %q", out.String(), tt.wantOut)
			}
			if tt.wantCodes != nil {
				if codes := fake.codes(); strings.Join(codes, ",") != strings.Join(tt.wantCodes, ",") {
					t.Errorf("verify_code per poll = %q, want %q", codes, tt.wantCodes)
				}
			}
		})
	}
}

// TestQRPoller_FollowsRedirectHost: after scaned_but_redirect the remaining
// polls go to redirect_host, where the scan is confirmed.
func TestQRPoller_FollowsRedirectHost(t *testing.T) {
	t.Parallel()
	confirmed := testQRConfirmed
	confirmed.BaseURL = "https://sh.ilinkai.weixin.qq.com"
	idc := &fakeILink{replies: []setupStatusResp{{Status: "scaned"}, confirmed}}
	idcSrv := httptest.NewTLSServer(idc)
	defer idcSrv.Close()
	host := strings.TrimPrefix(idcSrv.URL, "https://")

	origin := &fakeILink{replies: []setupStatusResp{{Status: "scaned_but_redirect", RedirectHost: host}}}
	originSrv := httptest.NewTLSServer(origin)
	defer originSrv.Close()

	p, out := newTestPoller(originSrv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := p.poll(ctx, "qr-1")
	if err != nil || got.BotToken != "tok" {
		t.Fatalf("poll = %+v, %v; want the confirmed reply from the redirect host", got, err)
	}
	if got.BaseURL != confirmed.BaseURL {
		t.Errorf("confirmed baseurl = %q, want %q", got.BaseURL, confirmed.BaseURL)
	}
	if n := len(origin.codes()); n != 1 {
		t.Errorf("origin polled %d times, want 1 (then switch hosts)", n)
	}
	if n := len(idc.codes()); n != 2 {
		t.Errorf("redirect host polled %d times, want 2", n)
	}
	if !strings.Contains(out.String(), "改到 "+idcSrv.URL) {
		t.Errorf("output %q does not announce the host switch", out.String())
	}
}

func TestIsBareHost(t *testing.T) {
	t.Parallel()
	for h, want := range map[string]bool{
		"sh.ilinkai.weixin.qq.com":     true,
		"ilinkai.weixin.qq.com:8443":   true,
		"127.0.0.1:1234":               true,
		"":                             false,
		"evil.example/x":               false,
		"evil.example?q=1":             false,
		"evil.example#f":               false,
		"user@evil.example":            false,
		":443":                         false,
		"ilinkai.weixin.qq.com\x1b[2J": false,
	} {
		if got := isBareHost(h); got != want {
			t.Errorf("isBareHost(%q) = %v, want %v", h, got, want)
		}
	}
}
