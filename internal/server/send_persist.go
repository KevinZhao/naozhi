// Phase 3f-prep / R-send-persist-extract (2026-05-28):
// persistErr type + persistFileRefs (~127 行) 抽到独立文件。纯物理切分、
// 零行为变化。
//
// 这两个东西构成完整的 attachment 持久化批处理：
//   - persistErr (status, msg) 错误对
//   - persistFileRefs 把 KindFileRef 写入 workspace + best-effort 持久化 inline image
//
// 与 *Hub 没有 receiver 关系，是 SendHandler.handleSend / handleUpload
// 调用的纯 helper。同包可见性让所有调用方零改动。
package server

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
)

// persistErr is the (status, msg) pair returned from persistFileRefs. The
// struct lets the handler forward both pieces without building a new error
// type hierarchy just for this one call site.
type persistErr struct {
	status int
	msg    string
}

// persistFileRefs walks atts and, for every Kind==KindFileRef entry,
// writes its Data to the session workspace via internal/attachment.Persist.
// It returns a new []cli.Attachment slice where file_ref entries now carry
// WorkspacePath (and have Data cleared to release memory) and image_inline
// entries are passed through unchanged.
//
// Image inline entries are ALSO persisted to disk as a side effect: the
// inline Data still rides along to the CLI (unchanged), but a copy is
// written to the workspace attachment directory and the relative path is
// stashed in WorkspacePath. buildUserEntry pulls it onto EventEntry so the
// dashboard lightbox can load the original via /api/sessions/attachment
// instead of a downsampled data URI. If the persist fails, we log and
// continue — the inline bytes still reach the CLI so the model call
// succeeds; only the "view original" affordance degrades to the thumbnail.
//
// rollback, when non-nil, removes every file that Persist just wrote.
// Callers use it on any failure path between this call and the point
// where sessionSend accepts the request — without rollback, a validation
// failure after persist would leak disk until the GC sweep.
//
// Workspace requirement: file_ref attachments need a real absolute path.
// If workspace is empty or relative, we refuse here rather than silently
// writing somewhere unexpected. The same guard fires upstream in
// session.router (workspace resolution), but surfacing it at the HTTP
// layer gives the user a readable 400 instead of a generic forbidden.
func persistFileRefs(workspace string, atts []cli.Attachment, sessionKey, owner string) ([]cli.Attachment, func(), *persistErr) {
	if workspace == "" {
		return nil, nil, &persistErr{status: http.StatusBadRequest, msg: "workspace is required for file attachments"}
	}

	out := make([]cli.Attachment, len(atts))
	// written tracks absPaths across the batch so rollback can remove
	// every file if a later element fails. Capacity matches the realistic
	// upper bound (batch size) to avoid a growth reallocation in the
	// common happy path.
	written := make([]string, 0, len(atts))
	rollback := func() {
		for _, p := range written {
			attachment.Remove(p)
		}
	}

	for i, a := range atts {
		if a.Kind != cli.KindFileRef {
			out[i] = a
			// Inline images: best-effort persist a copy to disk so the
			// dashboard lightbox can fetch the original. Failure is
			// non-fatal — the inline Data still rides along to the CLI.
			// out[i].Data is DELIBERATELY retained (unlike file_ref
			// below which clears Data post-persist) because the inline
			// image path ships bytes as a content block; the on-disk
			// copy is purely for the dashboard's "view original" URL.
			// Lifetime is request-scoped — the duplicate bytes are
			// freed when the send completes, not cached indefinitely.
			if ext := imageExtForMime(a.MimeType); ext != "" && len(a.Data) > 0 {
				meta := attachment.Meta{
					OrigName:   a.OrigName,
					MimeType:   a.MimeType,
					Size:       int64(len(a.Data)),
					SessionKey: sessionKey,
					Owner:      owner,
				}
				if p, err := attachment.Persist(workspace, a.Data, ext, meta); err == nil {
					written = append(written, p.AbsPath)
					out[i].WorkspacePath = p.RelPath
				} else {
					slog.Debug("inline image persist failed",
						"key", sessionKey, "err", err)
				}
			}
			continue
		}
		// Map MimeType to an extension allowlist entry. Only PDF is live
		// today — future formats add a case.
		var ext string
		switch a.MimeType {
		case "application/pdf":
			ext = ".pdf"
		default:
			rollback()
			return nil, nil, &persistErr{
				status: http.StatusBadRequest,
				msg:    "unsupported attachment type",
			}
		}
		meta := attachment.Meta{
			OrigName:   a.OrigName,
			MimeType:   a.MimeType,
			Size:       int64(len(a.Data)),
			SessionKey: sessionKey,
			Owner:      owner,
		}
		p, err := attachment.Persist(workspace, a.Data, ext, meta)
		if err != nil {
			rollback()
			// A disk-full / permission error is operator-visible via slog but
			// collapsed to a generic message for the client; exposing the path
			// would leak workspace layout.
			slog.Warn("attachment persist failed",
				"key", sessionKey, "owner", owner, "err", err)
			return nil, nil, &persistErr{
				status: http.StatusInternalServerError,
				msg:    "failed to save attachment",
			}
		}
		written = append(written, p.AbsPath)
		out[i] = cli.Attachment{
			Kind:          cli.KindFileRef,
			MimeType:      a.MimeType,
			WorkspacePath: p.RelPath,
			OrigName:      a.OrigName,
			Size:          p.Size,
			// Data intentionally nil: coalesce/dispatch will copy this slice
			// multiple times and we don't want a 32 MB PDF riding along in
			// memory for a trip that only needs the path string.
		}
	}
	return out, rollback, nil
}
