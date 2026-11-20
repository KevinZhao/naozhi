package httputil

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// proxyHeaders are the headers a forwarding hop adds; a request carrying any
// of them came through one, whatever its RemoteAddr says.
var proxyHeaders = []string{"Forwarded", "Via", "X-Real-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "Fastly-Client-Ip"}

// DirectLoopback reports whether r reached this node's own listener straight
// from a loopback address: the one kind of request the writes that stay on
// the machine are taken from (the 懂你 section's, planner RFC D126 decision
// B; the ledger of the lines of work, D134). Only RemoteAddr counts; a
// forwarding header (which anyone may send) never makes a request local and
// always makes it remote, so a proxy on this machine (or EC2's load
// balancer) is remote too. A reverse-node `api` call has no RemoteAddr and
// is remote.
func DirectLoopback(r *http.Request) bool {
	for k := range r.Header {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if strings.HasPrefix(ck, "X-Forwarded-") {
			return false
		}
		for _, p := range proxyHeaders {
			if ck == p {
				return false
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
