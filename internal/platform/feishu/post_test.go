package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
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
	cases := []struct {
		name     string
		raw      string
		wantText string
		wantKeys []string
	}{
		{
			name: "doc sample",
			raw:  docPost,
			wantText: "我是一个标题\n第一行 :超链接 (http://www.feishu.cn)\n\n第二行:文本测试\n\n---\n" +
				"```go\nfunc main() int64 {\n    return 0\n}\n```",
			wantKeys: []string{"img_47354fbc-a159-40ed-86ab-2ad0f1acb42g"},
		},
		{
			name:     "locale wrapped",
			raw:      `{"en_us":{"title":"","content":[[{"tag":"text","text":"hello"}]]}}`,
			wantText: "hello",
		},
		{
			name:     "zh_cn wins over en_us",
			raw:      `{"en_us":{"content":[[{"tag":"text","text":"hello"}]]},"zh_cn":{"content":[[{"tag":"text","text":"你好"}]]}}`,
			wantText: "你好",
		},
		{
			name:     "mention only",
			raw:      `{"content":[[{"tag":"at","user_id":"@_user_1","user_name":"bot"},{"tag":"text","text":" "}]]}`,
			wantText: "",
		},
		{
			name:     "bare link and link without href",
			raw:      `{"content":[[{"tag":"a","href":"https://x.test","text":"https://x.test"},{"tag":"text","text":" "},{"tag":"a","text":"label"}]]}`,
			wantText: "https://x.test label",
		},
		{
			name:     "malformed image key skipped",
			raw:      `{"content":[[{"tag":"img","image_key":"img key"},{"tag":"img","image_key":"img_ok"}]]}`,
			wantKeys: []string{"img_ok"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, keys, err := parsePostContent(tc.raw)
			if err != nil {
				t.Fatalf("parsePostContent: %v", err)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if !reflect.DeepEqual(keys, tc.wantKeys) {
				t.Errorf("image keys = %q, want %q", keys, tc.wantKeys)
			}
		})
	}
}

func TestParsePostContent_Errors(t *testing.T) {
	t.Parallel()
	if _, _, err := parsePostContent(`not json`); err == nil {
		t.Error("malformed content parsed without error")
	}
	big, _ := json.Marshal(map[string]any{"content": [][]map[string]string{{
		{"tag": "text", "text": strings.Repeat("a", maxIncomingTextBytes+1)},
	}}})
	if _, _, err := parsePostContent(string(big)); !errors.Is(err, errPostTooLong) {
		t.Errorf("oversized post: err = %v, want errPostTooLong", err)
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

// A post with no picture is a text message: no download, no admission check
// in the adapter.
func TestPostMessage_TextOnly(t *testing.T) {
	t.Parallel()
	f, stub, _ := newMediaFeishu(t, Config{}, nil)
	var h handled
	pe, ok := f.parseSDKEvent(sdkMediaEvent("ev_t", "post", `{"content":[[{"tag":"text","text":"line 1"}],[{"tag":"text","text":"line 2"}]]}`))
	if !ok || pe.MediaType != "" || len(pe.ImageKeys) != 0 {
		t.Fatalf("parseSDKEvent = %+v, %v; want a plain text event", pe, ok)
	}
	f.routeParsed(context.Background(), h.handler, pe)
	f.dispatch.Wait()
	if got := h.all(); len(got) != 1 || got[0].Text != "line 1\nline 2" {
		t.Errorf("handler got %+v, want one message with both lines", got)
	}
	if stub.downloads.Load() != 0 {
		t.Errorf("text-only post caused %d downloads", stub.downloads.Load())
	}

	for _, raw := range []string{`{"content":[[{"tag":"emotion","emoji_type":"SMILE"}]]}`, `{"content":"x"}`} {
		if pe, ok := f.parseSDKEvent(sdkMediaEvent("ev_e", "post", raw)); ok {
			t.Errorf("post %s parsed as %+v, want dropped", raw, pe)
		}
	}
}

func TestHandlePost_Caps(t *testing.T) {
	t.Parallel()
	t.Run("count cap", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{}, nil)
		var h handled
		keys := make([]string, limits.MaxFileAttachmentsPerMessage+1)
		for i := range keys {
			keys[i] = "img_" + string(rune('a'+i))
		}
		f.handlePost(context.Background(), h.handler, mediaMsg, "om_1", keys)
		got := h.all()
		if len(got) != 1 {
			t.Fatalf("handler got %d messages, want 1", len(got))
		}
		if len(got[0].Images) != limits.MaxFileAttachmentsPerMessage || stub.downloads.Load() != int64(limits.MaxFileAttachmentsPerMessage) {
			t.Errorf("images=%d downloads=%d, want %d each", len(got[0].Images), stub.downloads.Load(), limits.MaxFileAttachmentsPerMessage)
		}
		if len(got[0].Files) != 1 || got[0].Files[0].Reject != platform.FileRejectTooMany {
			t.Errorf("Files = %+v, want the extra picture rejected as too_many", got[0].Files)
		}
	})
	t.Run("download failure is reported", func(t *testing.T) {
		t.Parallel()
		f := newFileFeishu(t, Config{}, &fileStub{status: http.StatusInternalServerError})
		var h handled
		f.handlePost(context.Background(), h.handler, mediaMsg, "om_1", []string{"img_1"})
		got := h.all()
		if len(got) != 1 || len(got[0].Images) != 0 || len(got[0].Files) != 1 ||
			got[0].Files[0].Reject != platform.FileRejectDownloadFailed {
			t.Errorf("handler got %+v, want one message naming the failed picture", got)
		}
	})
	t.Run("refused sender", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{}, func(context.Context, platform.IncomingMessage) bool { return false })
		var h handled
		f.handlePost(context.Background(), h.handler, mediaMsg, "om_1", []string{"img_1"})
		if stub.downloads.Load() != 0 || len(h.all()) != 0 {
			t.Errorf("refused post: downloads=%d handled=%d, want 0/0", stub.downloads.Load(), len(h.all()))
		}
	})
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
	t.Run("silent cases", func(t *testing.T) {
		t.Parallel()
		f, stub, _ := newMediaFeishu(t, Config{}, nil)
		f.noticeUnsupported(platform.IncomingMessage{ChatID: "oc_g", ChatType: "group"}, "sticker")
		f.noticeUnsupported(platform.IncomingMessage{ChatID: "oc_s", ChatType: "direct"}, "system")
		f.noticeUnsupported(platform.IncomingMessage{ChatID: "oc_e", ChatType: "direct"}, "")
		f.dispatch.Wait()
		refused, rstub, _ := newMediaFeishu(t, Config{}, func(context.Context, platform.IncomingMessage) bool { return false })
		refused.noticeUnsupported(platform.IncomingMessage{ChatID: "oc_r", ChatType: "direct"}, "sticker")
		refused.dispatch.Wait()
		if stub.replies.Load() != 0 || rstub.replies.Load() != 0 {
			t.Errorf("replies = %d/%d, want none for an unmentioned group, a system message, or a refused sender",
				stub.replies.Load(), rstub.replies.Load())
		}
	})
}

func TestNoticeThrottle(t *testing.T) {
	t.Parallel()
	var th noticeThrottle
	now := time.Unix(1_700_000_000, 0)
	if !th.allow("a", now) || th.allow("a", now.Add(unsupportedNoticeWindow-time.Second)) {
		t.Error("second notice inside the window was allowed, or the first was refused")
	}
	if !th.allow("a", now.Add(unsupportedNoticeWindow)) {
		t.Error("notice after the window was refused")
	}
	if !th.allow("b", now) {
		t.Error("a different chat was throttled")
	}

	var full noticeThrottle
	refused := 0
	for i := 0; i <= unsupportedNoticeCap; i++ {
		if !full.allow(string(rune(0x4e00+i)), now) {
			refused++
		}
	}
	if refused != 1 || len(full.last) != unsupportedNoticeCap {
		t.Errorf("refused=%d tracked=%d, want 1 refused once full of live entries, %d tracked",
			refused, len(full.last), unsupportedNoticeCap)
	}
	if !full.allow("late", now.Add(unsupportedNoticeWindow)) {
		t.Error("expired entries were not evicted for a new chat")
	}
}
