package discord

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// cdnObject is one canned CDN response; status 0 means 200.
type cdnObject struct {
	status   int
	body     []byte
	location string
}

// cdnFiles serves cdnObjects by URL path and records every request path.
// block, when set, holds each request until its context ends.
type cdnFiles struct {
	objects map[string]cdnObject
	block   chan struct{}

	mu   sync.Mutex
	hits []string
}

func (c *cdnFiles) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.hits = append(c.hits, req.URL.Path)
	c.mu.Unlock()
	if c.block != nil {
		close(c.block)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	obj, ok := c.objects[req.URL.Path]
	if !ok {
		obj.status = http.StatusNotFound
	}
	if obj.status == 0 {
		obj.status = http.StatusOK
	}
	h := http.Header{}
	if obj.location != "" {
		h.Set("Location", obj.location)
	}
	return &http.Response{
		StatusCode: obj.status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(obj.body)),
		Request:    req,
	}, nil
}

func (c *cdnFiles) hitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.hits)
}

// filesAdapter returns an adapter whose downloads go through a copy of the
// production CDN client, so its redirect policy runs, with cdn as transport.
func filesAdapter(cdn *cdnFiles) (*Discord, func() []platform.IncomingMessage) {
	d := New(Config{BotToken: "test-token"})
	client := *discordHTTPClient
	client.Transport = cdn
	d.cdnHTTP = &client
	setTestBotID(d, "bot123")
	var mu sync.Mutex
	var handled []platform.IncomingMessage
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		handled = append(handled, msg)
		mu.Unlock()
	}
	return d, func() []platform.IncomingMessage {
		d.dispatch.Wait()
		mu.Lock()
		defer mu.Unlock()
		return handled
	}
}

func cdnPath(name string) string { return "/attachments/1/2/" + name }

func cdnAtt(name, contentType string, size int) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{
		Filename:    name,
		ContentType: contentType,
		Size:        size,
		URL:         "https://cdn.discordapp.com" + cdnPath(name),
	}
}

func postDM(d *Discord, content string, atts ...*discordgo.MessageAttachment) {
	d.onMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "m1", Author: &discordgo.User{ID: "u1"}, ChannelID: "dm1",
		Content: content, Attachments: atts,
	}})
}

var (
	testPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	testPDF = []byte("%PDF-1.7\n%%EOF\n")
)

// sized pads prefix with 'a' bytes to n bytes in total.
func sized(prefix []byte, n int) []byte {
	return append(append([]byte{}, prefix...), bytes.Repeat([]byte("a"), n-len(prefix))...)
}

// fileSummary renders msg.Files as "name=reject/len" for compact comparison.
func fileSummary(files []platform.File) string {
	var parts []string
	for _, f := range files {
		r := string(f.Reject)
		if r == "" {
			r = "ok"
		}
		parts = append(parts, f.Name+"="+r+"/"+strconv.Itoa(len(f.Data)))
	}
	return strings.Join(parts, " ")
}

// TestOnMessageCreate_DeliversFiles: a document reaches msg.Files with its
// bytes and an image msg.Images, while an unsupported type is named in
// msg.Files without being downloaded.
func TestOnMessageCreate_DeliversFiles(t *testing.T) {
	t.Parallel()
	cdn := &cdnFiles{objects: map[string]cdnObject{
		cdnPath("report.pdf"): {body: testPDF},
		cdnPath("shot.png"):   {body: testPNG},
	}}
	d, handled := filesAdapter(cdn)
	postDM(d, "look",
		cdnAtt("report.pdf", "application/pdf", len(testPDF)),
		cdnAtt("shot.png", "image/png", len(testPNG)),
		cdnAtt("clip.mp4", "video/mp4", 1000))
	got := handled()
	if len(got) != 1 {
		t.Fatalf("handler calls = %d, want 1", len(got))
	}
	msg := got[0]
	if msg.Text != "look" {
		t.Errorf("text = %q, want look", msg.Text)
	}
	if want := "report.pdf=ok/15 clip.mp4=unsupported/0"; fileSummary(msg.Files) != want {
		t.Errorf("files = %q, want %q", fileSummary(msg.Files), want)
	}
	if !bytes.Equal(msg.Files[0].Data, testPDF) {
		t.Errorf("report.pdf bytes = %q, want the CDN body", msg.Files[0].Data)
	}
	if len(msg.Images) != 1 || msg.Images[0].MimeType != "image/png" {
		t.Errorf("images = %+v, want one image/png", msg.Images)
	}
	if n := cdn.hitCount(); n != 2 {
		t.Errorf("CDN downloads = %d (%v), want 2: clip.mp4 must not be fetched", n, cdn.hits)
	}
}

// TestOnMessageCreate_AttachmentOnly: a message with no text still reaches
// the handler when it carries a file, even one that is refused, so the user
// is told rather than ignored.
func TestOnMessageCreate_AttachmentOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		att  *discordgo.MessageAttachment
		want string
	}{
		{"text file", cdnAtt("notes.txt", "text/plain; charset=utf-8", 2), "notes.txt=ok/2"},
		{"unsupported file", cdnAtt("a.zip", "application/zip", 10), "a.zip=unsupported/0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdn := &cdnFiles{objects: map[string]cdnObject{cdnPath("notes.txt"): {body: []byte("hi")}}}
			d, handled := filesAdapter(cdn)
			postDM(d, "", tc.att)
			got := handled()
			if len(got) != 1 {
				t.Fatalf("handler calls = %d, want 1", len(got))
			}
			if s := fileSummary(got[0].Files); s != tc.want {
				t.Errorf("files = %q, want %q", s, tc.want)
			}
		})
	}
}

// TestAttachFiles_Rejects covers each way one attachment fails to arrive:
// every one becomes a File naming its reason, and none is truncated.
func TestAttachFiles_Rejects(t *testing.T) {
	t.Parallel()
	const imgCap, fileCap = maxDiscordImageBytes, limits.MaxFileAttachmentBytes
	cases := []struct {
		name     string
		att      *discordgo.MessageAttachment
		obj      cdnObject
		want     string
		wantHits int
		wantImgs int
	}{
		{"image declared over cap", cdnAtt("big.png", "image/png", imgCap+1),
			cdnObject{body: testPNG}, "big.png=too_large/0", 0, 0},
		{"file declared over cap", cdnAtt("big.pdf", "application/pdf", fileCap+1),
			cdnObject{body: testPDF}, "big.pdf=too_large/0", 0, 0},
		{"image body over cap is not truncated", cdnAtt("big.png", "image/png", 10),
			cdnObject{body: sized(testPNG, imgCap+1)}, "big.png=too_large/0", 1, 0},
		{"file body over cap", cdnAtt("big.txt", "text/plain", 10),
			cdnObject{body: sized(nil, fileCap+1)}, "big.txt=too_large/0", 1, 0},
		{"image at cap", cdnAtt("cap.png", "image/png", imgCap),
			cdnObject{body: sized(testPNG, imgCap)}, "", 1, 1},
		{"CDN error", cdnAtt("gone.pdf", "application/pdf", 10),
			cdnObject{status: http.StatusNotFound}, "gone.pdf=download_failed/0", 1, 0},
		{"redirect not followed", cdnAtt("r.pdf", "application/pdf", 10),
			cdnObject{status: http.StatusFound, location: "http://169.254.169.254/"}, "r.pdf=download_failed/0", 1, 0},
		{"image whose bytes are not an image", cdnAtt("fake.png", "image/png", 10),
			cdnObject{body: testPDF}, "fake.png=unsupported/0", 1, 0},
		{"empty image", cdnAtt("empty.png", "image/png", 0),
			cdnObject{}, "empty.png=unsupported/0", 1, 0},
		{"host off the CDN allowlist", &discordgo.MessageAttachment{Filename: "x.pdf",
			ContentType: "application/pdf", Size: 10, URL: "https://evil.example.com" + cdnPath("x.pdf")},
			cdnObject{body: testPDF}, "x.pdf=download_failed/0", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdn := &cdnFiles{objects: map[string]cdnObject{cdnPath(tc.att.Filename): tc.obj}}
			d, handled := filesAdapter(cdn)
			postDM(d, "", tc.att)
			got := handled()
			if len(got) != 1 {
				t.Fatalf("handler calls = %d, want 1", len(got))
			}
			if s := fileSummary(got[0].Files); s != tc.want {
				t.Errorf("files = %q, want %q", s, tc.want)
			}
			if len(got[0].Images) != tc.wantImgs {
				t.Errorf("images = %d, want %d", len(got[0].Images), tc.wantImgs)
			}
			if n := cdn.hitCount(); n != tc.wantHits {
				t.Errorf("CDN downloads = %d, want %d", n, tc.wantHits)
			}
		})
	}
}

// TestAttachFiles_MessageCaps covers the per-message count and the aggregate
// byte cap that images and files share.
func TestAttachFiles_MessageCaps(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	txt := func(name string, size int) (*discordgo.MessageAttachment, cdnObject) {
		return cdnAtt(name, "text/plain", size), cdnObject{body: sized(nil, size)}
	}
	type upload struct {
		att *discordgo.MessageAttachment
		obj cdnObject
	}
	up := func(a *discordgo.MessageAttachment, o cdnObject) upload { return upload{a, o} }
	cases := []struct {
		name     string
		uploads  []upload
		want     string
		wantHits int
		wantImgs int
	}{
		{"count cap skips undownloadable types", []upload{
			up(cdnAtt("clip.mp4", "video/mp4", 10), cdnObject{}),
			up(txt("1.txt", 1)), up(txt("2.txt", 1)), up(txt("3.txt", 1)),
			up(cdnAtt("4.png", "image/png", len(testPNG)), cdnObject{body: testPNG}),
			up(txt("5.txt", 1)), up(txt("6.txt", 1)),
		}, "clip.mp4=unsupported/0 1.txt=ok/1 2.txt=ok/1 3.txt=ok/1 5.txt=ok/1 6.txt=too_many/0", 5, 1},
		{"declared size over the remaining total", []upload{
			up(txt("a.txt", 20*mib)), up(txt("b.txt", 20*mib)), up(txt("c.txt", 12*mib)),
		}, "a.txt=ok/20971520 b.txt=total_too_large/0 c.txt=ok/12582912", 2, 0},
		{"understated size is cut at the remaining total", []upload{
			up(txt("a.txt", 20*mib)),
			up(cdnAtt("b.txt", "text/plain", 1), cdnObject{body: sized(nil, 12*mib+1)}),
		}, "a.txt=ok/20971520 b.txt=total_too_large/0", 2, 0},
		{"images count toward the total", []upload{
			up(cdnAtt("shot.png", "image/png", 10*mib), cdnObject{body: sized(testPNG, 10*mib)}),
			up(txt("a.txt", 22*mib+1)),
		}, "a.txt=total_too_large/0", 1, 1},
		{"rejected image frees its share", []upload{
			up(cdnAtt("fake.png", "image/png", 10*mib), cdnObject{body: sized(testPDF, 10*mib)}),
			up(txt("b.txt", 22*mib+1)),
		}, "fake.png=unsupported/0 b.txt=ok/23068673", 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdn := &cdnFiles{objects: map[string]cdnObject{}}
			var atts []*discordgo.MessageAttachment
			for _, u := range tc.uploads {
				cdn.objects[cdnPath(u.att.Filename)] = u.obj
				atts = append(atts, u.att)
			}
			d, handled := filesAdapter(cdn)
			postDM(d, "", atts...)
			got := handled()
			if len(got) != 1 {
				t.Fatalf("handler calls = %d, want 1", len(got))
			}
			if s := fileSummary(got[0].Files); s != tc.want {
				t.Errorf("files = %q, want %q", s, tc.want)
			}
			if len(got[0].Images) != tc.wantImgs {
				t.Errorf("images = %d, want %d", len(got[0].Images), tc.wantImgs)
			}
			if n := cdn.hitCount(); n != tc.wantHits {
				t.Errorf("CDN downloads = %d (%v), want %d", n, cdn.hits, tc.wantHits)
			}
		})
	}
}

// TestOnMessageCreate_StopDuringDownloadForwardsNothing: Stop cancels an
// in-flight download, and the half-attached message is dropped.
func TestOnMessageCreate_StopDuringDownloadForwardsNothing(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cdn := &cdnFiles{block: started}
	d, handled := filesAdapter(cdn)
	d.stopCtx, d.stopCancel = context.WithCancel(context.Background())
	postDM(d, "read this", cdnAtt("a.pdf", "application/pdf", 10))
	<-started
	d.stopCancel()
	if got := handled(); len(got) != 0 {
		t.Fatalf("handler calls = %d, want 0 after Stop", len(got))
	}
}
