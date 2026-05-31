package dispatch

import "github.com/naozhi/naozhi/internal/apierr"

// localizeAPIError is a thin wrapper around apierr.Localize kept for
// backward compatibility with existing call sites inside this package.
// The canonical implementation lives in internal/apierr so that packages
// outside dispatch (e.g. internal/cron) can consume it without creating
// an import cycle.
func localizeAPIError(text string) string { return apierr.Localize(text) }
