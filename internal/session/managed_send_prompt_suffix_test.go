package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestSend_LastPromptCountsFilesApart: the session list's last-prompt line
// names a file ref as a file, on the serialized and the passthrough path.
func TestSend_LastPromptCountsFilesApart(t *testing.T) {
	t.Parallel()
	atts := []clievent.Attachment{
		{Data: []byte("x"), MimeType: "image/png"},
		{Kind: clievent.KindFileRef, MimeType: "application/pdf", WorkspacePath: "a.pdf"},
	}
	const want = "看 [+1 image(s)] [+1 file(s)]"
	for _, passthrough := range []bool{false, true} {
		r := NewRouter(RouterConfig{MaxProcs: 1})
		t.Cleanup(r.Shutdown)
		proc := NewTestProcess()
		proc.PassthroughVal = passthrough
		s := r.InjectSession("feishu:direct:suffix:general", proc)
		var err error
		if passthrough {
			_, err = s.SendPassthrough(context.Background(), "看", atts, nil, "")
		} else {
			_, err = s.Send(context.Background(), "看", atts, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Snapshot().LastPrompt; got != want {
			t.Errorf("passthrough=%v: LastPrompt = %q, want %q", passthrough, got, want)
		}
	}
}
