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
			wantErr: "QR code expired",
		},
		{
			name:    "confirmed without token",
			replies: []setupStatusResp{{Status: "confirmed"}},
			wantErr: "no token",
		},
		{
			name:      "need_verifycode sends the typed number until accepted",
			replies:   []setupStatusResp{{Status: "need_verifycode"}, {Status: "need_verifycode"}, {Status: "scaned"}, testQRConfirmed},
			stdin:     "12\n34\n",
			wantCodes: []string{"", "12", "34", ""},
			wantOut:   "did not match",
		},
		{
			name:    "need_verifycode with no input",
			replies: []setupStatusResp{{Status: "need_verifycode"}},
			wantErr: "interactive terminal",
		},
		{
			name:    "verify_code_blocked",
			replies: []setupStatusResp{{Status: "verify_code_blocked"}},
			wantErr: "verify_code_blocked",
		},
		{
			name:    "binded_redirect",
			replies: []setupStatusResp{{Status: "binded_redirect"}},
			wantErr: "binded_redirect",
		},
		{
			name:    "redirect without host keeps polling",
			replies: []setupStatusResp{{Status: "scaned_but_redirect"}, {Status: "scaned_but_redirect"}, testQRConfirmed},
			wantOut: "without a usable redirect_host",
		},
		{
			name:    "unknown status is reported once",
			replies: []setupStatusResp{{Status: "brand_new"}, {Status: "brand_new"}, testQRConfirmed},
			wantOut: `unknown iLink QR status "brand_new"`,
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
			if tt.wantOut != "" && strings.Count(out.String(), tt.wantOut) != 1 {
				t.Errorf("output %q repeats %q", out.String(), tt.wantOut)
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
	idc := &fakeILink{replies: []setupStatusResp{{Status: "scaned"}, testQRConfirmed}}
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
	if n := len(origin.codes()); n != 1 {
		t.Errorf("origin polled %d times, want 1 (then switch hosts)", n)
	}
	if n := len(idc.codes()); n != 2 {
		t.Errorf("redirect host polled %d times, want 2", n)
	}
	if !strings.Contains(out.String(), "Continuing on "+idcSrv.URL) {
		t.Errorf("output %q does not announce the host switch", out.String())
	}
}

func TestRedirectBaseURL(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{
		"sh.ilinkai.weixin.qq.com":      "https://sh.ilinkai.weixin.qq.com",
		"127.0.0.1:8443":                "https://127.0.0.1:8443",
		"":                              "",
		"evil.com/path":                 "",
		"user@evil.com":                 "",
		"evil.com?x=1":                  "",
		"evil.com#frag":                 "",
		"https://sh.ilinkai.weixin.com": "",
		":8443":                         "",
	} {
		got, ok := redirectBaseURL(host)
		if got != want || ok != (want != "") {
			t.Errorf("redirectBaseURL(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}
