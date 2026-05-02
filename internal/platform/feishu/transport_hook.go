package feishu

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
)

// registerWebhook registers the Feishu webhook HTTP handler.
func (f *Feishu) registerWebhook(mux *http.ServeMux, handler platform.MessageHandler) {
	mux.HandleFunc("POST /webhook/feishu", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		// Defense-in-depth: refuse zero-credential webhook invocations even if
		// config.validateConfig has been bypassed (e.g. programmatic constructor
		// in tests or a future refactor). Without at least one of VerificationToken
		// or EncryptKey set, the handler below would skip token/signature/nonce
		// checks and happily process arbitrary events. config.validateConfig
		// already rejects this combination at startup for webhook mode; this
		// second gate ensures the invariant holds even if that refusal is
		// skipped or weakened. R67-SEC-9.
		if f.cfg.VerificationToken == "" && f.cfg.EncryptKey == "" {
			slog.Error("feishu webhook refused: no verification_token or encrypt_key configured")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Read up to maxBody+1 so we can distinguish "exactly maxBody" (legal)
		// from "exceeds maxBody" (silently truncated). A truncated body would
		// deserialize into malformed/empty JSON and drop the event silently;
		// better to surface 413 so operators can raise the cap if needed.
		const maxBody = 64 * 1024
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body) > maxBody {
			slog.Warn("feishu webhook body exceeds limit", "limit", maxBody)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		slog.Debug("feishu webhook received", "body_len", len(body))

		// Parse the outer envelope
		var envelope struct {
			Challenge string `json:"challenge"`
			Token     string `json:"token"`
			Type      string `json:"type"`
			Schema    string `json:"schema"`
			Header    *struct {
				EventID   string `json:"event_id"`
				EventType string `json:"event_type"`
				Token     string `json:"token"`
			} `json:"header"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Token verification (v1: top-level token, v2: header.token)
		if f.cfg.VerificationToken != "" {
			token := envelope.Token
			if envelope.Header != nil && envelope.Header.Token != "" {
				token = envelope.Header.Token
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(f.cfg.VerificationToken)) != 1 {
				slog.Warn("feishu token mismatch")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Timestamp verification — enforce for any authenticated webhook mode
		// to prevent replay attacks. Both EncryptKey and VerificationToken modes
		// benefit from timestamp freshness checks as a defense-in-depth measure.
		if ts := r.Header.Get("X-Lark-Request-Timestamp"); ts == "" {
			if f.cfg.EncryptKey != "" || f.cfg.VerificationToken != "" {
				slog.Warn("feishu request missing timestamp header")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		} else if !verifyTimestamp(ts) {
			slog.Warn("feishu request timestamp too old or invalid", "timestamp", ts)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Signature verification (v2 events with encrypt_key)
		if f.cfg.EncryptKey != "" {
			timestamp := r.Header.Get("X-Lark-Request-Timestamp")
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			sig := r.Header.Get("X-Lark-Signature")
			if !verifySignature(timestamp, nonce, f.cfg.EncryptKey, body, sig) {
				slog.Warn("feishu signature verification failed")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Nonce dedup: prevent replay attacks within the nonce TTL window.
		// Any authenticated webhook mode (EncryptKey or VerificationToken)
		// requires a nonce — a stolen webhook otherwise replays freely inside
		// the 5min timestamp window. url_verification challenges also reach
		// here but are exempt below.
		if ts := r.Header.Get("X-Lark-Request-Timestamp"); ts != "" {
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			if nonce != "" {
				// Feishu nonces are 16-char random strings in practice. Reject
				// anything pathologically large so a header-flood with giant
				// nonces cannot bloat seenNonces (sync.Map retains entries for
				// nonceTTL = 5min, cleaned up on a timer).
				if len(nonce) > 128 {
					slog.Warn("feishu webhook nonce too long", "len", len(nonce))
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				// Global cap: refuse new nonces once the map hits maxSeenNonces
				// so a flood of unique-nonce requests cannot bloat heap. Legitimate
				// traffic at a 5-minute TTL stays well below this cap.
				if f.seenNoncesCount.Load() >= maxSeenNonces {
					slog.Warn("feishu webhook nonce map at cap, dropping request",
						"cap", maxSeenNonces)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				key := ts + ":" + nonce
				expiry := time.Now().Add(nonceTTL).Unix()
				if _, loaded := f.seenNonces.LoadOrStore(key, expiry); loaded {
					// Log only the length and timestamp rather than the raw
					// nonce header value — attacker-supplied bytes can contain
					// newlines or JSON metacharacters that distort structured
					// log output and downstream log-ingest parsers.
					slog.Warn("feishu webhook replay detected",
						"nonce_len", len(nonce), "ts", ts)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				f.seenNoncesCount.Add(1)
			} else if f.cfg.EncryptKey != "" || f.cfg.VerificationToken != "" {
				// Authenticated modes must always supply a nonce; missing
				// nonce leaves the request replayable within the 5min
				// timestamp window. Feishu v2 sends X-Lark-Request-Nonce on
				// url_verification handshakes too, so no exemption here —
				// deployments that somehow receive nonce-less challenges will
				// need to reconfigure their Feishu app to v2 event schema.
				slog.Warn("feishu webhook missing nonce header", "type", envelope.Type)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Challenge verification (after authentication). Feishu challenges are
		// short opaque tokens (typically <=32 chars); cap at 1 KiB so a malicious
		// verified request cannot force us to reflect a multi-MB body back.
		if envelope.Type == "url_verification" {
			if len(envelope.Challenge) > 1024 {
				slog.Warn("feishu challenge too long", "len", len(envelope.Challenge))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Challenge is reflected verbatim into the response body; a
			// malformed UTF-8 payload would propagate to Feishu's verification
			// endpoint and could be weaponised if the verification token
			// leaked. Real Feishu challenges are opaque ASCII/Base64 tokens,
			// so invalid UTF-8 is always tampering.
			if !utf8.ValidString(envelope.Challenge) {
				slog.Warn("feishu challenge not valid utf-8")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge}); err != nil {
				slog.Warn("feishu challenge encode failed", "err", err)
			}
			return
		}

		// Return 200 immediately
		w.WriteHeader(http.StatusOK)

		// Only handle message events
		eventType := ""
		if envelope.Header != nil {
			eventType = envelope.Header.EventType
		}
		if eventType != "im.message.receive_v1" {
			return
		}

		// Parse message event
		var event struct {
			Sender struct {
				SenderID struct {
					OpenID string `json:"open_id"`
				} `json:"sender_id"`
			} `json:"sender"`
			Message struct {
				MessageID   string `json:"message_id"`
				ChatID      string `json:"chat_id"`
				ChatType    string `json:"chat_type"`
				Content     string `json:"content"`
				MessageType string `json:"message_type"`
				Mentions    []struct {
					Key  string `json:"key"`
					Name string `json:"name"`
					// ID.OpenID carries the @-target's bot/user open_id when
					// present. Feishu event schema has included this field for
					// years; older payloads that omit it decode as empty and
					// force isBotMentioned's degraded "any @" path.
					ID struct {
						OpenID string `json:"open_id"`
					} `json:"id"`
				} `json:"mentions"`
			} `json:"message"`
		}
		if err := json.Unmarshal(envelope.Event, &event); err != nil {
			slog.Error("parse feishu event", "err", err)
			return
		}

		// Only handle text, image, and audio messages
		msgType := event.Message.MessageType
		if msgType != "text" && msgType != "image" && msgType != "audio" {
			return
		}

		// Build base incoming message. v2 events carry Header.EventID; v1
		// events do not, so fabricate one from timestamp+nonce when both are
		// present (enforced above for authenticated modes). Without an ID
		// Dedup.Seen is a no-op and rapid retries would leak through.
		eventID := ""
		if envelope.Header != nil {
			eventID = envelope.Header.EventID
		}
		if eventID == "" {
			ts := r.Header.Get("X-Lark-Request-Timestamp")
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			if ts != "" && nonce != "" {
				eventID = "v1:" + ts + ":" + nonce
			}
		}

		chatType := "direct"
		if event.Message.ChatType == "group" {
			chatType = "group"
		}

		// Precise bot-mention detection via f.isBotMentioned: match each
		// mention's id.open_id against the bot's cached open_id. Falls back to
		// "any mention" when the bot open_id is unknown (fetchBotInfo failed
		// at Start, or the payload omits id.open_id — older Feishu versions).
		mentions := event.Message.Mentions
		hasMention := f.isBotMentioned(len(mentions), func(i int) string {
			return mentions[i].ID.OpenID
		})

		msg := platform.IncomingMessage{
			Platform:  "feishu",
			EventID:   eventID,
			MessageID: event.Message.MessageID,
			UserID:    event.Sender.SenderID.OpenID,
			ChatID:    event.Message.ChatID,
			ChatType:  chatType,
			MentionMe: hasMention,
		}

		switch msgType {
		case "text":
			var content struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
				// Without this log, Feishu schema drift silently drops every
				// message and operators see no reply with no trace.
				slog.Debug("feishu webhook: text content unmarshal failed",
					"err", err, "msg_id", event.Message.MessageID)
				return
			}
			text := content.Text
			// Feishu's own upstream limit on message text is ~4000 bytes
			// (~1333 CJK chars); anything larger is either a misconfigured
			// client or attacker-crafted payload. Reject at 8 KiB (2x the
			// official limit) so we don't ferry multi-KB slog attrs or push
			// oversized messages into the downstream CLI stdin path.
			const maxTextBytes = 8 * 1024
			if len(text) > maxTextBytes {
				slog.Warn("feishu webhook: text exceeds limit, dropping",
					"msg_id", event.Message.MessageID, "len", len(text))
				return
			}
			for _, m := range event.Message.Mentions {
				text = strings.ReplaceAll(text, m.Key, "")
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			msg.Text = text
			// Limit concurrent webhook handlers to avoid unbounded goroutine growth.
			select {
			case f.hookSem <- struct{}{}:
			default:
				slog.Warn("feishu webhook: handler semaphore full, dropping text message")
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-f.hookSem }()
				defer platform.RecoverHandler("feishu text")
				handler(f.stopCtx, msg)
			}()

		case "image":
			var content struct {
				ImageKey string `json:"image_key"`
			}
			if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil || content.ImageKey == "" {
				if err != nil {
					slog.Debug("feishu webhook: image content unmarshal failed",
						"err", err, "msg_id", event.Message.MessageID)
				}
				return
			}
			select {
			case f.hookSem <- struct{}{}:
			default:
				slog.Warn("feishu webhook: handler semaphore full, dropping image message")
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-f.hookSem }()
				defer platform.RecoverHandler("feishu image")
				imgMsg := msg
				data, mime, err := f.DownloadImage(f.stopCtx, event.Message.MessageID, content.ImageKey)
				if err != nil {
					slog.Error("feishu download image failed", "err", err, "key", content.ImageKey)
					return
				}
				imgMsg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(f.stopCtx, imgMsg)
			}()

		case "audio":
			var content struct {
				FileKey string `json:"file_key"`
			}
			if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil || content.FileKey == "" {
				if err != nil {
					slog.Debug("feishu webhook: audio content unmarshal failed",
						"err", err, "msg_id", event.Message.MessageID)
				}
				return
			}
			select {
			case f.hookSem <- struct{}{}:
			default:
				slog.Warn("feishu webhook: handler semaphore full, dropping audio message")
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-f.hookSem }()
				defer platform.RecoverHandler("feishu audio")
				audioMsg := msg
				f.handleAudio(f.stopCtx, handler, audioMsg, event.Message.MessageID, content.FileKey)
			}()
		}
	})
}
