package slack

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var _ platform.Admitter = (*Slack)(nil)

var (
	testPDF = []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF\n")
	testPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
)

// stubResp is one canned answer of fileStub.
type stubResp struct {
	status int
	ctype  string
	body   []byte
	header http.Header
}

// fileStub answers downloads by URL path and records every request.
type fileStub struct {
	mu    sync.Mutex
	resps map[string]stubResp
	reqs  []*http.Request
}

func (f *fileStub) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	r, ok := f.resps[req.URL.Path]
	f.mu.Unlock()
	if !ok {
		r = stubResp{status: http.StatusNotFound}
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	h := http.Header{}
	for k, v := range r.header {
		h[k] = v
	}
	if r.ctype != "" {
		h.Set("Content-Type", r.ctype)
	}
	return &http.Response{StatusCode: r.status, Header: h, Body: io.NopCloser(bytes.NewReader(r.body)), Request: req}, nil
}

func (f *fileStub) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

// newFileSlack returns an adapter whose file downloads go to stub through a
// copy of the production file client, so its redirect policy is the real one.
func newFileSlack(stub *fileStub) *Slack {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.botID = "UBOT"
	c := *slackFileHTTPClient
	c.Transport = stub
	s.fileHTTP = &c
	return s
}

func slackURL(path string) string { return "https://" + slackFileHost + path }

func fileShare(text string, files ...slack.File) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		User: "U456", Channel: "D789", ChannelType: "im", Text: text,
		TimeStamp: "1234567890.000100", SubType: subtypeFileShare,
		Message: &slack.Msg{Files: files},
	}
}

// deliver runs handleMessage and returns the messages the handler received.
func deliver(s *Slack, ev *slackevents.MessageEvent) []platform.IncomingMessage {
	var mu sync.Mutex
	var got []platform.IncomingMessage
	s.handler = func(_ context.Context, m platform.IncomingMessage) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}
	s.handleMessage(ev)
	s.dispatch.Wait()
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestHandleMessage_FileShareDeliversFilesAndImages(t *testing.T) {
	t.Parallel()
	stub := &fileStub{resps: map[string]stubResp{
		"/a/report.pdf": {ctype: "application/pdf", body: testPDF},
		"/a/shot.png":   {ctype: "image/png", body: testPNG},
	}}
	s := newFileSlack(stub)
	got := deliver(s, fileShare("总结一下",
		slack.File{ID: "F1", Name: "report.pdf", Mimetype: "application/pdf", Size: len(testPDF), URLPrivateDownload: slackURL("/a/report.pdf")},
		slack.File{ID: "F2", Name: "shot.png", Mimetype: "image/png", Size: len(testPNG), URLPrivateDownload: slackURL("/a/shot.png")},
	))
	if len(got) != 1 {
		t.Fatalf("handler called %d times, want 1", len(got))
	}
	m := got[0]
	if m.Text != "总结一下" {
		t.Errorf("Text = %q, want the caption", m.Text)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "report.pdf" || !bytes.Equal(m.Files[0].Data, testPDF) || m.Files[0].Reject != platform.FileRejectNone {
		t.Errorf("Files = %+v, want report.pdf with its bytes", m.Files)
	}
	if len(m.Images) != 1 || m.Images[0].MimeType != "image/png" || !bytes.Equal(m.Images[0].Data, testPNG) {
		t.Errorf("Images = %+v, want shot.png as image/png", m.Images)
	}
	reqs := stub.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d downloads, want 2", len(reqs))
	}
	for _, r := range reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("Authorization = %q, want the bot token", got)
		}
	}
}

func TestHandleMessage_FileShareWithoutCaption(t *testing.T) {
	t.Parallel()
	stub := &fileStub{resps: map[string]stubResp{"/a/shot.png": {body: testPNG}}}
	s := newFileSlack(stub)
	got := deliver(s, fileShare("", slack.File{Name: "shot.png", Mimetype: "image/png", URLPrivate: slackURL("/a/shot.png")}))
	if len(got) != 1 || got[0].Text != "" || len(got[0].Images) != 1 {
		t.Fatalf("got %+v, want one image-only message (url_private fallback)", got)
	}
}

func TestHandleMessage_OtherSubtypesIgnoreFiles(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"message_changed", "thread_broadcast", "bot_message"} {
		stub := &fileStub{resps: map[string]stubResp{"/a/x.pdf": {body: testPDF}}}
		s := newFileSlack(stub)
		ev := fileShare("hi", slack.File{Name: "x.pdf", URLPrivateDownload: slackURL("/a/x.pdf")})
		ev.SubType = sub
		if got := deliver(s, ev); len(got) != 0 {
			t.Errorf("subtype %s: handler called %d times, want 0", sub, len(got))
		}
		if n := len(stub.requests()); n != 0 {
			t.Errorf("subtype %s: %d downloads, want 0", sub, n)
		}
	}
}

// TestDownloadFile_HostWhitelist pins that the bot token is only ever sent to
// https://files.slack.com: every other URL fails before any request.
func TestDownloadFile_HostWhitelist(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"http://files.slack.com/a/x.pdf",
		"https://evil.example/a/x.pdf",
		"https://files.slack.com.evil.example/a/x.pdf",
		"https://u:p@files.slack.com/a/x.pdf",
		"https://files.slack.com:8443/a/x.pdf",
		"https://slack.com/a/x.pdf",
		"",
	} {
		stub := &fileStub{resps: map[string]stubResp{"/a/x.pdf": {body: testPDF}}}
		s := newFileSlack(stub)
		_, err := s.downloadFile(context.Background(), slack.File{URLPrivateDownload: raw}, 1<<20)
		if err == nil {
			t.Errorf("%q: download succeeded, want refused", raw)
		}
		if n := len(stub.requests()); n != 0 {
			t.Errorf("%q: %d requests sent, want 0", raw, n)
		}
	}
}

func TestDownloadFile_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	stub := &fileStub{resps: map[string]stubResp{
		"/a/x.pdf":   {status: http.StatusFound, header: http.Header{"Location": {"https://files.slack.com/elsewhere"}}},
		"/elsewhere": {body: testPDF},
	}}
	s := newFileSlack(stub)
	if _, err := s.downloadFile(context.Background(), slack.File{URLPrivateDownload: slackURL("/a/x.pdf")}, 1<<20); err == nil {
		t.Error("a 302 download succeeded, want an error")
	}
	for _, r := range stub.requests() {
		if r.URL.Path == "/elsewhere" {
			t.Error("the redirect target was requested")
		}
	}
}

func TestSlackFileHTTPClient_Budget(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if s.fileHTTP != slackFileHTTPClient {
		t.Fatal("New does not wire slackFileHTTPClient")
	}
	if slackFileHTTPClient == slackHTTPClient {
		t.Fatal("file downloads share the 10s API client")
	}
	// 32 MiB at 256 KB/s; the API client's 10s would fail any slow link.
	if slackFileHTTPClient.Timeout < 128*time.Second {
		t.Errorf("file client timeout = %v, want at least 128s", slackFileHTTPClient.Timeout)
	}
}

// TestAttachFiles_Rejects pins that every upload that is not delivered is
// reported with its reason, and that caps are applied before any download
// when Slack's declared size already exceeds them.
func TestAttachFiles_Rejects(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("a"), maxSlackImageBytes+1)
	file := func(name, mime string, size int) slack.File {
		return slack.File{Name: name, Mimetype: mime, Size: size, URLPrivateDownload: slackURL("/a/" + name)}
	}
	cases := []struct {
		name      string
		resp      stubResp
		file      slack.File
		want      platform.FileReject
		wantFetch bool
	}{
		{"declared over file cap", stubResp{body: testPDF}, file("x.pdf", "application/pdf", limits.MaxFileAttachmentBytes+1), platform.FileRejectTooLarge, false},
		{"declared over image cap", stubResp{body: testPNG}, file("x.png", "image/png", maxSlackImageBytes+1), platform.FileRejectTooLarge, false},
		{"body over image cap despite declared size", stubResp{body: big}, file("x.png", "image/png", 10), platform.FileRejectTooLarge, true},
		{"image bytes are not an image", stubResp{body: testPDF}, file("x.png", "image/png", len(testPDF)), platform.FileRejectUnsupported, true},
		{"not found", stubResp{status: http.StatusNotFound}, file("x.pdf", "application/pdf", 10), platform.FileRejectDownloadFailed, true},
		{"sign-in page", stubResp{ctype: "text/html; charset=utf-8", body: []byte("<html>")}, file("x.txt", "text/plain", 6), platform.FileRejectDownloadFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &fileStub{resps: map[string]stubResp{"/a/" + tc.file.Name: tc.resp}}
			s := newFileSlack(stub)
			var msg platform.IncomingMessage
			s.attachFiles(context.Background(), &msg, []slack.File{tc.file})
			if len(msg.Images) != 0 || len(msg.Files) != 1 {
				t.Fatalf("images %d files %+v, want one rejected file", len(msg.Images), msg.Files)
			}
			f := msg.Files[0]
			if f.Name != tc.file.Name || f.Reject != tc.want || f.Data != nil {
				t.Errorf("file = {%q %q %d bytes}, want {%q %q no bytes}", f.Name, f.Reject, len(f.Data), tc.file.Name, tc.want)
			}
			if fetched := len(stub.requests()) > 0; fetched != tc.wantFetch {
				t.Errorf("fetched = %v, want %v", fetched, tc.wantFetch)
			}
		})
	}
}

func TestAttachFiles_MessageCaps(t *testing.T) {
	t.Parallel()
	t.Run("count", func(t *testing.T) {
		t.Parallel()
		stub := &fileStub{resps: map[string]stubResp{}}
		var files []slack.File
		for i := 0; i <= limits.MaxFileAttachmentsPerMessage; i++ {
			name := "f" + string(rune('a'+i)) + ".txt"
			stub.resps["/a/"+name] = stubResp{body: []byte("ok\n")}
			files = append(files, slack.File{Name: name, Mimetype: "text/plain", Size: 3, URLPrivateDownload: slackURL("/a/" + name)})
		}
		s := newFileSlack(stub)
		var msg platform.IncomingMessage
		s.attachFiles(context.Background(), &msg, files)
		if n := len(stub.requests()); n != limits.MaxFileAttachmentsPerMessage {
			t.Errorf("%d downloads, want %d", n, limits.MaxFileAttachmentsPerMessage)
		}
		last := msg.Files[len(msg.Files)-1]
		if len(msg.Files) != len(files) || last.Reject != platform.FileRejectTooMany || last.Data != nil {
			t.Errorf("files = %d, last = %+v; want the extra file reported as too many", len(msg.Files), last)
		}
	})
	t.Run("aggregate declared", func(t *testing.T) {
		t.Parallel()
		half := limits.MaxFileAttachmentBytes/2 + 1
		stub := &fileStub{resps: map[string]stubResp{
			"/a/a.txt": {body: bytes.Repeat([]byte("a"), half)},
			"/a/b.txt": {body: bytes.Repeat([]byte("b"), half)},
		}}
		s := newFileSlack(stub)
		var msg platform.IncomingMessage
		s.attachFiles(context.Background(), &msg, []slack.File{
			{Name: "a.txt", Size: half, URLPrivateDownload: slackURL("/a/a.txt")},
			{Name: "b.txt", Size: half, URLPrivateDownload: slackURL("/a/b.txt")},
		})
		if len(msg.Files) != 2 || len(msg.Files[0].Data) != half || msg.Files[1].Reject != platform.FileRejectTotalTooLarge {
			t.Errorf("files = %d, second reject %q; want a.txt kept and b.txt over the aggregate", len(msg.Files), msg.Files[1].Reject)
		}
		if n := len(stub.requests()); n != 1 {
			t.Errorf("%d downloads, want b.txt refused before download", n)
		}
	})
	t.Run("aggregate actual", func(t *testing.T) {
		t.Parallel()
		half := limits.MaxFileAttachmentBytes/2 + 1
		stub := &fileStub{resps: map[string]stubResp{
			"/a/a.txt": {body: bytes.Repeat([]byte("a"), half)},
			"/a/b.txt": {body: bytes.Repeat([]byte("b"), half)},
		}}
		s := newFileSlack(stub)
		var msg platform.IncomingMessage
		s.attachFiles(context.Background(), &msg, []slack.File{
			{Name: "a.txt", Size: 1, URLPrivateDownload: slackURL("/a/a.txt")},
			{Name: "b.txt", Size: 1, URLPrivateDownload: slackURL("/a/b.txt")},
		})
		if len(msg.Files) != 2 || msg.Files[1].Reject != platform.FileRejectTotalTooLarge || msg.Files[1].Data != nil {
			t.Errorf("second file reject %q, want total_too_large without bytes", msg.Files[1].Reject)
		}
	})
}

// TestHandleMessage_FileShareAdmissionBeforeDownload: a sender the
// dispatcher would refuse, or an unmentioned group upload, costs no download.
func TestHandleMessage_FileShareAdmissionBeforeDownload(t *testing.T) {
	t.Parallel()
	mentionGate := func(_ context.Context, m platform.IncomingMessage) bool {
		return m.ChatType != "group" || m.MentionMe
	}
	refuse := func(context.Context, platform.IncomingMessage) bool { return false }
	cases := []struct {
		name        string
		admit       platform.AdmitFunc
		channelType string
		text        string
		wantFetch   int
		wantHandled int
	}{
		{"refused", refuse, "im", "", 0, 0},
		{"unmentioned group upload", mentionGate, "channel", "look", 0, 0},
		{"mentioned group upload", mentionGate, "channel", "<@UBOT> look", 1, 1},
		{"no admitter", nil, "channel", "", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &fileStub{resps: map[string]stubResp{"/a/x.pdf": {body: testPDF}}}
			s := newFileSlack(stub)
			if tc.admit != nil {
				s.SetAdmission(tc.admit)
			}
			ev := fileShare(tc.text, slack.File{Name: "x.pdf", URLPrivateDownload: slackURL("/a/x.pdf")})
			ev.ChannelType = tc.channelType
			got := deliver(s, ev)
			if n := len(stub.requests()); n != tc.wantFetch {
				t.Errorf("%d downloads, want %d", n, tc.wantFetch)
			}
			if len(got) != tc.wantHandled {
				t.Errorf("handler called %d times, want %d", len(got), tc.wantHandled)
			}
		})
	}
}

func TestHandleMessage_FileShareStopsOnShutdown(t *testing.T) {
	t.Parallel()
	stub := &fileStub{resps: map[string]stubResp{"/a/x.pdf": {body: testPDF}}}
	s := newFileSlack(stub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	if got := deliver(s, fileShare("hi", slack.File{Name: "x.pdf", URLPrivateDownload: slackURL("/a/x.pdf")})); len(got) != 0 {
		t.Errorf("handler called %d times after shutdown, want 0", len(got))
	}
}

func TestSlackFileName_FallsBackToTitle(t *testing.T) {
	t.Parallel()
	if got := slackFileName(slack.File{Title: "notes.md"}); got != "notes.md" {
		t.Errorf("slackFileName = %q, want the title", got)
	}
	if got := slackFileName(slack.File{Name: "a.txt", Title: "ignored"}); !strings.HasPrefix(got, "a.txt") {
		t.Errorf("slackFileName = %q, want the name", got)
	}
}
