package planner

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Files resolves the file_ids of a message to the Planner the way a session
// send resolves its own (the server's upload store): the same per-send cap,
// upload-owner check and atomic take, and a PDF persisted into the Planner's
// working directory. On a refusal Take has written the response itself
// (status, error text and files_consumed as a session send writes them) and
// reports ok=false. rollback (nil for none) removes what it wrote to disk,
// for a message the Planner then refuses.
type Files interface {
	Take(w http.ResponseWriter, r *http.Request, ids []string) (files []clievent.Attachment, rollback func(), ok bool)
}
