package cli

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
)

// A custody message's transcript entry carries its source, reason and count
// from the turn's ctx; its text is the message alone.
func TestSend_CustodyEntry(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	writes := startServerWriteTap(srv)
	p.startReadLoop()
	t.Cleanup(func() { p.Kill(); srv.Close() })
	ctx := ctxutil.WithCustody(ctxutil.WithSource(context.Background(), clievent.SourceCustody), ctxutil.Custody{Reason: "出错重试", N: 2})
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Send(ctx, "继续", nil, nil)
		errCh <- err
	}()
	awaitWrite(t, writes)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)
	if err := awaitSendErr(t, errCh); err != nil {
		t.Fatal(err)
	}
	for _, e := range p.EventLastN(8) {
		if e.Type != clievent.KindUser {
			continue
		}
		if e.Detail != "继续" || e.Source != clievent.SourceCustody || e.CustodyReason != "出错重试" || e.CustodyN != 2 {
			t.Errorf("user entry = %+v", e)
		}
		return
	}
	t.Fatal("no user entry")
}
