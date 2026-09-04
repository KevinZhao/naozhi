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

	"github.com/naozhi/naozhi/internal/osutil"
)

const (
	defaultBaseURL         = "https://ilinkai.weixin.qq.com"
	defaultLongPollTimeout = 35 * time.Second
	defaultAPITimeout      = 15 * time.Second
	channelVersion         = "naozhi-1.0.0"
)

// baseInfo is attached to every request; without it iLink falls back to
// one-shot mode and silently drops every sendMessage after the first.
type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// apiClient wraps the iLink Bot HTTP API.
type apiClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// ssrfDialGuard re-validates every RESOLVED address against the SSRF deny-set
// before the TCP connect. The config-time guard only rejects literal private
// IPs; a hostname under attacker DNS control could still resolve to IMDS or
// an internal admin port, and DialContext is the only place that sees the IP.
// Not installed for approved loopback dev mocks (see newAPIClient).
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
		// A literal IP is validated directly; a hostname is resolved here so
		// every candidate IP can be checked.
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
		// DNS-rebinding defence: dial the validated IP literal, never the
		// hostname, so no second lookup can return an internal address.
		var lastRejectErr error
		for _, ip := range ips {
			if err := rejectInternalIP(ip); err != nil {
				lastRejectErr = err
				continue
			}
			return base(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		if lastRejectErr != nil {
			return nil, lastRejectErr
		}
		return nil, fmt.Errorf("weixin dial: no IPs resolved for %q (SSRF guard)", host)
	}
}

// rejectInternalIP errors iff ip is loopback, private, link-local (incl. IMDS)
// or unspecified; mirrors the config-time literal-IP guard.
func rejectInternalIP(ip net.IP) error {
	if ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() {
		return fmt.Errorf("weixin dial: refusing connection to internal address %s (SSRF guard)", ip)
	}
	return nil
}

// isLoopbackBaseURL reports whether baseURL targets a loopback host (the dev
// mocks validateBaseURLScheme allows). Parse failure = non-loopback, so the
// SSRF guard fails closed.
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
	// Small pinned idle pool: long-poll reconnects every ~35s and keep-alive
	// avoids a fresh TCP+TLS handshake per poll.
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Pin the TLS floor; matches the other adapters.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	// DNS-aware SSRF dial guard for every non-loopback relay; loopback dev
	// mocks (and the httptest suite) must stay dialable.
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
			// No redirects: a MITM'd relay could 3xx the bearer token to IMDS
			// or an internal admin port.
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

	// Shorter timeout for non-polling endpoints.
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
		// Raw upstream body may carry C1/bidi/control bytes; sanitize.
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, osutil.SanitizeForLog(string(data), 256))
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
		return fmt.Errorf("sendMessage failed: ret=%d errcode=%d errmsg=%q", resp.Ret, resp.ErrCode, osutil.SanitizeForLog(resp.ErrMsg, 256))
	}
	slog.Debug("weixin sendMessage ok")
	return nil
}
