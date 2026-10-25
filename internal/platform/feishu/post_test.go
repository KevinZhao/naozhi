package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// docPost is the received-post sample from Feishu's message content docs,
// trimmed to the element kinds naozhi renders.
const docPost = `{"title":"我是一个标题","content":[` +
	`[{"tag":"text","text":"第一行 :","style":["bold","underline"]},` +
	`{"tag":"a","href":"http://www.feishu.cn","text":"超链接","style":["bold","italic"]},` +
	`{"tag":"at","user_id":"@_user_1","user_name":"","style":[]}],` +
	`[{"tag":"img","image_key":"img_47354fbc-a159-40ed-86ab-2ad0f1acb42g"}],` +
	`[{"tag":"text","text":"第二行:","style":["bold","underline"]},{"tag":"text","text":"文本测试","style":[]}],` +
	`[{"tag":"emotion","emoji_type":"SMILE"}],` +
	`[{"tag":"hr"}],` +
	`[{"tag":"code_block","language":"GO","text":"func main() int64 {\n    return 0\n}"}]]}`

func TestParsePostContent(t *testing.T) {
	t.Parallel()
	text, keys, err := parsePostContent(docPost)
	if err != nil {
		t.Fatalf("parsePostContent: %v", err)
	}
	const wantText = "我是一个标题\n第一行 :超链接 (http://www.feishu.cn)\n\n第二行:文本测试\n\n---\n" +
		"```go\nfunc main() int64 {\n    return 0\n}\n```"
	if text != wantText {
		t.Errorf("text = %q, want %q", text, wantText)
	}
	if want := []string{"img_47354fbc-a159-40ed-86ab-2ad0f1acb42g"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("image keys = %q, want %q", keys, want)
	}
}

// A post with text, an @ of the bot and an inline picture reaches the
// handler as one message on both transports: text without the mention, the
// picture downloaded, MentionMe set.
func TestPostMessage_BothTransports(t *testing.T) {
	t.Parallel()
	const content = `{"title":"","content":[` +
		`[{"tag":"at","user_id":"@_user_1","user_name":"bot"},{"tag":"text","text":" 看下这段代码"}],` +
		`[{"tag":"code_block","language":"Go","text":"x := 1"}],` +
		`[{"tag":"img","image_key":"img_v3_post"}]]}`
	const wantText = "看下这段代码\n```go\nx := 1\n```"
	check := func(t *testing.T, stub *mediaStub, h *handled) {
		t.Helper()
		got := h.all()
		if len(got) != 1 {
			t.Fatalf("handler got %d messages, want 1", len(got))
		}
		if got[0].Text != wantText || len(got[0].Images) != 1 || len(got[0].Files) != 0 {
			t.Errorf("message = {Text:%q Images:%d Files:%+v}, want {%q 1 none}",
				got[0].Text, len(got[0].Images), got[0].Files, wantText)
		}
		if !got[0].MentionMe || got[0].ChatType != "group" {
			t.Errorf("MentionMe=%v ChatType=%q, want true/group", got[0].MentionMe, got[0].ChatType)
		}
		if n := stub.downloads.Load(); n != 1 {
			t.Errorf("downloads = %d, want 1", n)
		}
	}

	t.Run("webhook", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{ConnectionMode: "webhook", VerificationToken: "test_token"}, nil)
		f.botOpenID = "ou_bot"
		var h handled
		mux := http.NewServeMux()
		f.registerWebhook(mux, h.handler)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(buildV2GroupPostBody("ev_post", content)))
		if w.Code != http.StatusOK {
			t.Fatalf("webhook status = %d, want 200", w.Code)
		}
		f.dispatch.Wait()
		check(t, stub, &h)
	})
	t.Run("websocket", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{}, nil)
		f.botOpenID = "ou_bot"
		var h handled
		ev := sdkMediaEvent("ev_post", "post", content)
		ev.Event.Message.ChatType = strPtr("group")
		ev.Event.Message.Mentions = []*larkim.MentionEvent{{
			Key: strPtr("@_user_1"), Name: strPtr("bot"), Id: &larkim.UserId{OpenId: strPtr("ou_bot")},
		}}
		pe, ok := f.parseSDKEvent(ev)
		if !ok || pe.MediaType != "post" {
			t.Fatalf("parseSDKEvent(post) = %+v, %v; want a post event", pe, ok)
		}
		f.routeParsed(context.Background(), h.handler, pe)
		f.dispatch.Wait()
		check(t, stub, &h)
	})
}

func buildV2GroupPostBody(eventID, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_id": eventID, "event_type": "im.message.receive_v1", "token": "test_token"},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_eve"}},
			"message": map[string]any{
				"message_id": "om_" + eventID, "chat_id": "oc_1", "chat_type": "group",
				"message_type": "post", "content": content,
				"mentions": []map[string]any{{"key": "@_user_1", "name": "bot", "id": map[string]any{"open_id": "ou_bot"}}},
			},
		},
	})
	return b
}

// A message type naozhi cannot read gets a notice instead of silence, at most
// once per chat per window, on both transports; it never reaches the handler.
func TestUnsupportedType_Notice(t *testing.T) {
	t.Parallel()
	t.Run("webhook", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{ConnectionMode: "webhook", VerificationToken: "test_token"}, nil)
		var h handled
		mux := http.NewServeMux()
		f.registerWebhook(mux, h.handler)
		for _, id := range []string{"ev_s1", "ev_s2"} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, buildTokenRequest(buildV2MediaBody(id, "sticker", `{"file_key":"file_v3_s"}`)))
			if w.Code != http.StatusOK {
				t.Fatalf("webhook status = %d, want 200", w.Code)
			}
			f.dispatch.Wait()
		}
		if n := stub.replies.Load(); n != 1 || len(h.all()) != 0 {
			t.Errorf("replies=%d handled=%d, want one throttled notice and nothing handled", n, len(h.all()))
		}
	})
	t.Run("websocket", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{}, nil)
		var h handled
		for _, id := range []string{"ev_m1", "ev_m2"} {
			pe, ok := f.parseSDKEvent(sdkMediaEvent(id, "media", `{"file_key":"file_v3_m"}`))
			if !ok {
				t.Fatal("parseSDKEvent dropped a media message")
			}
			f.routeParsed(context.Background(), h.handler, pe)
			f.dispatch.Wait()
		}
		if n := stub.replies.Load(); n != 1 || len(h.all()) != 0 {
			t.Errorf("replies=%d handled=%d, want one throttled notice and nothing handled", n, len(h.all()))
		}
	})
}
