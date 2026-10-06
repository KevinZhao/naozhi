package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestHandleMessage_ThreadTs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, threadTs string }{
		{"in a thread", "1700000000.000001"},
		{"top level", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
			s.botID = "U123"
			got := make(chan platform.IncomingMessage, 1)
			s.handler = func(_ context.Context, msg platform.IncomingMessage) { got <- msg }
			s.handleMessage(&slackevents.MessageEvent{
				User: "U456", Channel: "C789", ChannelType: "channel",
				Text: "<@U123> hi", TimeStamp: "1700000000.000200", ThreadTimeStamp: tc.threadTs,
			})
			if m := <-got; m.ThreadID != tc.threadTs {
				t.Errorf("ThreadID = %q, want %q", m.ThreadID, tc.threadTs)
			}
		})
	}
}

// uploadWebAPI answers the three calls of a files upload v2 plus
// chat.postMessage, recording each form.
type uploadWebAPI struct {
	mu    sync.Mutex
	calls []apiCall
	srv   *httptest.Server
}

func newUploadAdapter(t *testing.T) (*Slack, *uploadWebAPI) {
	t.Helper()
	f := &uploadWebAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: method, form: r.PostForm})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"ok": true, "channel": r.PostForm.Get("channel"), "ts": "1700000000.000300"}
		switch method {
		case "files.getUploadURLExternal":
			resp["upload_url"] = f.srv.URL + "/upload"
			resp["file_id"] = "F1"
		case "files.completeUploadExternal":
			resp["files"] = []map[string]any{{"id": "F1"}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.api = slack.New("xoxb-test", slack.OptionHTTPClient(slackHTTPClient), slack.OptionAPIURL(f.srv.URL+"/"))
	return s, f
}

func (f *uploadWebAPI) threadTs(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.form.Get("thread_ts"))
		}
	}
	return out
}

// TestReply_PostsIntoThread: a reply with a ThreadID goes into that thread,
// its text and its images alike; without one it goes to the channel.
func TestReply_PostsIntoThread(t *testing.T) {
	t.Parallel()
	for _, thread := range []string{"1700000000.000001", ""} {
		s, api := newUploadAdapter(t)
		_, err := s.Reply(context.Background(), platform.OutgoingMessage{
			ChatID: "C1", ThreadID: thread, Text: "hi",
			Images: []platform.Image{{Data: []byte("png"), MimeType: "image/png"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := api.threadTs("chat.postMessage"); len(got) != 1 || got[0] != thread {
			t.Errorf("thread %q: chat.postMessage thread_ts = %q", thread, got)
		}
		if got := api.threadTs("files.completeUploadExternal"); len(got) != 1 || got[0] != thread {
			t.Errorf("thread %q: files.completeUploadExternal thread_ts = %q", thread, got)
		}
	}
}

func TestSendQuestionCard_PostsIntoThread(t *testing.T) {
	t.Parallel()
	for _, thread := range []string{"1700000000.000001", ""} {
		s, api := newAPIAdapter(t)
		_, err := s.SendQuestionCard(context.Background(), "C42", platform.QuestionCard{
			ThreadID: thread,
			Items:    []platform.QuestionItem{{Question: "Q", Options: []platform.QuestionOption{{Label: "A"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls := api.recorded(); len(calls) != 1 || calls[0].form.Get("thread_ts") != thread {
			t.Errorf("thread %q: calls = %+v", thread, calls)
		}
	}
}

// TestHandleBlockActions_ThreadFromEnvelope: a click on a card in a thread
// carries the thread, read from the envelope (container first, then the
// message), never from the button value. A top-level card that has replies
// carries its own ts as thread_ts and stays top-level.
func TestHandleBlockActions_ThreadFromEnvelope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, container, message, want string
	}{
		{"container", "17.1", "", "17.1"},
		{"message", "", "17.2", "17.2"},
		{"container wins", "17.1", "17.2", "17.1"},
		{"top level", "", "", ""},
		{"top level with replies", "111.222", "111.222", ""},
		{"top level with replies, message only", "", "111.222", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, msgs := clickAdapter(t)
			val := answer("group")
			val.ThreadID = "forged"
			cb := clickCallback("C1", "U9", askActionIDPrefix+"0", val)
			cb.Container.ThreadTs = tc.container
			cb.Message.ThreadTimestamp = tc.message
			s.handleBlockActions(cb)
			got := msgs()
			if len(got) != 1 || got[0].ThreadID != tc.want {
				t.Errorf("dispatched %+v, want ThreadID %q", got, tc.want)
			}
		})
	}
}
