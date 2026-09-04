package dispatch

import "github.com/naozhi/naozhi/internal/apierr"

// localizeAPIError wraps apierr.Localize; the canonical implementation lives
// in internal/apierr so internal/cron can use it without an import cycle.
func localizeAPIError(text string) string { return apierr.Localize(text) }
