package dispatch

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/turn"
)

const resumeLostNotice = "之前的会话记录已丢失，已开始新会话。"

// TestSessionReady_NoticeOnlyWhenResumeLost: of the four session statuses
// only SessionResumeLost tells the chat its context is gone; the others,
// SessionNew included, post nothing before the answer.
func TestSessionReady_NoticeOnlyWhenResumeLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		st   sessionview.SessionStatus
		want []string
	}{
		{"existing", sessionview.SessionExisting, nil},
		{"resumed", sessionview.SessionResumed, nil},
		{"new", sessionview.SessionNew, nil},
		{"resume_lost", sessionview.SessionResumeLost, []string{resumeLostNotice}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fp := &fakePlatform{supportsInterim: true}
			d := newTestDispatcher(fp)
			fireSessionReady(d, fp.Name(), tc.st)
			if got := fp.allReplies(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("replies = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionReady_ResumeLostNoticeGates: the notice skips a platform without
// interim messages, and still goes out on a turn that is not the owner loop's
// first, since a session can die and lose its transcript between turns.
func TestSessionReady_ResumeLostNoticeGates(t *testing.T) {
	t.Parallel()
	t.Run("no_interim", func(t *testing.T) {
		t.Parallel()
		fp := &fakePlatform{}
		d := newTestDispatcher(fp)
		fireSessionReady(d, fp.Name(), sessionview.SessionResumeLost)
		if got := fp.allReplies(); len(got) != 0 {
			t.Errorf("replies = %q, want none on a platform without interim messages", got)
		}
	})
	t.Run("drain_turn", func(t *testing.T) {
		t.Parallel()
		fp := &fakePlatform{supportsInterim: true}
		d := newTestDispatcher(fp)
		msg := noticeMsg(fp.Name())
		o := d.newIMOrigin(msg, slog.Default(), noticeKey(fp.Name()), "general", session.AgentOpts{}, imMessage, len(msg.Text), 0)
		dl := &imDelivery{o: o, info: turn.TurnInfo{First: false}, p: fp, lg: slog.Default()}
		dl.BeforeSession(context.Background())
		dl.SessionReady(context.Background(), sessionview.SessionResumeLost)
		dl.tracker.stop()
		if got := fp.allReplies(); len(got) != 1 || got[0] != resumeLostNotice {
			t.Errorf("replies = %q, want the resume-lost notice", got)
		}
	})
}
