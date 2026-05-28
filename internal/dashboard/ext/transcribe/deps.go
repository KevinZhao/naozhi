// Package transcribe hosts the dashboard /api/transcribe endpoint that
// converts dashboard-uploaded audio to text. Phase 3d
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package transcribe

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// IPLimiter is the subset of internal/server.ipLimiter the transcribe
// handler uses.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// maxMultipartFields caps non-file form entries in a single multipart
// upload. Phase 3d duplicated from internal/server/dashboard_send.go's
// constant. Currently only 3 known transcribe form fields, the cap of 32
// leaves slack for future fields without enabling unbounded growth.
const maxMultipartFields = 32

// rejectIfTooManyFields writes 400 + returns true when the parsed multipart
// form carries more than maxMultipartFields non-file entries. Callers must
// invoke this immediately after ParseMultipartForm and bail out on a true
// return.
//
// Phase 3d duplicated from internal/server/dashboard_send.go to avoid
// reverse-import. The two implementations stay equivalent; if the policy
// shifts (e.g. per-handler caps), they can diverge here without coupling.
func rejectIfTooManyFields(w http.ResponseWriter, r *http.Request) bool {
	if r.MultipartForm == nil {
		return false
	}
	total := 0
	for _, vs := range r.MultipartForm.Value {
		total += len(vs)
		if total > maxMultipartFields {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many form fields"})
			return true
		}
	}
	return false
}
