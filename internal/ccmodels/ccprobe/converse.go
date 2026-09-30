package ccprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/naozhi/naozhi/internal/ccmodels"
)

// converseBody asks for a single output token: the probe only needs the call to
// be authorised, not to produce anything.
var converseBody = []byte(`{"messages":[{"role":"user","content":[{"text":"ok"}]}],` +
	`"inferenceConfig":{"maxTokens":1}}`)

const (
	converseTimeout = 30 * time.Second
	signingService  = "bedrock"
	// maxErrBody bounds how much of a provider error is read into a Detail.
	maxErrBody = 4 << 10
)

// ProbeProfile reports whether this caller may invoke one inference profile.
//
// The "[1m]" suffix is stripped first: it is cc's marker for a context-window
// choice, not part of any profile id, and Bedrock rejects it outright.
func ProbeProfile(ctx context.Context, hc *http.Client, creds aws.Credentials, region, profile string) ccmodels.Verdict {
	id := strings.TrimSuffix(profile, "[1m]")
	v := ccmodels.Verdict{Alias: profile}

	ctx, cancel := context.WithTimeout(ctx, converseTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse",
		region, url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(converseBody))
	if err != nil {
		v.Status, v.Detail = ccmodels.StatusUnknown, err.Error()
		return v
	}
	req.Header.Set("Content-Type", "application/json")
	sum := sha256.Sum256(converseBody)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]),
		signingService, region, time.Now().UTC()); err != nil {
		v.Status, v.Detail = ccmodels.StatusUnknown, fmt.Sprintf("sign request: %v", err)
		return v
	}
	resp, err := hc.Do(req)
	if err != nil {
		v.Status, v.Detail = ccmodels.StatusUnknown, err.Error()
		return v
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	v.Status, v.Detail = classifyConverse(resp.StatusCode, resp.Header.Get("X-Amzn-Errortype"), body)
	return v
}

// classifyConverse maps a Converse response to a verdict.
//
// Throttling means authorised-but-busy, so it keeps the model. Any other
// unrecognised failure is undecided rather than fatal.
func classifyConverse(code int, errType string, body []byte) (ccmodels.Status, string) {
	if code == http.StatusOK {
		return ccmodels.StatusOK, ""
	}
	kind := awsErrorType(errType, body)
	detail := kind
	if msg := awsErrorMessage(body); msg != "" {
		detail = kind + ": " + msg
	}
	switch {
	case code == http.StatusTooManyRequests || strings.Contains(kind, "Throttling"):
		return ccmodels.StatusUnknown, "throttled, treated as authorised"
	case strings.Contains(kind, "AccessDenied"),
		strings.Contains(kind, "ResourceNotFound"),
		strings.Contains(kind, "Validation"):
		return ccmodels.StatusDenied, detail
	case code >= 500:
		return ccmodels.StatusUnknown, detail
	default:
		return ccmodels.StatusUnknown, fmt.Sprintf("HTTP %d %s", code, detail)
	}
}

// awsErrorType prefers the X-Amzn-Errortype header, falling back to the body's
// __type. The header carries a "Type:endpoint" form, so only the head is kept.
func awsErrorType(header string, body []byte) string {
	if header != "" {
		return strings.SplitN(header, ":", 2)[0]
	}
	var env struct {
		Type string `json:"__type"`
	}
	if json.Unmarshal(body, &env) == nil && env.Type != "" {
		return strings.TrimPrefix(env.Type, "com.amazon.coral.service#")
	}
	return "unknown error"
}

func awsErrorMessage(body []byte) string {
	var env struct {
		Message string `json:"message"`
		Alt     string `json:"Message"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if env.Message != "" {
		return env.Message
	}
	return env.Alt
}
