package server

import (
	"strconv"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// ViewingSink takes the dashboard's "the user has this session open"
// reports: custody holds back while the user is at a session
// (custody.Engine), opening one closes its "后台任务跑完了"
// (notify.Notifier) and marks its new result read (task.Board).
type ViewingSink interface {
	SetViewing(key, viewer string, viewing bool)
	DropViewer(viewer string)
}

// viewingSinks hands each report to every sink.
type viewingSinks []ViewingSink

func (s viewingSinks) SetViewing(key, viewer string, viewing bool) {
	for _, v := range s {
		v.SetViewing(key, viewer, viewing)
	}
}

func (s viewingSinks) DropViewer(viewer string) {
	for _, v := range s {
		v.DropViewer(viewer)
	}
}

// viewerSeq names each connection as a viewer.
var viewerSeq atomic.Uint64

func nextViewer() string { return "ws" + strconv.FormatUint(viewerSeq.Add(1), 10) }

// handleViewing takes a viewing frame: the session c has open in a visible,
// focused tab, "" for none. One connection views one session at a time, so
// a new one ends the last; the same one again renews it. Only this node's
// sessions are in custody: a remote node's reads as none.
func (h *Hub) handleViewing(c *wsClient, msg node.ClientMsg) {
	if h.viewing == nil {
		return
	}
	key := msg.Key
	if (msg.Node != "" && msg.Node != "local") || sessionkey.ValidateSessionKey(key) != nil {
		key = ""
	}
	if c.viewing != "" && c.viewing != key {
		h.viewing.SetViewing(c.viewing, c.viewer, false)
	}
	c.viewing = key
	if key != "" {
		h.viewing.SetViewing(key, c.viewer, true)
	}
}
