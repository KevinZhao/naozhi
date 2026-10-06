package dispatch

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

var testPDF = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")

func fileMsg(id, text string, files ...platform.File) platform.IncomingMessage {
	m := incomingMsg(text)
	m.EventID = "evt-files-" + id
	m.Files = files
	return m
}

func TestPrepareInbound_FileOnlyBecomesPendingFileRef(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	p, ok := d.prepareInbound(context.Background(), fileMsg("pdf", "", platform.File{Name: "合同\u202e.pdf", Data: testPDF}))
	if !ok {
		t.Fatal("a file-only message was dropped")
	}
	if len(p.images) != 1 {
		t.Fatalf("attachments = %d, want 1", len(p.images))
	}
	a := p.images[0]
	if a.Kind != clievent.KindFileRef || a.MimeType != "application/pdf" || a.WorkspacePath != "" {
		t.Errorf("attachment = {Kind %q, Mime %q, Path %q}, want a pending application/pdf file_ref", a.Kind, a.MimeType, a.WorkspacePath)
	}
	if !bytes.Equal(a.Data, testPDF) || a.Size != int64(len(testPDF)) {
		t.Errorf("Data/Size not carried: len %d size %d", len(a.Data), a.Size)
	}
	if a.OrigName != "合同.pdf" {
		t.Errorf("OrigName = %q, want the sanitized name", a.OrigName)
	}
	if n := fp.replyCount(); n != 0 {
		t.Errorf("replies = %v, want none for an accepted file", fp.allReplies())
	}
}

func TestPrepareInbound_ImagesPrecedeFiles(t *testing.T) {
	t.Parallel()
	d := newTestDispatcher(&fakePlatform{})
	msg := fileMsg("mixed", "看看", platform.File{Name: "a.md", Data: []byte("# a\n")})
	msg.Images = []platform.Image{{Data: []byte("png"), MimeType: "image/png"}}
	p, ok := d.prepareInbound(context.Background(), msg)
	if !ok || len(p.images) != 2 {
		t.Fatalf("ok=%v attachments=%d, want accepted with 2", ok, len(p.images))
	}
	if p.images[0].Kind == clievent.KindFileRef || p.images[1].Kind != clievent.KindFileRef {
		t.Errorf("kinds = [%q %q], want the image first, then the file_ref", p.images[0].Kind, p.images[1].Kind)
	}
	if p.images[1].MimeType != "text/markdown" {
		t.Errorf("file mime = %q", p.images[1].MimeType)
	}
}

func TestPrepareInbound_RejectedFileOnlyRepliesAndStops(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	before := d.messageCount.Load()
	if _, ok := d.prepareInbound(context.Background(), fileMsg("docx", "", platform.File{Name: "plan.docx", Data: []byte("PK\x03\x04...")})); ok {
		t.Fatal("a message whose only file was rejected was submitted")
	}
	got := fp.allReplies()
	if len(got) != 1 || !strings.Contains(got[0], "plan.docx") || !strings.Contains(got[0], fileReasonUnsupported) {
		t.Fatalf("replies = %q, want one notice naming plan.docx as unsupported", got)
	}
	if d.messageCount.Load() != before {
		t.Error("a fully rejected message was counted as accepted")
	}
}

func TestPrepareInbound_TextWithRejectedFileStillSubmits(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	p, ok := d.prepareInbound(context.Background(), fileMsg("text", "总结一下", platform.File{Name: "x.pdf", Reject: platform.FileRejectDownloadFailed}))
	if !ok {
		t.Fatal("the text part was dropped along with the failed file")
	}
	if p.cleanText != "总结一下" || len(p.images) != 0 {
		t.Errorf("cleanText=%q attachments=%d, want the text and no attachment", p.cleanText, len(p.images))
	}
	if got := fp.lastReply(); !strings.Contains(got, "x.pdf") || !strings.Contains(got, fileReasonDownload) {
		t.Errorf("notice = %q, want x.pdf with the download-failed reason", got)
	}
}

func TestPrepareInbound_FileInGroupNeedsMention(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	msg := fileMsg("group", "", platform.File{Name: "a.pdf", Data: testPDF})
	msg.ChatType = "group"
	if _, ok := d.prepareInbound(context.Background(), msg); ok {
		t.Fatal("an un-mentioned group file was submitted")
	}
	if n := fp.replyCount(); n != 0 {
		t.Errorf("un-mentioned group chatter got %d replies, want a silent drop", n)
	}
}

func TestFileAttachments_Caps(t *testing.T) {
	t.Parallel()
	txt := func(name string) platform.File { return platform.File{Name: name, Data: []byte("ok\n")} }

	t.Run("count", func(t *testing.T) {
		var files []platform.File
		for i := 0; i <= limits.MaxFileAttachmentsPerMessage; i++ {
			files = append(files, txt("f"+string(rune('a'+i))+".txt"))
		}
		acc, notice := fileAttachments(files)
		if len(acc) != limits.MaxFileAttachmentsPerMessage {
			t.Errorf("accepted %d, want %d", len(acc), limits.MaxFileAttachmentsPerMessage)
		}
		last := files[len(files)-1].Name
		if !strings.Contains(notice, last+"："+fileReasonTooMany) {
			t.Errorf("notice = %q, want %s rejected as too many", notice, last)
		}
	})

	t.Run("adapter rejects do not use up the count", func(t *testing.T) {
		files := []platform.File{{Name: "big.pdf", Reject: platform.FileRejectTooLarge}}
		for i := 0; i < limits.MaxFileAttachmentsPerMessage; i++ {
			files = append(files, txt("f"+string(rune('a'+i))+".txt"))
		}
		acc, notice := fileAttachments(files)
		if len(acc) != limits.MaxFileAttachmentsPerMessage {
			t.Errorf("accepted %d, want %d", len(acc), limits.MaxFileAttachmentsPerMessage)
		}
		if !strings.Contains(notice, "big.pdf："+fileReasonTooLarge) || strings.Contains(notice, fileReasonTooMany) {
			t.Errorf("notice = %q, want only big.pdf as too large", notice)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		half := bytes.Repeat([]byte("a"), limits.MaxFileAttachmentBytes/2+1)
		acc, notice := fileAttachments([]platform.File{{Name: "a.txt", Data: half}, {Name: "b.txt", Data: half}})
		if len(acc) != 1 || acc[0].OrigName != "a.txt" {
			t.Errorf("accepted %d, want only a.txt", len(acc))
		}
		if !strings.Contains(notice, "b.txt："+fileReasonTotal) {
			t.Errorf("notice = %q, want b.txt over the aggregate cap", notice)
		}
	})

	t.Run("notice lists a bounded number of files", func(t *testing.T) {
		var files []platform.File
		for i := 0; i < limits.MaxFileAttachmentsPerMessage+3; i++ {
			files = append(files, platform.File{Reject: platform.FileRejectUnsupported})
		}
		_, notice := fileAttachments(files)
		if lines := strings.Count(notice, "\n- "); lines != limits.MaxFileAttachmentsPerMessage+1 {
			t.Errorf("notice has %d entries, want %d listed + 1 summary:\n%s", lines, limits.MaxFileAttachmentsPerMessage+1, notice)
		}
		if !strings.Contains(notice, "另有 3 个文件") || !strings.Contains(notice, "未命名文件") {
			t.Errorf("notice = %q", notice)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		_, notice := fileAttachments([]platform.File{{Name: "e.txt"}})
		if !strings.Contains(notice, "e.txt："+fileReasonEmpty) {
			t.Errorf("notice = %q", notice)
		}
	})
}
