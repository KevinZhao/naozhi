package clievent

// Moved from internal/cli with NewUserMessageWithMeta (#2649 G1-e): the
// assertion type-asserts the unexported inputTextBlock, which stopped being
// reachable across the package boundary. Moving the test beats exporting a
// production type so a foreign package can read one field off it.

import "testing"

// TestNewUserMessage_NoFileRef_ByteIdentical pins the no-regression
// invariant: when no file_ref is present, the wire form must be bit-for-bit
// identical to the pre-PDF code path. Otherwise every image-only send in
// production would subtly change behaviour on rollout.
func TestNewUserMessage_NoFileRef_ByteIdentical(t *testing.T) {
	t.Parallel()
	// Text-only
	text := NewUserMessageWithMeta("just text", nil, "", "")
	s, ok := text.Message.Content.(string)
	if !ok || s != "just text" {
		t.Errorf("text-only content changed: got %T %v", text.Message.Content, text.Message.Content)
	}

	// Image-only
	imgs := []Attachment{{Kind: KindImageInline, Data: []byte("x"), MimeType: "image/png"}}
	m := NewUserMessageWithMeta("look", imgs, "", "")
	blocks, _ := m.Message.Content.([]any)
	if len(blocks) != 2 {
		t.Fatalf("image+text expected 2 blocks, got %d", len(blocks))
	}
	// The text block must carry the user text verbatim, no hint prefix.
	last, _ := blocks[len(blocks)-1].(inputTextBlock)
	if last.Text != "look" {
		t.Errorf("image-only text block leaked hint prefix: %q", last.Text)
	}
}

// TestAttachmentSuffix: summaries count images and file refs separately, so a
// PDF or IM file is not announced as an image.
func TestAttachmentSuffix(t *testing.T) {
	t.Parallel()
	img := Attachment{Data: []byte("x"), MimeType: "image/png"}
	ref := Attachment{Kind: KindFileRef, MimeType: "application/pdf", WorkspacePath: "a.pdf"}
	cases := []struct {
		atts []Attachment
		want string
	}{
		{nil, ""},
		{[]Attachment{img, img}, " [+2 image(s)]"},
		{[]Attachment{ref}, " [+1 file(s)]"},
		{[]Attachment{img, ref, ref}, " [+1 image(s)] [+2 file(s)]"},
	}
	for _, c := range cases {
		if got := AttachmentSuffix(c.atts); got != c.want {
			t.Errorf("AttachmentSuffix(%d atts) = %q, want %q", len(c.atts), got, c.want)
		}
	}
}
