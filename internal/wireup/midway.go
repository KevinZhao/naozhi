package wireup

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/midway"
)

// MidwayExpiry is when this machine's Midway session expires, zero without
// one (notify.Config.Midway, custody.Config.CredExpiry): the file is read
// each time, so an mwinit shows at the next look.
func MidwayExpiry() time.Time {
	exp, err := midway.Expiry(midway.CookiePath())
	if err != nil {
		slog.Debug("midway: cookie expiry unreadable", "err", err)
	}
	return exp
}
