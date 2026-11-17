package discord

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestCDNTransport_ReachesLoopbackProxy: a download goes through the
// operator's proxy on loopback, which the reserved-IP guard used to refuse
// (every Discord attachment failed while the gateway worked); a direct dial
// to a reserved IP is still refused.
func TestCDNTransport_ReachesLoopbackProxy(t *testing.T) {
	t.Parallel()
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "cdn.discordapp.com" {
			http.Error(w, "unexpected target "+r.URL.Host, http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, "via proxy")
	}))
	t.Cleanup(proxySrv.Close)
	proxyURL, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: newCDNTransport(http.ProxyURL(proxyURL))}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://cdn.discordapp.com/attachments/1/2/a.png", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("download through the loopback proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "via proxy" {
		t.Fatalf("status %d body %q, want 200 via proxy", resp.StatusCode, body)
	}

	direct := &http.Client{Transport: newCDNTransport(nil)}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, proxySrv.URL, nil)
	if resp, err := direct.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("direct dial to a loopback address got through the guard")
	} else if !strings.Contains(err.Error(), "reserved IP") {
		t.Fatalf("direct dial error = %v, want the reserved-IP refusal", err)
	}
}
