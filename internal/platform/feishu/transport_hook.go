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
		// Read up to maxWebhookBodyBytes+1 so we can distinguish "exactly at
		// the limit" (legal) from "exceeds limit" (silently truncated). A
		// truncated body would deserialize into malformed/empty JSON and drop
		// the event silently; better to surface 413 so operators can raise the
		// cap if needed.
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

		// #1724: when running in token-only mode (encrypt_key absent, opted in
		// via allow_insecure_webhook), surface a SECURITY error on the FIRST
		// live delivery rather than relying solely on the startup Warn. A
		// traffic-correlated signal is far harder for operators to miss than a
		// single line buried in boot logs. sync.Once bounds this to one emit per
		// process so a request flood cannot amplify it into a log-spam DoS.
		if f.cfg.EncryptKey == "" && f.cfg.AllowInsecureWebhook {
			f.insecureWebhookWarnOnce.Do(func() {
				slog.Error("SECURITY: feishu webhook is processing live traffic in verification_token-only mode (no encrypt_key/HMAC) — events are replay/forgery-prone if the token leaks. Configure encrypt_key to disable allow_insecure_webhook posture.")
			})
		}

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

		// Timestamp verification — enforce for any authenticated webhook mode
		// to prevent replay attacks BEFORE the token check so a request with
		// a valid token but missing/stale timestamp cannot be replayed within
		// Feishu's 5-minute freshness window. Both EncryptKey and
		// VerificationToken modes benefit from timestamp freshness checks as
		// a defense-in-depth measure. Exception: url_verification handshakes
		// are a one-shot Feishu bootstrap that historically may arrive without
		// the X-Lark-Request-Timestamp header on some legacy app versions; we
		// still gate them behind token equality (below) and the hookSem cap
		// (challenge branch), and they cannot dispatch to handlers (the
		// branch only reflects the challenge). When a url_verification DOES
		// supply a timestamp we still reject if it is stale/malformed —
		// only the missing-header case is exempted. R246-SEC-13.
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
			// Reject pathologically long tokens before hashing — real Feishu
			// tokens are ~32 bytes, so anything beyond maxWebhookTokenLen is
			// either a malformed sender or an attempt to amplify the SHA-256
			// cost inside constantTimeEqualString.
			if len(token) > maxWebhookTokenLen {
				slog.Warn("feishu token too long", "len", len(token))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Hash both sides to a fixed-length digest before the constant-time
			// compare so that pathologically short/long attacker tokens cannot
			// leak the real token's length via timing on the length prefix
			// check that ConstantTimeCompare does internally when operand sizes
			// differ.
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
			// Real Feishu signatures are 64-byte hex strings (SHA-256).
			// Cap at maxWebhookSigLen to prevent an oversized header from
			// amplifying the string concatenation inside verifySignature.
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

		// Nonce dedup: prevent replay attacks within the nonce TTL window.
		// Any authenticated webhook mode (EncryptKey or VerificationToken)
		// requires a nonce — a stolen webhook otherwise replays freely inside
		// the 5min timestamp window. url_verification challenges go through the
		// same seenNonces dedup as event webhooks (R164029-SEC-2 / #1594): the
		// eviction self-heal below means a leaked-token attacker can no longer
		// pin the map at cap, so the previous exemption is removed and a
		// replayed challenge with a fixed ts:nonce is rejected.
		if ts := r.Header.Get("X-Lark-Request-Timestamp"); ts != "" {
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			if nonce != "" {
				// Feishu nonces are 16-char random strings in practice. Reject
				// anything pathologically large so a header-flood with giant
				// nonces cannot bloat seenNonces (sync.Map retains entries for
				// nonceTTL = 5min, cleaned up on a timer).
				if len(nonce) > maxWebhookNonceLen {
					slog.Warn("feishu webhook nonce too long", "len", len(nonce))
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				// R176-SEC-M: nonce is concatenated into the seenNonces map
				// key and reaches slog attrs indirectly via helper logs in
				// future refactors. Restrict to printable ASCII (0x21-0x7E)
				// so byte-level C0/C1/bidi/LS/PS cannot corrupt structured
				// log output. Real Feishu nonces are alphanumeric random
				// strings (mix of upper/lower-case letters and digits, no
				// punctuation) so accepting the full printable ASCII range
				// is a pure-defense tightening — no valid traffic should
				// trip it. (R219-CR-10: prior comment said "base-16-ish"
				// which understated the accepted alphabet; the filter has
				// always allowed full 0x21-0x7E to leave headroom for
				// upstream changes to the nonce alphabet.)
				for i := 0; i < len(nonce); i++ {
					c := nonce[i]
					if c < 0x21 || c > 0x7e {
						slog.Warn("feishu webhook nonce contains non-printable bytes", "len", len(nonce))
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				// R164029-SEC-2 (#1594): url_verification challenges no longer
				// skip the seenNonces map. The original exemption existed to stop
				// an attacker with a leaked verification_token from pinning the
				// map at maxSeenNonces with unique-nonce challenges, but the
				// eviction self-heal below (R20260527122801-SEC-8 / #1332) makes
				// that pin impossible — the oldest entries are evicted on every
				// cap hit. Including challenges in the dedup set closes the replay
				// gap: in token-only mode (allowInsecureWebhook), a captured
				// url_verification challenge with a fixed ts:nonce could otherwise
				// be replayed for the full nonceTTL window, repeatedly reflecting
				// the challenge and consuming hookSem with no nonce-level
				// backpressure. Format checks above still run regardless.
				{
					// Global cap: refuse new nonces once the map hits maxSeenNonces
					// so a flood of unique-nonce requests cannot bloat heap.
					// Reserve-then-check pattern: increment first, then attempt
					// insert; decrement on duplicate or over-cap. Without this,
					// a concurrent burst of N webhooks could each pass the Load()
					// guard before any Add(1) fires, letting count overshoot the
					// cap by up to N (bounded by hookSem but still observable).
					//
					// R20260527122801-SEC-8 (#1332): when the cap is hit, evict
					// the oldest nonceEvictionBatch entries before refusing the
					// request. Without this self-heal, an attacker holding a
					// leaked verification_token can pin the map at cap for the
					// full nonceTTL window (5 min), 429-ing every legitimate
					// webhook. Re-check the cap after eviction; if eviction
					// somehow returned zero entries (race with cleanupNoncesTick
					// or a sync.Map quirk under contention), fall back to the
					// 429 surface so memory stays bounded.
					if n := f.seenNoncesCount.Add(1); n > maxSeenNonces {
						// R20260531070014-SEC-4 (#1534): evictOldestNonces resyncs
						// seenNoncesCount to the map's actual live size under a
						// mutex, which discards this goroutine's speculative +1
						// reservation. Re-apply the +1 AFTER eviction returns so the
						// counter still reserves a slot for the LoadOrStore below
						// and the post-evict cap re-check sees this in-flight insert.
						// Doing the recount inside evict (rather than a racy relative
						// Add) is what keeps the counter from dipping below the real
						// map size under concurrent cap-hit goroutines.
						evicted := f.evictOldestNonces()
						postEvict := f.seenNoncesCount.Add(1)
						if evicted == 0 || postEvict > maxSeenNonces {
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
						// Undo our speculative increment since no new entry landed.
						f.seenNoncesCount.Add(-1)
						// Log only the length and timestamp rather than the raw
						// nonce header value — attacker-supplied bytes can contain
						// newlines or JSON metacharacters that distort structured
						// log output and downstream log-ingest parsers.
						slog.Warn("feishu webhook replay detected",
							"nonce_len", len(nonce), "ts", ts)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
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
			// R218-SEC-1: gate url_verification through hookSem like every
			// other branch below. Without this, a leaked verification_token
			// lets an attacker flood challenge endpoints (each request still
			// passes auth) without ever hitting the concurrent-handler cap;
			// since challenges run synchronously on the HTTP goroutine this
			// only bounds in-flight challenge replies, but matches the
			// semaphore contract the rest of the handler relies on.
			select {
			case f.hookSem <- struct{}{}:
			default:
				slog.Warn("feishu webhook: handler semaphore full, dropping url_verification")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer func() { <-f.hookSem }()
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
			// R182-SEC-L3: utf8.ValidString only rejects malformed UTF-8,
			// not valid-but-hazardous runes (C0/C1/bidi override/LS/PS).
			// Feishu challenges are documented as opaque ASCII tokens, so
			// rejecting anything that would be sanitized in logs has zero
			// false-positive risk and mirrors the nonce sweep above.
			for _, r := range envelope.Challenge {
				if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
					slog.Warn("feishu challenge contains control/bidi rune")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			// SetEscapeHTML(false) mirrors feishu.go buildMarkdownCardJSON:
			// Feishu's verification endpoint compares our response against
			// the raw challenge it sent, and default HTML-entity escaping
			// of `<`, `>`, `&` could make a challenge containing those
			// characters fail to match. Challenges already went through
			// the control/bidi sweep above so no injection risk.
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(map[string]string{"challenge": envelope.Challenge}); err != nil {
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
		// Interactive-card button click from an AskUserQuestion card. Route
		// it through the card_action branch instead of dropping it; on
		// success the handler synthesises an IncomingMessage whose Text is
		// the chosen option so the answer flows through the same dispatch
		// path as a regular chat reply. Run on hookSem + f.wg like other
		// message types so a burst of card clicks cannot exhaust HTTP
		// server goroutines, and graceful shutdown can wait for in-flight
		// dispatch. R218-SEC-P1.
		if eventType == "card.action.trigger" || eventType == "im.card.action.v1_trigger" {
			select {
			case f.hookSem <- struct{}{}:
			default:
				slog.Warn("feishu webhook: handler semaphore full, dropping card action")
				return
			}
			f.wg.Add(1)
			rawEvent := envelope.Event
			go func() {
				defer f.wg.Done()
				defer func() { <-f.hookSem }()
				defer platform.RecoverHandler("feishu card_action")
				f.handleCardActionWebhook(f.stopCtx, rawEvent, handler)
			}()
			return
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
		// R186-SEC-L: cap eventID length before it reaches Dedup.Seen, which
		// stores the raw string as a map key. Feishu event_ids are UUID-ish
		// (~36 bytes); the nonce+timestamp fallback tops out at ~64. A tampered
		// or malicious upstream (or a future Feishu schema bump emitting larger
		// IDs) could otherwise feed the dedup map with 64 KiB keys up to the
		// per-bucket cap (50000), i.e. ~3 GiB heap worst-case. Drop the ID
		// (skip dedup) so a single replayed event may double-process but the
		// server stays within memory budget.
		if len(eventID) > maxEventIDLen {
			slog.Warn("feishu webhook: event_id too long, skipping dedup for this delivery",
				"len", len(eventID))
			eventID = ""
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
					"err", err, "msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64))
				return
			}
			text := content.Text
			// Feishu's own upstream limit on message text is ~4000 bytes
			// (~1333 CJK chars); anything larger is either a misconfigured
			// client or attacker-crafted payload. Reject at 8 KiB (2x the
			// official limit) so we don't ferry multi-KB slog attrs or push
			// oversized messages into the downstream CLI stdin path.
			// maxIncomingTextBytes is shared with transport_ws.go.
			if len(text) > maxIncomingTextBytes {
				slog.Warn("feishu webhook: text exceeds limit, dropping",
					"msg_id", osutil.SanitizeForLog(event.Message.MessageID, 64), "len", len(text))
				return
			}
			// Strip all @-mention tokens in a single pass. Previously each
			// ReplaceAll allocated a fresh string and copied the whole text;
			// a group message with multiple @ users did that N times.
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
					// R190-SEC-M3: content.ImageKey is attacker-controlled (a
					// Feishu workspace member can craft a message with any
					// image_key string). Sanitize before slog so C1/bidi/LS/PS
					// runes can't fragment structured-log fields.
					slog.Error("feishu download image failed", "err", err,
						"key", osutil.SanitizeForLog(content.ImageKey, 128))
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

// isValidFeishuResourceKey accepts the narrow character set Feishu uses for
// image_key / file_key identifiers. Real keys are opaque printable ASCII
// (base62-ish) capped at ~100 bytes; reject anything with whitespace, C0/C1
// control bytes, or non-ASCII so a message from an attacker-controlled
// sender cannot smuggle structured-log-splitting bytes or oversized payloads
// into the Feishu API URL builder.
func isValidFeishuResourceKey(k string) bool {
	if k == "" || len(k) > 256 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		// Printable ASCII excluding whitespace. Hyphen and underscore are
		// common in real keys; accept them explicitly via the range test.
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// constantTimeEqualString compares two strings in constant time without leaking
// their lengths. subtle.ConstantTimeCompare returns 0 immediately when operand
// lengths differ, which allows an attacker to probe the configured token's
// length via timing. Hashing both sides to a fixed-length SHA-256 digest first
// equalises lengths before the constant-time compare, at the cost of two
// extra hashes per request.
func constantTimeEqualString(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
