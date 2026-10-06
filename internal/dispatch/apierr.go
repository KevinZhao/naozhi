package dispatch

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// localizeAPIError wraps apierr.Localize; the canonical implementation lives
// in internal/apierr so internal/cron can use it without an import cycle.
func localizeAPIError(text string) string { return apierr.Localize(text) }

// turnReplyText is the reply a turn's result gets before decoration: the
// failure notice when usermsg has one, else the answer with secrets redacted
// before an API error in it is localized (#1571). An is_error answer is
// localized without the envelope prefix check. A failed turn is logged and
// counted by class, never with the backend's raw message. answer reports
// whether text is the turn's answer rather than a notice or error text.
func turnReplyText(ctx context.Context, r *clievent.SendResult) (text string, answer bool) {
	notice, class := usermsg.ForTurnResult(r)
	text = textutil.RedactSecrets(r.Text)
	switch {
	case notice != "":
		text = "⚠️ " + notice
	case r.IsError && text != "":
		class = "error_text"
		text, _ = apierr.LocalizeError(text)
	default:
		return localizeAPIError(text), true
	}
	attrs := []any{"class", class, "subtype", r.SubType}
	if be := r.BackendError; be != nil {
		attrs = append(attrs, "backend", be.Backend, "rpc_code", be.Code)
	}
	slog.WarnContext(ctx, "turn ended in failure", attrs...)
	dispatchTurnErrorResultTotal.Add(class, 1)
	return text, false
}
