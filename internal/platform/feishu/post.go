package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
)

// post.go — inbound "post" (rich-text) messages, and the notice for message
// types naozhi cannot read. The Feishu client sends a post for formatted
// text, pasted code and text with inline pictures (#3626).

// msgUnsupportedType answers a message type naozhi cannot read, so the user
// is not left waiting on a bot that dropped it.
const msgUnsupportedType = "[暂不支持此类消息，请发送文字、图片或文件]"

// unsupportedNoticeWindow is the quiet time between two unsupported-type
// notices in one chat; unsupportedNoticeCap bounds the chats remembered.
const (
	unsupportedNoticeWindow = time.Minute
	unsupportedNoticeCap    = 1024
)

// errPostTooLong is returned by parsePostContent when the rendered text is
// over maxIncomingTextBytes.
var errPostTooLong = errors.New("post text exceeds limit")

// postElement is one inline element of a post paragraph. Only the fields
// naozhi renders are decoded.
type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Href     string `json:"href"`
	ImageKey string `json:"image_key"`
	Language string `json:"language"`
}

// postBody is a post's title and paragraphs, each a run of inline elements.
type postBody struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
}

// parsePostContent renders a post message's content to plain text and
// collects the image_keys of its inline pictures, in order. Received posts
// carry the body at the top level; a locale-wrapped body ({"zh_cn": {...}},
// the shape a sender builds) is accepted too. @-mentions are dropped like the
// text path strips their keys; malformed image keys are skipped with a warning.
func parsePostContent(raw string) (text string, imageKeys []string, err error) {
	var body postBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return "", nil, err
	}
	if body.Title == "" && len(body.Content) == 0 {
		var locales map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &locales); err != nil {
			return "", nil, err
		}
		// Fixed preference first, so the pick does not depend on map order.
		for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
			if b, ok := locales[lang]; ok {
				if err := json.Unmarshal(b, &body); err != nil {
					return "", nil, err
				}
				break
			}
		}
	}

	var b strings.Builder
	if t := strings.TrimSpace(body.Title); t != "" {
		b.WriteString(t)
		b.WriteByte('\n')
	}
	for _, para := range body.Content {
		for _, el := range para {
			switch el.Tag {
			case "text", "md":
				b.WriteString(el.Text)
			case "a":
				switch {
				case el.Text == "" || el.Text == el.Href:
					b.WriteString(el.Href)
				case el.Href == "":
					b.WriteString(el.Text)
				default:
					b.WriteString(el.Text + " (" + el.Href + ")")
				}
			case "code_block":
				b.WriteString("```" + strings.ToLower(el.Language) + "\n" + strings.TrimSuffix(el.Text, "\n") + "\n```")
			case "hr":
				b.WriteString("---")
			case "img":
				if !isValidFeishuResourceKey(el.ImageKey) {
					slog.Warn("feishu post: skipping malformed image_key",
						"key", osutil.SanitizeForLog(el.ImageKey, 64))
					continue
				}
				imageKeys = append(imageKeys, el.ImageKey)
			}
			// "at" is dropped, as are elements with nothing to read
			// ("emotion", inline "media").
		}
		b.WriteByte('\n')
	}
	// Same ingress cap as a text message.
	if b.Len() > maxIncomingTextBytes {
		return "", nil, errPostTooLong
	}
	return strings.TrimSpace(b.String()), imageKeys, nil
}

// handlePost downloads a post's inline pictures under the per-message count
// and byte caps, then calls handler with them in msg.Images. A picture that
// is not delivered is named in msg.Files with its FileReject, so dispatch
// tells the user. A sender refused by admission costs no download.
func (f *Feishu) handlePost(ctx context.Context, handler platform.MessageHandler, msg platform.IncomingMessage, messageID string, imageKeys []string) {
	if !f.admitted(ctx, msg) {
		return
	}
	total := 0
	for i, key := range imageKeys {
		name := "图片 " + strconv.Itoa(i+1)
		reject := func(r platform.FileReject) {
			msg.Files = append(msg.Files, platform.File{Name: name, Reject: r})
		}
		if i >= limits.MaxFileAttachmentsPerMessage {
			reject(platform.FileRejectTooMany)
			continue
		}
		remaining := limits.MaxFileAttachmentBytes - total
		perImage := min(maxImageDownloadBytes, remaining)
		if perImage <= 0 {
			reject(platform.FileRejectTotalTooLarge)
			continue
		}
		data, mime, err := f.downloadResource(ctx, messageID, key, "image", int64(perImage), "image/png")
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; nothing to answer
			}
			// image_key is sender-controlled; sanitize before slog.
			slog.Warn("feishu download post image failed", "err", err,
				"key", osutil.SanitizeForLog(key, 128))
			switch {
			case !errors.Is(err, errResourceTooLarge):
				reject(platform.FileRejectDownloadFailed)
			case perImage == maxImageDownloadBytes:
				reject(platform.FileRejectTooLarge)
			default:
				reject(platform.FileRejectTotalTooLarge)
			}
			continue
		}
		total += len(data)
		msg.Images = append(msg.Images, platform.Image{Data: data, MimeType: mime})
	}
	handler(ctx, msg)
}

// noticeUnsupported answers a message of a type naozhi cannot read with a
// short notice, at most once per unsupportedNoticeWindow per chat. A group
// message that does not @ the bot, and a sender refused by admission, get
// none. Runs on the dispatch pool; the caller must not block on it.
func (f *Feishu) noticeUnsupported(msg platform.IncomingMessage, msgType string) {
	slog.Debug("feishu: unsupported message type",
		"type", osutil.SanitizeForLog(msgType, 32), "chat_type", msg.ChatType,
		"msg_id", osutil.SanitizeForLog(msg.MessageID, 64))
	// "system" messages (member joined, …) are not the user talking to us.
	if msgType == "" || msgType == "system" || (msg.ChatType == "group" && !msg.MentionMe) {
		return
	}
	f.dispatch.TryGo("feishu unsupported notice", func() {
		if !f.admitted(f.stopCtx, msg) || !f.unsupportedNotices.allow(msg.ChatID, time.Now()) {
			return
		}
		f.replyError(f.stopCtx, msg, msgUnsupportedType)
	})
}

// noticeThrottle remembers when each chat was last sent a notice.
type noticeThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether key may get a notice at now, recording it if so.
// When the map is full of live entries a new key gets none.
func (t *noticeThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, seen := t.last[key]
	if seen && now.Sub(at) < unsupportedNoticeWindow {
		return false
	}
	if !seen && len(t.last) >= unsupportedNoticeCap {
		for k, at := range t.last {
			if now.Sub(at) >= unsupportedNoticeWindow {
				delete(t.last, k)
			}
		}
		if len(t.last) >= unsupportedNoticeCap {
			return false
		}
	}
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	t.last[key] = now
	return true
}
