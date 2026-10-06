// turn_sender_files.go — IM file attachments reach turnSender.Send as
// pending file refs (bytes in Data, no WorkspacePath); they are written into
// the session's own cwd here, the one place the CLI's real workspace is
// known, so the Read hint's relative path resolves.
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
)

// isPendingFileRef reports a file ref whose bytes are not on disk yet.
// Dashboard refs arrive persisted (WorkspacePath set, Data nil) and are
// never written twice.
func isPendingFileRef(a clievent.Attachment) bool {
	return a.Kind == clievent.KindFileRef && a.WorkspacePath == "" && len(a.Data) > 0
}

// persistPendingFileRefs writes every pending file ref into workspace and
// returns atts with those entries carrying WorkspacePath and no Data. A ref
// that cannot be written is dropped and named in note, a line to prepend to
// the turn's text so the model can tell the user; the turn itself still
// runs. atts is returned as is when nothing is pending. Files stay on disk
// even if the send then fails: the CLI may already have read the hint.
func persistPendingFileRefs(workspace string, atts []clievent.Attachment, key string) (out []clievent.Attachment, note string) {
	pending := 0
	for _, a := range atts {
		if isPendingFileRef(a) {
			pending++
		}
	}
	if pending == 0 {
		return atts, ""
	}
	wsErr := checkFileRefWorkspace(workspace)
	out = make([]clievent.Attachment, 0, len(atts))
	var lost []string
	for _, a := range atts {
		if !isPendingFileRef(a) {
			out = append(out, a)
			continue
		}
		err := wsErr
		var p attachment.Persisted
		if err == nil {
			if ext := attachment.ExtForMime(a.MimeType); ext == "" {
				err = fmt.Errorf("no extension for %q", a.MimeType)
			} else {
				p, err = attachment.Persist(workspace, a.Data, ext, attachment.Meta{
					OrigName:   a.OrigName,
					MimeType:   a.MimeType,
					Size:       int64(len(a.Data)),
					SessionKey: key,
				})
			}
		}
		if err != nil {
			slog.Warn("send: file attachment not saved", "key", session.SanitizeLogAttr(key), "err", err)
			name := a.OrigName
			if name == "" {
				name = "a file"
			}
			lost = append(lost, name)
			continue
		}
		out = append(out, clievent.Attachment{
			Kind:          clievent.KindFileRef,
			MimeType:      a.MimeType,
			WorkspacePath: p.RelPath,
			OrigName:      a.OrigName,
			Size:          p.Size,
		})
	}
	if len(lost) > 0 {
		note = "[System: The user attached " + strings.Join(lost, ", ") +
			" but naozhi could not save it to the workspace. Tell the user it did not arrive.]\n\n"
	}
	return out, note
}

// checkFileRefWorkspace refuses a workspace Persist would otherwise create:
// MkdirAll under a missing cwd would mask a session whose cwd is gone.
func checkFileRefWorkspace(workspace string) error {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return errors.New("session workspace is not an absolute path")
	}
	fi, err := os.Stat(workspace)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("session workspace is not a directory")
	}
	return nil
}
