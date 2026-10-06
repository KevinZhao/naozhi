package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/naozhi/naozhi/internal/platform"
)

// topicCases: only a message with a thread_id is in a topic; the topic is
// named by its root message, which is the message itself when it opens the
// topic. A quote reply has a root but no thread and stays in the chat. A
// message outside any topic would open one under itself (wantSelf).
var topicCases = []struct {
	name, threadID, rootID, want, wantSelf string
}{
	{"topic reply", "omt_1", "om_root", "om_root", ""},
	{"topic opener", "omt_1", "", "om_self", ""},
	{"quote reply", "", "om_root", "", "om_self"},
	{"plain message", "", "", "", "om_self"},
}

func TestTopicRef(t *testing.T) {
	t.Parallel()
	for _, tc := range topicCases {
		if got := topicRef(tc.threadID, tc.rootID, "om_self"); got != tc.want {
			t.Errorf("%s: topicRef = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := topicRef("omt_1", "om_"+strings.Repeat("x", maxTopicRefLen), "om_self"); got != "" {
		t.Errorf("over-long root: topicRef = %q, want dropped", got)
	}
}

func TestSelfTopicRef(t *testing.T) {
	t.Parallel()
	for _, tc := range topicCases {
		if got := selfTopicRef(tc.threadID, "om_self"); got != tc.wantSelf {
			t.Errorf("%s: selfTopicRef = %q, want %q", tc.name, got, tc.wantSelf)
		}
	}
	if got := selfTopicRef("", "om_"+strings.Repeat("x", maxTopicRefLen)); got != "" {
		t.Errorf("over-long message id: selfTopicRef = %q, want dropped", got)
	}
}

func TestParseSDKEvent_Topic(t *testing.T) {
	t.Parallel()
	for _, tc := range topicCases {
		m := &larkim.EventMessage{
			MessageId:   strPtr("om_self"),
			MessageType: strPtr("text"),
			ChatId:      strPtr("oc_1"),
			ChatType:    strPtr("group"),
			Content:     strPtr(`{"text":"hi"}`),
		}
		if tc.threadID != "" {
			m.ThreadId = strPtr(tc.threadID)
		}
		if tc.rootID != "" {
			m.RootId = strPtr(tc.rootID)
		}
		pe, ok := (&Feishu{}).parseSDKEvent(&larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{Message: m}})
		if !ok || pe.Msg.ThreadID != tc.want || pe.Msg.SelfThread != tc.wantSelf {
			t.Errorf("%s: parsed %+v, %v; want ThreadID %q, SelfThread %q", tc.name, pe.Msg, ok, tc.want, tc.wantSelf)
		}
	}
}

func TestWebhook_Topic(t *testing.T) {
	t.Parallel()
	for _, tc := range topicCases {
		f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
		got := make(chan platform.IncomingMessage, 1)
		mux := http.NewServeMux()
		f.registerWebhook(mux, func(_ context.Context, msg platform.IncomingMessage) { got <- msg })
		message := map[string]any{
			"message_id": "om_self", "chat_id": "oc_1", "chat_type": "group",
			"message_type": "text", "content": `{"text":"hi"}`,
		}
		if tc.threadID != "" {
			message["thread_id"] = tc.threadID
		}
		if tc.rootID != "" {
			message["root_id"] = tc.rootID
		}
		body, _ := json.Marshal(map[string]any{
			"schema": "2.0",
			"header": map[string]any{"event_id": "ev_" + tc.name, "event_type": "im.message.receive_v1", "token": "test_token"},
			"event":  map[string]any{"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_1"}}, "message": message},
		})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(body))
		select {
		case m := <-got:
			if m.ThreadID != tc.want || m.SelfThread != tc.wantSelf {
				t.Errorf("%s: ThreadID, SelfThread = %q, %q; want %q, %q", tc.name, m.ThreadID, m.SelfThread, tc.want, tc.wantSelf)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no message dispatched (status %d)", tc.name, w.Code)
		}
		_ = f.Stop()
	}
}

// sendRecorder is a Feishu Open API stub that records every message POST and
// answers a topic reply with replyCode.
type sendRecorder struct {
	mu        sync.Mutex
	posts     []sentPost
	replyCode int
}

type sentPost struct {
	path, query string
	body        map[string]any
}

func newSendFeishu(t *testing.T, replyCode int) (*Feishu, *sendRecorder) {
	t.Helper()
	rec := &sendRecorder{replyCode: replyCode}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
			return
		case r.URL.Path == "/open-apis/im/v1/images":
			_, _ = w.Write([]byte(`{"code":0,"data":{"image_key":"img_1"}}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rec.mu.Lock()
		rec.posts = append(rec.posts, sentPost{path: r.URL.Path, query: r.URL.RawQuery, body: body})
		rec.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/reply") && rec.replyCode != 0 {
			_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(rec.replyCode) + `,"msg":"refused"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_new"}}`))
	}))
	t.Cleanup(srv.Close)
	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	t.Cleanup(func() { _ = f.Stop() })
	f.baseURL = srv.URL
	return f, rec
}

func (r *sendRecorder) all() []sentPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentPost(nil), r.posts...)
}

// TestReply_InTopic: text, images and the question card of a message in a
// topic are replies to the topic's root with reply_in_thread; outside a
// topic they are created in the chat.
func TestReply_InTopic(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{ThreadID: "om_root", Items: []platform.QuestionItem{{
		Question: "Q", Options: []platform.QuestionOption{{Label: "A"}},
	}}}
	for _, thread := range []string{"om_root", ""} {
		f, rec := newSendFeishu(t, 0)
		if _, err := f.Reply(context.Background(), platform.OutgoingMessage{
			ChatID: "oc_1", ThreadID: thread, Text: "hi",
			Images: []platform.Image{{Data: []byte("png"), MimeType: "image/png"}},
		}); err != nil {
			t.Fatal(err)
		}
		card.ThreadID = thread
		if _, err := f.SendQuestionCard(context.Background(), "oc_1", card); err != nil {
			t.Fatal(err)
		}
		posts := rec.all()
		if len(posts) != 3 {
			t.Fatalf("thread %q: %d posts %+v, want text, image, card", thread, len(posts), posts)
		}
		for i, p := range posts {
			if thread != "" {
				if p.path != "/open-apis/im/v1/messages/om_root/reply" || p.body["reply_in_thread"] != true || p.body["receive_id"] != nil {
					t.Errorf("post %d in topic = %+v, want a reply_in_thread reply to om_root", i, p)
				}
			} else if p.path != "/open-apis/im/v1/messages" || p.query != "receive_id_type=chat_id" || p.body["receive_id"] != "oc_1" {
				t.Errorf("post %d outside a topic = %+v, want a create in oc_1", i, p)
			}
		}
		if posts[0].body["msg_type"] != "interactive" || posts[1].body["msg_type"] != "image" {
			t.Errorf("thread %q: msg types %v, %v", thread, posts[0].body["msg_type"], posts[1].body["msg_type"])
		}
	}
}

// TestReply_TopicRefusedFallsBackToChat: a topic reply Feishu refuses (root
// recalled, no permission) is sent to the chat instead; a refused token or
// a rate limit is returned for ReplyWithRetry and not sent anywhere else.
func TestReply_TopicRefusedFallsBackToChat(t *testing.T) {
	t.Parallel()
	f, rec := newSendFeishu(t, 230011)
	id, err := f.Reply(context.Background(), platform.OutgoingMessage{ChatID: "oc_1", ThreadID: "om_root", Text: "hi"})
	if err != nil || id != "om_new" {
		t.Fatalf("Reply = %q, %v; want the chat send's id", id, err)
	}
	if posts := rec.all(); len(posts) != 2 || !strings.HasSuffix(posts[0].path, "/reply") || posts[1].body["receive_id"] != "oc_1" {
		t.Errorf("posts = %+v, want the refused reply then a create in oc_1", posts)
	}

	f, rec = newSendFeishu(t, 99991671)
	if _, err := f.Reply(context.Background(), platform.OutgoingMessage{ChatID: "oc_1", ThreadID: "om_root", Text: "hi"}); !platform.IsTokenInvalidated(err) {
		t.Errorf("token refusal err = %v, want a token-invalidated error", err)
	}
	if posts := rec.all(); len(posts) != 1 {
		t.Errorf("token refusal posts = %+v, want only the reply", posts)
	}

	for _, code := range []int{99991400, 11232, 11233, 11234, 230020} {
		f, rec = newSendFeishu(t, code)
		_, err := f.Reply(context.Background(), platform.OutgoingMessage{ChatID: "oc_1", ThreadID: "om_root", Text: "hi"})
		var api *APIError
		if !errors.As(err, &api) || api.Code != code {
			t.Errorf("rate limit %d: err = %v, want the APIError", code, err)
		}
		if posts := rec.all(); len(posts) != 1 {
			t.Errorf("rate limit %d: posts = %+v, want only the reply", code, posts)
		}
	}
}

// TestDispatchCardAction_Topic: the answer to a card in a topic carries the
// topic from the card value, accepted only in message-id shape.
func TestDispatchCardAction_Topic(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"om_root1":                       "om_root1",
		"":                               "",
		"om_../../chats":                 "",
		"oc_chat":                        "",
		"om_" + strings.Repeat("a", 200): "",
	} {
		var got platform.IncomingMessage
		val := platform.AskAnswerPayload{Kind: platform.AskAnswerKind, Label: "A", ThreadID: in}
		(&Feishu{}).dispatchCardAction(context.Background(), val, "oc_1", "", "group", "ou_1",
			func(_ context.Context, m platform.IncomingMessage) { got = m })
		if got.ThreadID != want {
			t.Errorf("value thread %q: ThreadID = %q, want %q", in, got.ThreadID, want)
		}
	}
}

func TestBuildQuestionCardJSON_CarriesTopic(t *testing.T) {
	t.Parallel()
	body, err := buildQuestionCardJSON(platform.QuestionCard{ThreadID: "om_root", Items: []platform.QuestionItem{{
		Question: "Q", Options: []platform.QuestionOption{{Label: "A"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"thread_id":"om_root"`) {
		t.Errorf("card value lacks the topic: %s", body)
	}
}

// TestReplyError_InTopic: a voice-failure notice for a message in a topic
// is posted in that topic.
func TestReplyError_InTopic(t *testing.T) {
	t.Parallel()
	f, rec := newSendFeishu(t, 0)
	f.replyError(context.Background(), platform.IncomingMessage{ChatID: "oc_1", ThreadID: "om_root"}, msgVoiceDownloadFailed)
	if posts := rec.all(); len(posts) != 1 || posts[0].path != "/open-apis/im/v1/messages/om_root/reply" {
		t.Errorf("posts = %+v, want one reply in om_root's topic", posts)
	}
}
