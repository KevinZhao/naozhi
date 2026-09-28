package backend

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// turnRejectFixture is, for one backend, the frame its CLI sends when it
// rejects the in-flight turn over RPC, or the reason it has no such frame.
type turnRejectFixture struct {
	frame string
	none  string
}

// turnRejectFixtures covers every backend RegisterDefaults registers. A
// protocol that answers a rejection with a plain error leaves the session in
// state=running, so a new backend must show here that its rejection frame
// comes back as a cli.TurnRejectedError, or say why it has none.
var turnRejectFixtures = map[string]turnRejectFixture{
	"claude": {none: "stream-json reports a failed turn as a result event; there is no RPC reply to reject a turn"},
	"kiro":   {frame: `{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"model overloaded"}}`},
	"codex":  {frame: `{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"Server overloaded"}}`},
}

// TestAll_TurnRejectionClosesTheTurn: every registered backend either turns
// its rejection frame into a cli.TurnRejectedError tagged with its own ID, or
// declares it has no rejection path.
func TestAll_TurnRejectionClosesTheTurn(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()
		for _, p := range All() {
			fx, ok := turnRejectFixtures[p.ID]
			if !ok {
				t.Errorf("backend %q has no turnRejectFixtures entry: add the frame its CLI sends when it rejects a turn (and return cli.TurnRejectedError for it), or a none reason", p.ID)
				continue
			}
			if fx.none != "" {
				continue
			}
			_, _, err := p.NewProtocol(ProtocolDeps{}).ReadEvent(fx.frame)
			var rejected *cli.TurnRejectedError
			if !errors.As(err, &rejected) {
				t.Errorf("backend %q: ReadEvent(rejection frame) err = %v, want a *cli.TurnRejectedError", p.ID, err)
				continue
			}
			if rejected.Backend != p.ID {
				t.Errorf("backend %q: rejection tagged %q, want its own ID", p.ID, rejected.Backend)
			}
		}
	})
}
