package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

func newAPIClient(baseURL, token string) *apiClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &apiClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: defaultLongPollTimeout + 10*time.Second, // covers long-poll (35s) + margin
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
		return fmt.Errorf("sendMessage failed: ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}
	slog.Debug("weixin sendMessage ok")
	return nil
}
