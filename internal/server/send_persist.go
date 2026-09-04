// attachment 持久化批处理：persistErr (status, msg) 错误对 + persistFileRefs
// （KindFileRef 写入 workspace + best-effort 持久化 inline image）。
// SendHandler.handleSend / handleUpload 调用的纯 helper。
package server

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
)

// persistErr is the (status, msg) pair returned from persistFileRefs.
type persistErr struct {
	status int
	msg    string
}

// persistFileRefs writes every Kind==KindFileRef entry's Data to the session
// workspace via attachment.Persist and returns a new slice where those
// entries carry WorkspacePath with Data cleared. Inline images pass through
// unchanged but are also best-effort persisted so the dashboard lightbox can
// load the original via /api/sessions/attachment; a failed image persist only
// degrades that affordance.
//
// rollback removes every file Persist wrote; callers must invoke it on any
// failure path before sessionSend accepts the request, or the files leak
// until the GC sweep. An empty workspace is refused with a readable 400.
func persistFileRefs(workspace string, atts []cli.Attachment, sessionKey, owner string) ([]cli.Attachment, func(), *persistErr) {
	if workspace == "" {
		return nil, nil, &persistErr{status: http.StatusBadRequest, msg: "workspace is required for file attachments"}
	}

	out := make([]cli.Attachment, len(atts))
	// written tracks absPaths across the batch so rollback can remove
	// every file if a later element fails.
	written := make([]string, 0, len(atts))
	rollback := func() {
		for _, p := range written {
			attachment.Remove(p)
		}
	}

	for i, a := range atts {
		if a.Kind != cli.KindFileRef {
			out[i] = a
			// Inline images: best-effort persist for the lightbox "view
			// original" URL. out[i].Data is deliberately retained — the
			// inline path ships bytes to the CLI as a content block.
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
		// Extension allowlist; only PDF is live today.
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
			// Generic client message: exposing the path would leak workspace layout.
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
			// Data intentionally nil: coalesce/dispatch copies this slice
			// repeatedly and only needs the path.
		}
	}
	return out, rollback, nil
}
