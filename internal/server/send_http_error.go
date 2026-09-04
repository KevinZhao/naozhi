package server

import "net/http"

// User-facing 429 labels naming the limiter that fired; shown verbatim (#2418).
const (
	sendRateLimitedMsg   = "发送过于频繁，请稍后重试"
	uploadRateLimitedMsg = "上传过于频繁，请稍后重试"
)

// writeSendError writes the standard {"error": msg} envelope for a rejected
// send. filesConsumed=true means TakeAll already removed the pre-uploaded
// attachments, so the client must drop its stale chips rather than retry the
// same file_ids (#2418); rejections that run BEFORE TakeAll must pass false.
func writeSendError(w http.ResponseWriter, status int, msg string, filesConsumed bool) {
	body := map[string]any{"error": msg}
	if filesConsumed {
		body["files_consumed"] = true
	}
	writeJSONStatus(w, status, body)
}
