package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

func pendingRef(name, mime string, data []byte) clievent.Attachment {
	return clievent.Attachment{Kind: clievent.KindFileRef, Data: data, MimeType: mime, OrigName: name, Size: int64(len(data))}
}

// TestTurnSender_PersistsPendingFileRefsInSessionWorkspace: an IM file ref
// reaches the CLI as a path inside the session's own cwd with no bytes left
// in the attachment, on the serialized and the passthrough path alike, and
// the hint the CLI builds from it names that path.
func TestTurnSender_PersistsPendingFileRefsInSessionWorkspace(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
		t.Cleanup(r.Shutdown)
		const key = "feishu:direct:files:general"
		ws := t.TempDir()
		var gotText string
		var got []clievent.Attachment
		proc := session.NewTestProcess()
		proc.PassthroughVal = passthrough
		proc.SendFunc = func(_ context.Context, text string, atts []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
			gotText, got = text, atts
			return &clievent.SendResult{Text: "ok"}, nil
		}
		proc.SendPassthroughFunc = func(_ context.Context, text string, atts []clievent.Attachment, _ clievent.EventCallback, _ string) (*clievent.SendResult, error) {
			gotText, got = text, atts
			return &clievent.SendResult{Text: "ok"}, nil
		}
		sess := r.InjectSession(key, proc)
		sess.SetWorkspaceForTest(ws)

		body := []byte("第一行\n")
		img := clievent.Attachment{Data: []byte("png"), MimeType: "image/png"}
		in := []clievent.Attachment{img, pendingRef("notes.txt", "text/plain", body)}
		s := turnSender{router: r, notify: &recordingNotifier{}}
		if _, err := s.Send(context.Background(), key, sess, "读一下", in, turn.SendSpec{Passthrough: passthrough}, nil); err != nil {
			t.Fatal(err)
		}
		if gotText != "读一下" {
			t.Errorf("passthrough=%v: text = %q, want it unchanged", passthrough, gotText)
		}
		if len(got) != 2 || !bytes.Equal(got[0].Data, img.Data) {
			t.Fatalf("passthrough=%v: attachments = %+v, want the image then the ref", passthrough, got)
		}
		ref := got[1]
		if ref.Data != nil || ref.Kind != clievent.KindFileRef || !strings.HasSuffix(ref.WorkspacePath, ".txt") {
			t.Fatalf("passthrough=%v: ref = {Kind %q, Path %q, %d bytes}, want a persisted .txt ref without Data", passthrough, ref.Kind, ref.WorkspacePath, len(ref.Data))
		}
		if ref.OrigName != "notes.txt" || ref.Size != int64(len(body)) {
			t.Errorf("passthrough=%v: OrigName/Size = %q/%d", passthrough, ref.OrigName, ref.Size)
		}
		onDisk, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(ref.WorkspacePath)))
		if err != nil || !bytes.Equal(onDisk, body) {
			t.Errorf("passthrough=%v: file under the session workspace = %q, %v; want the sent bytes", passthrough, onDisk, err)
		}
		if in[1].WorkspacePath != "" || in[1].Data == nil {
			t.Error("the caller's slice was mutated")
		}
		wire, err := json.Marshal(clievent.NewUserMessageWithMeta(gotText, got, "", ""))
		if err != nil || !strings.Contains(string(wire), "Read tool") || !strings.Contains(string(wire), ref.WorkspacePath) {
			t.Errorf("passthrough=%v: stdin message %s does not hint at %s (err %v)", passthrough, wire, ref.WorkspacePath, err)
		}
	}
}

// TestPersistPendingFileRefs_LeavesPersistedRefsAlone: a ref that already has
// a path (a dashboard upload), or has no bytes to write, is not pending; with
// nothing pending the slice comes back as is and nothing is written.
func TestPersistPendingFileRefs_LeavesPersistedRefsAlone(t *testing.T) {
	ws := t.TempDir()
	in := []clievent.Attachment{
		{Kind: clievent.KindFileRef, MimeType: "application/pdf", WorkspacePath: ".naozhi/attachments/2026-01-01/x.pdf"},
		{Kind: clievent.KindFileRef, MimeType: "application/pdf", WorkspacePath: ".naozhi/attachments/2026-01-01/y.pdf", Data: []byte("%PDF-")},
		{Kind: clievent.KindFileRef, MimeType: "application/pdf"},
	}
	out, note := persistPendingFileRefs(ws, in, "k")
	if note != "" || len(out) != len(in) || &out[0] != &in[0] {
		t.Errorf("out=%+v note=%q, want the input slice back and no note", out, note)
	}
	if _, err := os.Stat(filepath.Join(ws, attachment.Dir)); !os.IsNotExist(err) {
		t.Errorf("attachment dir exists (err %v), want nothing written", err)
	}
}

// TestPersistPendingFileRefs_FailureDegradesToNote: a ref that cannot be
// written is dropped and named in a note for the model, and the turn keeps
// its other attachments.
func TestPersistPendingFileRefs_FailureDegradesToNote(t *testing.T) {
	good := pendingRef("ok.md", "text/markdown", []byte("# ok\n"))
	cases := []struct {
		name      string
		workspace func(t *testing.T) string
		atts      []clievent.Attachment
		wantKept  int
		wantLost  []string
	}{
		{"missing workspace", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			[]clievent.Attachment{good}, 0, []string{"ok.md"}},
		{"relative workspace", func(*testing.T) string { return "rel/ws" },
			[]clievent.Attachment{good}, 0, []string{"ok.md"}},
		{"workspace is a file", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, []clievent.Attachment{good}, 0, []string{"ok.md"}},
		{"unmapped mime among good ones", func(t *testing.T) string { return t.TempDir() },
			[]clievent.Attachment{good, pendingRef("", "application/x-msdownload", []byte("MZ"))}, 1, []string{"a file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := c.workspace(t)
			out, note := persistPendingFileRefs(ws, c.atts, "k")
			if len(out) != c.wantKept {
				t.Errorf("kept %d refs, want %d", len(out), c.wantKept)
			}
			for _, a := range out {
				if a.WorkspacePath == "" || a.Data != nil {
					t.Errorf("kept ref %+v is not persisted", a)
				}
			}
			for _, name := range c.wantLost {
				if !strings.Contains(note, name) {
					t.Errorf("note = %q, want it to name %q", note, name)
				}
			}
			if !strings.HasPrefix(note, "[System: ") || !strings.HasSuffix(note, "]\n\n") {
				t.Errorf("note = %q, want a [System: ...] line ahead of the user text", note)
			}
			if c.name == "missing workspace" {
				if _, err := os.Stat(ws); !os.IsNotExist(err) {
					t.Errorf("a missing workspace was created (err %v)", err)
				}
			}
		})
	}
}

// TestTurnSender_FileNotePrecedesUserText: the note reaches the CLI ahead of
// what the user wrote.
func TestTurnSender_FileNotePrecedesUserText(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "feishu:direct:filenote:general"
	var gotText string
	var got []clievent.Attachment
	proc := session.NewTestProcess()
	proc.SendFunc = func(_ context.Context, text string, atts []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
		gotText, got = text, atts
		return &clievent.SendResult{Text: "ok"}, nil
	}
	sess := r.InjectSession(key, proc)
	sess.SetWorkspaceForTest(filepath.Join(t.TempDir(), "gone"))
	s := turnSender{router: r, notify: &recordingNotifier{}}
	if _, err := s.Send(context.Background(), key, sess, "看附件", []clievent.Attachment{pendingRef("a.pdf", "application/pdf", []byte("%PDF-1.7"))}, turn.SendSpec{}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotText, "[System: ") || !strings.Contains(gotText, "a.pdf") || !strings.HasSuffix(gotText, "]\n\n看附件") {
		t.Errorf("text = %q, want the note then the user text", gotText)
	}
	if len(got) != 0 {
		t.Errorf("attachments = %+v, want the lost ref dropped", got)
	}
}
