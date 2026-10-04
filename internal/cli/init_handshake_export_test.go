package cli

import (
	"io"
	"testing"
)

// InitHandshakeErrForTest runs the ACP Init handshake against a shim that
// sends frames and returns the error Spawn would, so package cli_test can
// feed the real chain to usermsg (which imports cli through session).
func InitHandshakeErrForTest(t testing.TB, frames ...string) error {
	t.Helper()
	p, srv := shimTestPair(&ACPProtocol{})
	t.Cleanup(func() { srv.Close(); p.link.conn.Close() })
	go io.Copy(io.Discard, srv.conn) //nolint:errcheck // drains the handshake requests
	go func() {
		for _, f := range frames {
			srv.SendFrame(f)
		}
	}()
	_, err := initHandshake(&ACPProtocol{}, p, "", "/tmp/work")
	return err
}
