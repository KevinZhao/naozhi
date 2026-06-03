package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultBaseURL         = "https://ilinkai.weixin.qq.com"
	defaultLongPollTimeout = 35 * time.Second
	defaultAPITimeout      = 15 * time.Second
	channelVersion         = "naozhi-1.0.0"
)

// baseInfo is attached to every outgoing API request.
// Without this field, iLink server falls back to one-shot mode
// and silently drops all sendMessage calls after the first one.
type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// apiClient wraps the iLink Bot HTTP API.
type apiClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// ssrfDialGuard wraps a base DialContext so every resolved address is
// re-validated against the loopback/private/link-local SSRF deny-set BEFORE
// the TCP connection is established. The config-time guard in
// internal/config/config.go only rejects *literal* private IPs in base_url;
// a hostname like wechat.example.com sails past it and is resolved by the OS
// resolver at dial time — so an attacker who controls the DNS record (or an
// internal DNS misconfig) could still steer the request at 169.254.169.254
// (EC2 IMDS) or an internal admin port. Hooking DialContext is the only place
// that sees the *resolved* IP, so the guard belongs here. R20260603040203-SEC-10.
//
// enabled is false for explicitly-configured loopback dev mocks (httptest /
// local relays) — validateBaseURLScheme already greenlights those, and a
// blanket guard would refuse to dial 127.0.0.1, breaking local development.
func ssrfDialGuard(base func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return ssrfDialGuardWithResolver(base, func(ctx context.Context, network, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, network, host)
	})
}

// ssrfDialGuardWithResolver is the injectable form used by tests.
// resolver(ctx, "ip", host) must return the candidate IPs for host.
func ssrfDialGuardWithResolver(
	base func(ctx context.Context, network, addr string) (net.Conn, error),
	resolver func(ctx context.Context, network, host string) ([]net.IP, error),
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("weixin dial: split %q: %w", addr, err)
		}
		// addr handed to DialContext already has the host resolved to a
		// literal IP only when the resolver ran; when it's still a name the
		// default resolver is about to resolve it, so resolve here ourselves
		// and validate every candidate IP.
		if ip := net.ParseIP(host); ip != nil {
			if err := rejectInternalIP(ip); err != nil {
				return nil, err
			}
			return base(ctx, network, addr)
		}
		ips, err := resolver(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("weixin dial: resolve %q: %w", host, err)
		}
		// DNS-rebinding defence: pass the already-validated IP literal to base
		// instead of the original hostname. Without this, the OS resolver runs a
		// second lookup inside base; a TTL=0 rebind can return 169.254.169.254
		// on that second query even though our check above saw a public IP.
		var lastRejectErr error
		for _, ip := range ips {
			if err := rejectInternalIP(ip); err != nil {
				lastRejectErr = err
				continue
			}
			// Use the validated IP directly — no second hostname resolution.
			return base(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		if lastRejectErr != nil {
			return nil, lastRejectErr
		}
		return nil, fmt.Errorf("weixin dial: no valid IP for %q", host)
	}
}

// rejectInternalIP returns a non-nil error iff ip falls in the SSRF deny-set:
// loopback, RFC1918/ULA private, link-local (incl. 169.254.0.0/16 IMDS), or
// the unspecified address. Mirrors the config-time literal-IP guard.
func rejectInternalIP(ip net.IP) error {
	if ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() {
		return fmt.Errorf("weixin dial: refusing connection to internal address %s (SSRF guard)", ip)
	}
	return nil
}

// isLoopbackBaseURL reports whether baseURL targets a loopback host
// (localhost / 127.0.0.0/8 / ::1). These are the dev-mock URLs that
// validateBaseURLScheme greenlights, so they must remain dialable with the
// SSRF guard disabled. Any parse failure is treated as non-loopback so the
// guard fails closed (a malformed URL gets the stricter treatment).
func isLoopbackBaseURL(baseURL string) bool {
	if baseURL == "" {
		return false // empty → defaultBaseURL (public iLink host)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func newAPIClient(baseURL, token string) *apiClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	// Explicit Transport with idle-conn bounds: the default http.Transport
	// only keeps 2 idle conns per host, which is fine for bursty traffic but
	// without tuning we also inherit unlimited MaxIdleConns globally and a
	// 90s IdleConnTimeout. Long-poll reconnects happen every ~35s so without
	// keep-alive the client would open a fresh TCP+TLS handshake on every
	// poll. Pinning a small per-host pool keeps reuse predictable.
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Refuse TLS 1.0/1.1 negotiation even if compiled against an older Go
		// toolchain; matches feishu/slack/discord clients.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	// R20260603040203-SEC-10: install a DNS-aware SSRF dial guard for every
	// non-loopback relay. Loopback dev mocks (validateBaseURLScheme-approved
	// http://127.0.0.1 / localhost) are exempt so local development and the
	// httptest-based test suite still dial the loopback server.
	if !isLoopbackBaseURL(baseURL) {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		transport.DialContext = ssrfDialGuard(dialer.DialContext)
	}
	return &apiClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   defaultLongPollTimeout + 10*time.Second, // covers long-poll (35s) + margin
			// Block redirects: a compromised or MITM'd relay could 3xx us
			// to an IMDS address (169.254.169.254) or an internal admin
			// port, with the Bearer token riding along. Feishu's client
			// does the same. ErrUseLastResponse returns the 3xx body
			// unchanged so callers see the upstream decision explicitly.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// --- request helpers ---

func randomWechatUIN() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := binary.BigEndian.Uint32(b)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", n)))
}

func generateClientID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("naozhi-%x", b)
}

func (c *apiClient) post(ctx context.Context, endpoint string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	// Use shorter timeout for non-polling endpoints
	if !strings.Contains(endpoint, "getupdates") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultAPITimeout)
		defer cancel()
	}

	u := c.baseURL + "/" + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomWechatUIN())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// --- getUpdates ---

type getUpdatesReq struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      baseInfo `json:"base_info"`
}

type weixinMessage struct {
	Seq          int           `json:"seq,omitempty"`
	MessageID    int           `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTimeMs int64         `json:"create_time_ms,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ItemList     []messageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

type messageItem struct {
	Type     int       `json:"type,omitempty"`
	TextItem *textItem `json:"text_item,omitempty"`
}

type textItem struct {
	Text string `json:"text,omitempty"`
}

const (
	msgItemTypeText  = 1
	msgItemTypeImage = 2
	msgTypeUser      = 1
	msgTypeBOT       = 2
	msgStateFinish   = 2
)

type getUpdatesResp struct {
	Ret               int             `json:"ret"`
	ErrCode           int             `json:"errcode,omitempty"`
	ErrMsg            string          `json:"errmsg,omitempty"`
	Msgs              []weixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf     string          `json:"get_updates_buf,omitempty"`
	LongPollTimeoutMs int             `json:"longpolling_timeout_ms,omitempty"`
}

func (c *apiClient) getUpdates(ctx context.Context, cursor string) (*getUpdatesResp, error) {
	data, err := c.post(ctx, "ilink/bot/getupdates", getUpdatesReq{
		GetUpdatesBuf: cursor,
		BaseInfo:      baseInfo{ChannelVersion: channelVersion},
	})
	if err != nil {
		return nil, err
	}
	var resp getUpdatesResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal getUpdates: %w", err)
	}
	return &resp, nil
}

// --- sendMessage ---

type sendMessageReq struct {
	Msg      weixinMessage `json:"msg"`
	BaseInfo baseInfo      `json:"base_info"`
}

func (c *apiClient) sendMessage(ctx context.Context, to, text, contextToken string) error {
	req := sendMessageReq{
		Msg: weixinMessage{
			FromUserID:   "", // must be empty per OpenClaw Weixin plugin
			ToUserID:     to,
			ClientID:     generateClientID(),
			MessageType:  msgTypeBOT,
			MessageState: msgStateFinish,
			ContextToken: contextToken,
			ItemList: []messageItem{
				{Type: msgItemTypeText, TextItem: &textItem{Text: text}},
			},
		},
		BaseInfo: baseInfo{ChannelVersion: channelVersion},
	}
	data, err := c.post(ctx, "ilink/bot/sendmessage", req)
	if err != nil {
		return err
	}
	var resp struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("unmarshal sendMessage response: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("sendMessage failed: ret=%d errcode=%d errmsg=%q", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}
	slog.Debug("weixin sendMessage ok")
	return nil
}
