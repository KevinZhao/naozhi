package server

import "net/http"

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
