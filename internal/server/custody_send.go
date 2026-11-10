package server

import (
	"errors"
	"fmt"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// errCustodyBusy answers a custody message the session could not take.
var errCustodyBusy = errors.New("会话忙或服务正在关闭，托管消息未送达")

// errUndoBusy answers an undo the session could not take.
var errUndoBusy = errors.New("会话忙或服务正在关闭，撤回消息未送达")

// errRelayBusy answers a relay the session could not take.
var errRelayBusy = errors.New("会话忙或服务正在关闭，转达未送达")

// RelayTarget is why key may not take the user's decision the Planner
// relays (docs/rfc/planner.md D98), nil when it may: a local session, not
// the Planner's own, a scout run's, a cron run's or a daemon's. Relaying
// never creates a session. It is action.Config.RelayTarget.
func (s *Server) RelayTarget(key string) error {
	switch {
	case sessionkey.ValidateSessionKey(key) != nil:
		return fmt.Errorf("%q 不是合法的会话 key", key)
	case sessionkey.IsPlannerNSKey(key), sessionkey.IsOneShotKey(key), sessionkey.IsCronKey(key), sessionkey.IsSysKey(key):
		return fmt.Errorf("会话 %s 不接收转达", key)
	case s.router == nil || s.router.SessionFor(key) == nil:
		return fmt.Errorf("找不到会话 %s（填 index.json 里本机会话的 key；远程节点的会话不能转达）", key)
	}
	return nil
}

// SubmitRelay sends text, the gateway's relay envelope, to key on the
// dashboard's own send path, its transcript entry marked
// clievent.SourceRelay: a session whose process went away resumes as for
// the user's message, a busy one takes it into its next turn. Unlike custody
// it is neither counted nor held back while the user is at the session: the
// user asked for it. It is action.Config.Relay.
func (s *Server) SubmitRelay(key, text string) error {
	if err := s.RelayTarget(key); err != nil {
		return err
	}
	return s.submit(sendParams{Key: key, Text: text, Source: clievent.SourceRelay}, errRelayBusy)
}

// SubmitCustody sends text to key as custody's n-th message over its
// lineage, for reason (internal/planner/custody): the dashboard's own send
// path, so a session whose process went away resumes as it would for the
// user's message, its transcript entry marked clievent.SourceCustody with
// the reason and n. It is custody.Config.Send.
func (s *Server) SubmitCustody(key, text, reason string, n int) error {
	return s.submit(sendParams{Key: key, Text: text, Source: clievent.SourceCustody, Custody: ctxutil.Custody{Reason: reason, N: n}}, errCustodyBusy)
}

// SubmitUser sends text to key as the user's own message, on the same path:
// the undo the user clicks of an answer custody gave on their behalf. It is
// custody.Config.SendUser.
func (s *Server) SubmitUser(key, text string) error {
	return s.submit(sendParams{Key: key, Text: text}, errUndoBusy)
}

func (s *Server) submit(p sendParams, busy error) error {
	if s.hub == nil || s.hub.engine == nil {
		return errPlannerNoEngine
	}
	e := s.hub.engine
	_, ack, err := e.sessionSend(p, e.httpOrigin(p.Key))
	switch {
	case err != nil:
		return err
	case ack == sendAckBusy:
		return busy
	}
	return nil
}
