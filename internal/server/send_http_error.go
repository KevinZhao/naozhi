package server

import "net/http"

// User-facing 429 labels for the HTTP send path. They name the limiter that
// fired so the dashboard can display the body verbatim; before #2418's
// follow-up (F6) it collapsed every 429 into "消息队列已满", which none of
// these sources is.
const (
	sendRateLimitedMsg   = "发送过于频繁，请稍后重试"
	uploadRateLimitedMsg = "上传过于频繁，请稍后重试"
)

// writeSendError writes the standard {"error": msg} envelope for a rejected
// send. When filesConsumed is set — i.e. the request's pre-uploaded
// attachments were already taken out of the uploadStore by TakeAll — it adds
// "files_consumed": true so the client knows a blind retry with the same
// file_ids would fail with "file not found or expired" and must drop its stale
// chips instead (#2418 follow-up F3). Rejections that run BEFORE TakeAll keep
// the files and must not pass filesConsumed.
func writeSendError(w http.ResponseWriter, status int, msg string, filesConsumed bool) {
	body := map[string]any{"error": msg}
	if filesConsumed {
		body["files_consumed"] = true
	}
	writeJSONStatus(w, status, body)
}
