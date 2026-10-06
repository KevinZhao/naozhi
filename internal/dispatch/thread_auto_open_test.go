package dispatch

import (
	"context"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// topLevelMsg is a group @mention outside any thread, which a reply could
// open thread S<id> under.
func topLevelMsg(id, text string) platform.IncomingMessage {
	m := scopeMsg(id, "group", "u1", "", text)
	m.SelfThread = "S" + id
	return m
}

func withThreadAutoOpen() dispatcherTestOption {
	return func(cfg *testDispatcherConfig) { cfg.ThreadAutoOpen = true }
}

// answerThreads is the ThreadID of every reply fp got, in order.
func answerThreads(fp *fakePlatform) []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	var out []string
	for _, r := range fp.replies {
		out = append(out, r.ThreadID)
	}
	return out
}

// TestThreadAutoOpen_MentionStartsAThread: with thread_auto_open a top-level
// group mention is answered in a new thread under it and, under the thread
// scope, runs on that thread's session, which the follow-up posted in the
// thread joins. Off, the mention keeps the channel's session and top level.
func TestThreadAutoOpen_MentionStartsAThread(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      []dispatcherTestOption
		wantKeys  []string
		wantReply []string
	}{
		{"off", nil,
			[]string{"fake:group:g:general", "fake:group:g#tS1:general"}, []string{"", "S1"}},
		{"thread scope", []dispatcherTestOption{withThreadAutoOpen()},
			[]string{"fake:group:g#tS1:general", "fake:group:g#tS1:general"}, []string{"S1", "S1"}},
		{"chat scope", []dispatcherTestOption{withThreadAutoOpen(), withGroupScope(GroupScopeChat)},
			[]string{"fake:group:g:general", "fake:group:g:general"}, []string{"S1", "S1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &keyRecorder{turned: make(chan struct{}, 2)}
			fp := &fakePlatform{}
			d := newTestDispatcher(fp, append(tc.opts, withSender(rec.sender()))...)
			h := d.BuildHandler()
			h(context.Background(), topLevelMsg("1", "hi"))
			h(context.Background(), scopeMsg("2", "group", "u1", "S1", "and then?"))
			rec.waitTurns(t, 2)

			if turns, _ := rec.got(); !slices.Equal(turns, tc.wantKeys) {
				t.Errorf("turn keys = %q, want %q", turns, tc.wantKeys)
			}
			if got := answerThreads(fp); !slices.Equal(got, tc.wantReply) {
				t.Errorf("answers went to threads %q, want %q", got, tc.wantReply)
			}
		})
	}
}

// TestThreadAutoOpen_CommandsAndDirectChatsStayPut: a slash command is
// answered, and acts, where it was posted; a direct chat never opens a
// thread.
func TestThreadAutoOpen_CommandsAndDirectChatsStayPut(t *testing.T) {
	rec := &keyRecorder{turned: make(chan struct{}, 1)}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withThreadAutoOpen(), withSender(rec.sender()))
	h := d.BuildHandler()
	h(context.Background(), topLevelMsg("1", "/help"))
	h(context.Background(), topLevelMsg("2", "/new"))
	dm := topLevelMsg("3", "hi")
	dm.ChatType = "direct"
	h(context.Background(), dm)
	rec.waitTurns(t, 1)

	turns, resets := rec.got()
	if want := []string{"fake:group:g:general"}; !slices.Equal(resets, want) {
		t.Errorf("/new reset %q, want the channel's session %q", resets, want)
	}
	if want := []string{"fake:direct:g:general"}; !slices.Equal(turns, want) {
		t.Errorf("turn keys = %q, want %q", turns, want)
	}
	for i, th := range answerThreads(fp) {
		if th != "" {
			t.Errorf("reply %d went to thread %q, want the top level", i, th)
		}
	}
	if fp.replyCount() < 3 {
		t.Errorf("got %d replies, want one each for /help, /new and the direct chat", fp.replyCount())
	}
}
