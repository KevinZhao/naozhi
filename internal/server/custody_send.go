package server

import (
	"errors"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// errCustodyBusy answers a custody message the session could not take.
var errCustodyBusy = errors.New("会话忙或服务正在关闭，托管消息未送达")

// errUndoBusy answers an undo the session could not take.
var errUndoBusy = errors.New("会话忙或服务正在关闭，撤回消息未送达")

// SubmitCustody sends text to key as custody's message
// (internal/planner/custody): the dashboard's own send path, so a session
// whose process went away resumes as it would for the user's message, its
// transcript entry marked clievent.SourceCustody. It is custody.Config.Send.
func (s *Server) SubmitCustody(key, text string) error {
	return s.submit(key, text, clievent.SourceCustody, errCustodyBusy)
}

// SubmitUser sends text to key as the user's own message, on the same path:
// the undo the user clicks of an answer custody gave on their behalf. It is
// custody.Config.SendUser.
func (s *Server) SubmitUser(key, text string) error {
	return s.submit(key, text, "", errUndoBusy)
}

func (s *Server) submit(key, text, source string, busy error) error {
	if s.hub == nil || s.hub.engine == nil {
		return errPlannerNoEngine
	}
	e := s.hub.engine
	_, ack, err := e.sessionSend(sendParams{Key: key, Text: text, Source: source}, e.httpOrigin(key))
	switch {
	case err != nil:
		return err
	case ack == sendAckBusy:
		return busy
	}
	return nil
}
