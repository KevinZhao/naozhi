// planner_files.go — the file_ids of a message to the Planner
// (POST /api/planner/send, internal/dashboard/ext/planner), resolved as a
// session send resolves its own.
package server

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/planner/agent"
)

// plannerFiles is dashplanner.Files over the upload store the session send
// uses. A PDF lands in workspace, the Planner's working directory (inside
// the tree it may Read). An image's original is kept under imageDir, the
// state root's planner/attachments where the Planner's tool images go: the
// working directory is outside allowed_root, so the lightbox's "view
// original" could not be served from it.
type plannerFiles struct {
	store        *uploadStore
	auth         *auth.Handlers
	trustedProxy bool
	workspace    func() string
	imageDir     string
}

// plannerImageDir is imageDir for the state root stateDir, "" for none.
func plannerImageDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return datadir.FromRoot(stateDir).PlannerAttachmentsRoot()
}

// Take mirrors handleSend's file_ids steps: the per-send cap, the upload
// owner, the atomic TakeAll and the persist, with its statuses and texts.
func (p plannerFiles) Take(w http.ResponseWriter, r *http.Request, ids []string) ([]clievent.Attachment, func(), bool) {
	if len(ids) > maxFilesPerSend {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errTooManyFiles})
		return nil, nil, false
	}
	owner, ok := uploadOwnerOrFail(w, r, p.auth, p.trustedProxy)
	if !ok {
		return nil, nil, false
	}
	// Never echo a client-supplied fid in the error. TakeAll is atomic: a
	// missing, expired or foreign fid consumes nothing.
	images, err := p.store.TakeAll(ids, owner)
	if err != nil {
		slog.Debug("planner send: one or more file_ids not found or expired", "count", len(ids))
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file not found or expired"})
		return nil, nil, false
	}

	var refs []int
	for i, a := range images {
		if a.Kind == clievent.KindFileRef {
			refs = append(refs, i)
		}
	}
	var refRollback func()
	if len(refs) > 0 {
		sub := make([]clievent.Attachment, len(refs))
		for j, i := range refs {
			sub[j] = images[i]
		}
		resolved, rb, perr := persistFileRefs(p.workspace(), sub, agent.Key, owner)
		if perr != nil {
			writeSendError(w, perr.status, perr.msg, true)
			return nil, nil, false
		}
		for j, i := range refs {
			images[i] = resolved[j]
		}
		refRollback = rb
	}

	// Best-effort, as persistFileRefs keeps a session image's original: a
	// failure only costs the lightbox its full-size view.
	var kept []string
	for i := range images {
		a := &images[i]
		ext := imageExtForMime(a.MimeType)
		if a.Kind == clievent.KindFileRef || ext == "" || len(a.Data) == 0 || p.imageDir == "" {
			continue
		}
		st, err := attachment.PersistIn(p.imageDir, a.Data, ext, attachment.Meta{
			OrigName: a.OrigName, MimeType: a.MimeType, Size: int64(len(a.Data)),
			SessionKey: agent.Key, Owner: owner,
		})
		if err != nil {
			slog.Debug("planner send: image original not kept", "err", err)
			continue
		}
		kept = append(kept, st.AbsPath)
		a.WorkspacePath = datadir.PlannerAttachmentsRel + "/" + st.RelPath
	}
	rollback := func() {
		if refRollback != nil {
			refRollback()
		}
		for _, path := range kept {
			attachment.Remove(path)
		}
	}
	return images, rollback, true
}
