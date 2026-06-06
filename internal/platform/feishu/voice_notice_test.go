package feishu

import (
	"strings"
	"testing"
)

// TestVoiceNoticeConstants pins R247-ARCH-19 (#631): the voice-pipeline
// user-facing notices are centralised as named constants instead of inline
// literals scattered through the audio handler, so the future i18n catalog
// has a single hook point. Guards that both stay non-empty and bracketed
// (the "[…]" form the dashboard / IM renders as a system notice rather than
// model output) so a refactor can't silently blank them.
func TestVoiceNoticeConstants(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"download":   msgVoiceDownloadFailed,
		"transcribe": msgVoiceTranscribeFailed,
	}
	for name, msg := range cases {
		if msg == "" {
			t.Errorf("%s notice is empty", name)
		}
		if !strings.HasPrefix(msg, "[") || !strings.HasSuffix(msg, "]") {
			t.Errorf("%s notice %q is not bracketed", name, msg)
		}
	}
	if msgVoiceDownloadFailed == msgVoiceTranscribeFailed {
		t.Error("download and transcribe notices must be distinct")
	}
}
