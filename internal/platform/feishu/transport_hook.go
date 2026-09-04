package feishu

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
)

// registerWebhook registers the Feishu webhook HTTP handler.
func (f *Feishu) registerWebhook(mux *http.ServeMux, handler platform.MessageHandler) {
	mux.HandleFunc("POST /webhook/feishu", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		// Defense-in-depth against a bypassed config.validateConfig: with no
		// credential every token/signature/nonce check below would be skipped.
		if f.cfg.VerificationToken == "" && f.cfg.EncryptKey == "" {
			slog.Error("feishu webhook refused: no verification_token or encrypt_key configured")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// +1 distinguishes "at the limit" from "truncated"; surface 413 rather
		// than silently dropping a truncated event.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body) > maxWebhookBodyBytes {
			slog.Warn("feishu webhook body exceeds limit", "limit", maxWebhookBodyBytes)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		slog.Debug("feishu webhook received", "body_len", len(body))

		// One traffic-correlated SECURITY error on the first live token-only
		// delivery; Once bounds log amplification (#1724).
		if f.cfg.EncryptKey == "" && f.cfg.AllowInsecureWebhook {
			f.insecureWebhookWarnOnce.Do(func() {
				slog.Error("SECURITY: feishu webhook is processing live traffic in verification_token-only mode (no encrypt_key/HMAC) — events are replay/forgery-prone if the token leaks. Configure encrypt_key to disable allow_insecure_webhook posture.")
			})
		}

		type feishuEnvelope struct {
			Challenge string `json:"challenge"`
			Token     string `json:"token"`
			Type      string `json:"type"`
			Schema    string `json:"schema"`
			Encrypt   string `json:"encrypt"`
			Header    *struct {
				EventID   string `json:"event_id"`
				EventType string `json:"event_type"`
				Token     string `json:"token"`
			} `json:"header"`
			Event json.RawMessage `json:"event"`
		}
		var envelope feishuEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Timestamp freshness is enforced for every authenticated mode BEFORE the
		// token check so a valid-token replay is rejected. url_verification may
		// legitimately arrive without the header (legacy apps) and only reflects
		// the challenge, so the missing-header case alone is exempted — a
		// supplied stale/malformed timestamp is still rejected.
		isURLVerification := envelope.Type == "url_verification"
		if ts := r.Header.Get("X-Lark-Request-Timestamp"); ts == "" {
			if !isURLVerification && (f.cfg.EncryptKey != "" || f.cfg.VerificationToken != "") {
				slog.Warn("feishu request missing timestamp header", "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		} else if !verifyTimestamp(ts) {
			slog.Warn("feishu request timestamp too old or invalid", "timestamp", ts)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Token verification (v1: top-level token, v2: header.token)
		if f.cfg.VerificationToken != "" {
			token := envelope.Token
			if envelope.Header != nil && envelope.Header.Token != "" {
				token = envelope.Header.Token
			}
			// Length cap before hashing bounds the SHA-256 cost an attacker can force.
			if len(token) > maxWebhookTokenLen {
				slog.Warn("feishu token too long", "len", len(token))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if token == "" || !constantTimeEqualString(token, f.cfg.VerificationToken) {
				slog.Warn("feishu token mismatch", "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Signature verification (v2 events with encrypt_key)
		if f.cfg.EncryptKey != "" {
			timestamp := r.Header.Get("X-Lark-Request-Timestamp")
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			sig := r.Header.Get("X-Lark-Signature")
			if len(sig) > maxWebhookSigLen {
				slog.Warn("feishu webhook signature header too long", "len", len(sig))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !verifySignature(timestamp, nonce, f.cfg.EncryptKey, body, sig) {
				slog.Warn("feishu signature verification failed", "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Encrypt Key mode: Feishu AES-256-CBC-encrypts the ENTIRE push body as
		// `{"encrypt":"<base64>"}`. The v2 signature covers the raw ciphertext
		// (already verified above), so only now decrypt and re-parse the real
		// challenge/token/event; the verification token travels inside (#2115).
		if f.cfg.EncryptKey != "" && envelope.Encrypt != "" {
			plain, derr := decryptFeishuEvent(f.cfg.EncryptKey, envelope.Encrypt)
			if derr != nil {
				slog.Warn("feishu webhook: encrypt payload decrypt failed", "err", derr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(plain) > maxWebhookBodyBytes {
				slog.Warn("feishu webhook decrypted body exceeds limit", "limit", maxWebhookBodyBytes)
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			var inner feishuEnvelope
			if err := json.Unmarshal(plain, &inner); err != nil {
				slog.Warn("feishu webhook: decrypted payload not valid json", "err", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// The outer shell carried no token; validate the inner one.
			if f.cfg.VerificationToken != "" {
				token := inner.Token
				if inner.Header != nil && inner.Header.Token != "" {
					token = inner.Header.Token
				}
				if len(token) > maxWebhookTokenLen {
					slog.Warn("feishu token too long (encrypted)", "len", len(token))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if token == "" || !constantTimeEqualString(token, f.cfg.VerificationToken) {
					slog.Warn("feishu token mismatch (encrypted)", "remote", r.RemoteAddr)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			envelope = inner
			// `body` stays the raw ciphertext on purpose: verifySignature is its
			// only reader and Feishu signs the encrypted body. Read `envelope`.
		}

		// Nonce dedup for every authenticated mode; url_verification challenges
		// are included (the eviction self-heal below makes a cap-pin impossible,
		// so a replayed fixed ts:nonce challenge is rejected, #1594).
		if ts := r.Header.Get("X-Lark-Request-Timestamp"); ts != "" {
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			if nonce != "" {
				if len(nonce) > maxWebhookNonceLen {
					slog.Warn("feishu webhook nonce too long", "len", len(nonce))
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				// The nonce becomes a map key and may reach slog attrs; printable
				// ASCII only so C0/C1/bidi bytes cannot corrupt structured logs.
				for i := 0; i < len(nonce); i++ {
					c := nonce[i]
					if c < 0x21 || c > 0x7e {
						slog.Warn("feishu webhook nonce contains non-printable bytes", "len", len(nonce))
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				{
					// Reserve-then-check: Add(1) first so a concurrent burst cannot
					// overshoot the cap; on cap-hit evict the oldest batch (a leaked
					// token could otherwise pin the map for nonceTTL, #1332) and
					// re-check; if eviction removed nothing fall back to 429.
					if n := f.seenNoncesCount.Add(1); n > maxSeenNonces {
						// evictOldestNonces resyncs the counter to the live size,
						// discarding this speculative +1; re-apply it AFTER eviction
						// so the LoadOrStore slot stays reserved (#1534).
						evicted := f.evictNonces()
						if evicted == 0 {
							// No resync happened, so the speculative +1 is still
							// live: roll it back (a second Add(1) would leak).
							f.seenNoncesCount.Add(-1)
							slog.Warn("feishu webhook nonce map at cap, dropping request",
								"cap", maxSeenNonces, "evicted", evicted)
							w.WriteHeader(http.StatusTooManyRequests)
							return
						}
						postEvict := f.seenNoncesCount.Add(1)
						if postEvict > maxSeenNonces {
							f.seenNoncesCount.Add(-1)
							slog.Warn("feishu webhook nonce map at cap, dropping request",
								"cap", maxSeenNonces, "evicted", evicted)
							w.WriteHeader(http.StatusTooManyRequests)
							return
						}
						slog.Warn("feishu webhook nonce map at cap, evicted oldest entries",
							"cap", maxSeenNonces, "evicted", evicted)
					}
					key := ts + ":" + nonce
					expiry := time.Now().Add(nonceTTL).Unix()
					if _, loaded := f.seenNonces.LoadOrStore(key, expiry); loaded {
						f.seenNoncesCount.Add(-1)
						// Length only — never log the attacker-supplied nonce bytes.
						slog.Warn("feishu webhook replay detected",
							"nonce_len", len(nonce), "ts", ts)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
			} else if f.cfg.EncryptKey != "" || f.cfg.VerificationToken != "" {
				// A nonce-less request is replayable within the 5min window.
				// Feishu v2 sends the nonce on url_verification too — no exemption.
				slog.Warn("feishu webhook missing nonce header", "type", envelope.Type)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Challenge reflection (after authentication). The challenge is
		// reflected verbatim, so it is capped at 1 KiB and must be valid UTF-8
		// with no control/bidi runes — real challenges are opaque ASCII tokens.
		if envelope.Type == "url_verification" {
			// Gate through the dispatch semaphore like every other branch so a
			// leaked token cannot flood challenges past the handler cap; the
			// only drop that reports back to the caller (503).
			if !f.dispatch.TryAcquire() {
				slog.Warn("feishu webhook: handler semaphore full, dropping url_verification")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer f.dispatch.Release()
			if len(envelope.Challenge) > 1024 {
				slog.Warn("feishu challenge too long", "len", len(envelope.Challenge))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !utf8.ValidString(envelope.Challenge) {
				slog.Warn("feishu challenge not valid utf-8")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, r := range envelope.Challenge {
				if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
					slog.Warn("feishu challenge contains control/bidi rune")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			// Feishu compares the reflected challenge byte-for-byte; HTML-entity
			// escaping of `<` `>` `&` would break the match.
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(map[string]string{"challenge": envelope.Challenge}); err != nil {
				slog.Warn("feishu challenge encode failed", "err", err)
			}
			return
		}

		w.WriteHeader(http.StatusOK)

		eventType := ""
		if envelope.Header != nil {
			eventType = envelope.Header.EventType
		}
		// AskUserQuestion card click: synthesised into an IncomingMessage whose
		// Text is the chosen option, on f.dispatch like other message types.
		if eventType == "card.action.trigger" || eventType == "im.card.action.v1_trigger" {
			rawEvent := envelope.Event
			f.dispatch.TryGo("feishu card_action", func() {
				f.handleCardActionWebhook(f.stopCtx, rawEvent, handler)
			})
			return
		}
		if eventType != "im.message.receive_v1" {
			return
		}

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
					// ID.OpenID is the @-target's open_id; payloads that omit it
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

		msgType := event.Message.MessageType
		if msgType != "text" && msgType != "image" && msgType != "audio" {
			return
		}

		// v1 events carry no Header.EventID; fabricate one from ts+nonce so
		// Dedup.Seen is not a no-op and rapid retries do not leak through.
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
		// Cap before the dedup map stores it as a key: 64 KiB keys × 50k bucket
		// cap ≈ 3 GiB worst case. Dropping the ID risks one double-process only.
		if len(eventID) > maxEventIDLen {
			slog.Warn("feishu webhook: event_id too long, skipping dedup for this delivery",
				"len", len(eventID))
			eventID = ""
		}

		chatType := "direct"
		if event.Message.ChatType == "group" {
			chatType = "group"
		}

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
				// Schema drift would otherwise drop every message with no trace.
				slog.Debug("feishu webhook: text content unmarshal failed",
					"err", err, "msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				return
			}
			text := content.Text
			// Feishu's own text limit is ~4 KiB; larger is a misconfigured client
			// or a crafted payload that must not reach slog attrs or CLI stdin.
			if len(text) > maxIncomingTextBytes {
				slog.Warn("feishu webhook: text exceeds limit, dropping",
					"msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64), "len", len(text))
				return
			}
			// Strip all @-mention tokens in a single pass.
			if len(event.Message.Mentions) > 0 {
				pairs := make([]string, 0, len(event.Message.Mentions)*2)
				for _, m := range event.Message.Mentions {
					pairs = append(pairs, m.Key, "")
				}
				text = strings.NewReplacer(pairs...).Replace(text)
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			msg.Text = text
			f.dispatch.TryGo("feishu text", func() { handler(f.stopCtx, msg) })

		case "image":
			var content struct {
				ImageKey string `json:"image_key"`
			}
			if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil || content.ImageKey == "" {
				if err != nil {
					slog.Debug("feishu webhook: image content unmarshal failed",
						"err", err, "msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				}
				return
			}
			if !isValidFeishuResourceKey(content.ImageKey) {
				slog.Warn("feishu webhook: rejecting malformed image_key",
					"key", osutil.SanitizeForLog(content.ImageKey, 64),
					"msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				return
			}
			f.dispatch.TryGo("feishu image", func() {
				imgMsg := msg
				data, mime, err := f.DownloadImage(f.stopCtx, event.Message.MessageID, content.ImageKey)
				if err != nil {
					// image_key is sender-controlled; sanitize before slog.
					slog.Error("feishu download image failed", "err", err,
						"key", osutil.SanitizeForLog(content.ImageKey, 128))
					return
				}
				imgMsg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(f.stopCtx, imgMsg)
			})

		case "audio":
			var content struct {
				FileKey string `json:"file_key"`
			}
			if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil || content.FileKey == "" {
				if err != nil {
					slog.Debug("feishu webhook: audio content unmarshal failed",
						"err", err, "msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				}
				return
			}
			if !isValidFeishuResourceKey(content.FileKey) {
				slog.Warn("feishu webhook: rejecting malformed file_key",
					"key", osutil.SanitizeForLog(content.FileKey, 64),
					"msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				return
			}
			f.dispatch.TryGo("feishu audio", func() {
				audioMsg := msg
				f.handleAudio(f.stopCtx, handler, audioMsg, event.Message.MessageID, content.FileKey)
			})
		}
	})
}

// isValidFeishuResourceKey accepts only printable non-whitespace ASCII (≤256
// bytes) for image_key / file_key so a sender-crafted key cannot smuggle
// log-splitting bytes or oversized payloads into the API URL builder.
func isValidFeishuResourceKey(k string) bool {
	if k == "" || len(k) > 256 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// constantTimeEqualString compares two strings in constant time without leaking
// lengths: ConstantTimeCompare short-circuits on unequal lengths, so both sides
// are hashed to a fixed-length digest first.
func constantTimeEqualString(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
