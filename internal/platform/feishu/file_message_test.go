package feishu

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

var pdfBytes = []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")

// fileStub serves the resource endpoint with a fixed status and body and
// records the query of every download it was asked for.
type fileStub struct {
	status int
	body   []byte

	mu      sync.Mutex
	queries []string
	hits    atomic.Int64
}

func (s *fileStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "/resources/") {
		http.NotFound(w, r)
		return
	}
	s.hits.Add(1)
	s.mu.Lock()
	s.queries = append(s.queries, r.URL.Path+"?"+r.URL.RawQuery)
	s.mu.Unlock()
	if s.status != 0 && s.status != http.StatusOK {
		w.WriteHeader(s.status)
		return
	}
	_, _ = w.Write(s.body)
}

func newFileFeishu(t *testing.T, cfg Config, upstream http.Handler) *Feishu {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	f := New(cfg, nil)
	t.Cleanup(func() { _ = f.Stop() })
	f.baseURL = srv.URL
	f.accessToken = "t_cached"
	f.tokenExpiry = time.Now().Add(time.Hour)
	return f
}

func TestParseSDKEvent_FileMessage(t *testing.T) {
	t.Parallel()
	pe, ok := (&Feishu{}).parseSDKEvent(sdkMediaEvent("ev_f", "file",
		`{"file_key":"file_v3_abc","file_name":"季度报告.pdf"}`))
	if !ok {
		t.Fatal("parseSDKEvent rejected a file message")
	}
	if pe.MediaType != "file" || pe.MediaKey != "file_v3_abc" || pe.MediaName != "季度报告.pdf" {
		t.Errorf("parsed %+v, want type=file key=file_v3_abc name=季度报告.pdf", pe)
	}
	if pe.MessageID != "om_ev_f" || pe.Msg.UserID != "ou_eve" {
		t.Errorf("MessageID=%q UserID=%q, want om_ev_f / ou_eve", pe.MessageID, pe.Msg.UserID)
	}

	if _, ok := (&Feishu{}).parseSDKEvent(sdkMediaEvent("ev_f2", "file", `{"file_name":"a.pdf"}`)); ok {
		t.Error("file message without file_key was accepted")
	}
}

// The WS transport validates resource keys like the webhook does: the key is
// sender-controlled and goes into the download URL.
func TestParseSDKEvent_RejectsMalformedResourceKey(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"space":     "img key",
		"newline":   "img\\nkey",
		"non-ascii": "图片",
		"too long":  strings.Repeat("k", 257),
	}
	for _, msgType := range []string{"image", "audio", "file"} {
		field := "file_key"
		if msgType == "image" {
			field = "image_key"
		}
		for name, key := range bad {
			t.Run(msgType+"/"+name, func(t *testing.T) {
				t.Parallel()
				ev := sdkMediaEvent("ev_bad", msgType, `{"`+field+`":"`+key+`"}`)
				if pe, ok := (&Feishu{}).parseSDKEvent(ev); ok {
					t.Errorf("accepted malformed %s key %q: %+v", msgType, key, pe)
				}
			})
		}
		t.Run(msgType+"/valid", func(t *testing.T) {
			t.Parallel()
			ev := sdkMediaEvent("ev_ok", msgType, `{"`+field+`":"k_v3_0123-abc"}`)
			if _, ok := (&Feishu{}).parseSDKEvent(ev); !ok {
				t.Errorf("rejected a well-formed %s key", msgType)
			}
		})
	}
}

func TestHandleFile_Download(t *testing.T) {
	t.Parallel()
	atCap := bytes.Repeat([]byte("a"), limits.MaxFileAttachmentBytes)
	overCap := append(append([]byte(nil), atCap...), 'a')
	cases := []struct {
		name       string
		stub       *fileStub
		wantData   []byte
		wantReject platform.FileReject
	}{
		{"pdf", &fileStub{body: pdfBytes}, pdfBytes, platform.FileRejectNone},
		{"at the cap", &fileStub{body: atCap}, atCap, platform.FileRejectNone},
		{"over the cap", &fileStub{body: overCap}, nil, platform.FileRejectTooLarge},
		{"server error", &fileStub{status: http.StatusInternalServerError}, nil, platform.FileRejectDownloadFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFileFeishu(t, Config{}, tc.stub)
			var h handled
			f.handleFile(context.Background(), h.handler, mediaMsg, "om_1", "file_1", "report.pdf")

			got := h.all()
			if len(got) != 1 || len(got[0].Files) != 1 {
				t.Fatalf("handler got %d messages (%+v), want one carrying one file", len(got), got)
			}
			file := got[0].Files[0]
			if file.Name != "report.pdf" || file.Reject != tc.wantReject || !bytes.Equal(file.Data, tc.wantData) {
				t.Errorf("file = {Name:%q Reject:%q len(Data):%d}, want {report.pdf %q %d}",
					file.Name, file.Reject, len(file.Data), tc.wantReject, len(tc.wantData))
			}
			if got[0].UserID != mediaMsg.UserID || got[0].ChatID != mediaMsg.ChatID {
				t.Errorf("forwarded message lost its sender: %+v", got[0])
			}
			tc.stub.mu.Lock()
			defer tc.stub.mu.Unlock()
			if len(tc.stub.queries) != 1 || tc.stub.queries[0] != "/open-apis/im/v1/messages/om_1/resources/file_1?type=file" {
				t.Errorf("downloads = %q, want one type=file request", tc.stub.queries)
			}
		})
	}
}

func TestHandleFile_RefusedOrCancelledForwardsNothing(t *testing.T) {
	t.Parallel()
	t.Run("refused sender", func(t *testing.T) {
		t.Parallel()
		stub := &fileStub{body: pdfBytes}
		f := newFileFeishu(t, Config{}, stub)
		var asked []string
		var mu sync.Mutex
		f.SetAdmission(refuseRecording(&asked, &mu))
		var h handled
		f.handleFile(context.Background(), h.handler, mediaMsg, "om_1", "file_1", "report.pdf")
		if stub.hits.Load() != 0 || len(h.all()) != 0 {
			t.Errorf("refused file: downloads=%d handled=%d, want 0/0", stub.hits.Load(), len(h.all()))
		}
		if len(asked) != 1 {
			t.Errorf("admission asked %d times, want 1", len(asked))
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		f := newFileFeishu(t, Config{}, &fileStub{body: pdfBytes})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var h handled
		f.handleFile(ctx, h.handler, mediaMsg, "om_1", "file_1", "report.pdf")
		if got := h.all(); len(got) != 0 {
			t.Errorf("an abandoned download was forwarded: %+v", got)
		}
	})
}

// Both transports route a file message to the download and on to the
// handler with the sender's file name.
func TestFileMessage_BothTransports(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, stub *fileStub, h *handled) {
		t.Helper()
		got := h.all()
		if len(got) != 1 || len(got[0].Files) != 1 {
			t.Fatalf("handler got %+v, want one message with one file", got)
		}
		if file := got[0].Files[0]; file.Name != "notes.md" || !bytes.Equal(file.Data, pdfBytes) || file.Reject != "" {
			t.Errorf("file = %+v, want notes.md with the downloaded bytes", file)
		}
		if got[0].Text != "" || len(got[0].Images) != 0 {
			t.Errorf("file message carried text %q / %d images", got[0].Text, len(got[0].Images))
		}
		if stub.hits.Load() != 1 {
			t.Errorf("downloads = %d, want 1", stub.hits.Load())
		}
	}
	const content = `{"file_key":"file_v3_abc","file_name":"notes.md"}`

	t.Run("webhook", func(t *testing.T) {
		t.Parallel()
		stub := &fileStub{body: pdfBytes}
		f := newFileFeishu(t, Config{ConnectionMode: "webhook", VerificationToken: "test_token"}, stub)
		var h handled
		mux := http.NewServeMux()
		f.registerWebhook(mux, h.handler)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(buildV2MediaBody("ev_file", "file", content)))
		if w.Code != http.StatusOK {
			t.Fatalf("webhook status = %d, want 200", w.Code)
		}
		f.dispatch.Wait()
		check(t, stub, &h)
	})
	t.Run("websocket", func(t *testing.T) {
		t.Parallel()
		stub := &fileStub{body: pdfBytes}
		f := newFileFeishu(t, Config{}, stub)
		var h handled
		pe, ok := f.parseSDKEvent(sdkMediaEvent("ev_file", "file", content))
		if !ok {
			t.Fatal("parseSDKEvent rejected the file message")
		}
		f.routeParsed(context.Background(), h.handler, pe)
		f.dispatch.Wait()
		check(t, stub, &h)
	})
}

func TestWebhook_FileMessageMalformedKeyNotDownloaded(t *testing.T) {
	t.Parallel()
	stub := &fileStub{body: pdfBytes}
	f := newFileFeishu(t, Config{ConnectionMode: "webhook", VerificationToken: "test_token"}, stub)
	var h handled
	mux := http.NewServeMux()
	f.registerWebhook(mux, h.handler)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, buildTokenRequest(buildV2MediaBody("ev_badfile", "file", `{"file_key":"file key","file_name":"a.pdf"}`)))
	f.dispatch.Wait()
	if stub.hits.Load() != 0 || len(h.all()) != 0 {
		t.Errorf("malformed file_key: downloads=%d handled=%d, want 0/0", stub.hits.Load(), len(h.all()))
	}
}

// The shared client's 10s Client.Timeout also covers the body read, so a file
// near the byte cap would need ~3.4 MB/s; file downloads get their own budget
// on the same connection pool.
func TestFileDownloadClient_Budget(t *testing.T) {
	t.Parallel()
	f := New(Config{}, nil)
	t.Cleanup(func() { _ = f.Stop() })
	if f.fileHTTP != feishuFileDownloadClient {
		t.Fatal("New does not wire feishuFileDownloadClient for file downloads")
	}
	fc := feishuFileDownloadClient
	if fc == feishuHTTPClient {
		t.Fatal("file downloads use the shared 10s client")
	}
	// The cap must survive a 256 KiB/s link.
	minBudget := time.Duration(limits.MaxFileAttachmentBytes/(256<<10)) * time.Second
	if fc.Timeout < minBudget {
		t.Errorf("file download Timeout = %v, want a bounded budget of at least %v", fc.Timeout, minBudget)
	}
	if fc.Transport != feishuHTTPClient.Transport {
		t.Error("file download client does not share the Feishu transport (TLS floor, pool)")
	}
}

// countingTransport counts the requests it forwards to http.DefaultTransport.
type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// DownloadFile goes through the file client; image and audio stay on the
// shared one.
func TestDownloadFile_UsesFileClient(t *testing.T) {
	t.Parallel()
	stub := &fileStub{body: pdfBytes}
	f := newFileFeishu(t, Config{}, stub)
	rt := &countingTransport{}
	f.fileHTTP = &http.Client{Transport: rt}

	if _, err := f.DownloadFile(context.Background(), "om_1", "file_1"); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	// Both fail the image/audio sniff on PDF bytes; only the route matters.
	_, _, _ = f.DownloadImage(context.Background(), "om_1", "img_1")
	_, _, _ = f.DownloadAudio(context.Background(), "om_1", "aud_1")
	if got := rt.n.Load(); got != 1 {
		t.Errorf("file client carried %d requests, want only the file download", got)
	}
	if got := stub.hits.Load(); got != 3 {
		t.Errorf("upstream saw %d downloads, want 3", got)
	}
}

// A file download carries the bearer token, so a 3xx is surfaced as a failure
// rather than followed to wherever the upstream points it.
func TestDownloadFile_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var followed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		_, _ = w.Write(pdfBytes)
	})
	mux.HandleFunc("/open-apis/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	f := newFileFeishu(t, Config{}, mux)
	if data, err := f.DownloadFile(context.Background(), "om_1", "file_1"); err == nil {
		t.Errorf("redirected download succeeded with %d bytes, want an error", len(data))
	}
	if n := followed.Load(); n != 0 {
		t.Errorf("redirect followed %d times, want 0", n)
	}
}
