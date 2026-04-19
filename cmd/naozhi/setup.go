package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	setupBaseURL      = "https://ilinkai.weixin.qq.com"
	setupPollInterval = 2 * time.Second
	setupPollTimeout  = 5 * time.Minute
)

var defaultConfigTemplate = `server:
  addr: ":8180"

cli:
  path: "claude"
  model: "sonnet"

session:
  max_procs: 3
  ttl: "30m"
  prune_ttl: "72h"
  store_path: "~/.naozhi/sessions.json"

platforms:
  weixin:
    token: "%s"

log:
  level: "info"
`

func runSetup(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: naozhi setup <platform>")
		fmt.Println()
		fmt.Println("Platforms:")
		fmt.Println("  weixin    WeChat (iLink Bot)")
		os.Exit(1)
	}

	switch args[0] {
	case "weixin":
		runSetupWeixin(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown platform: %s\n", args[0])
		os.Exit(1)
	}
}

func runSetupWeixin(args []string) {
	fs := flag.NewFlagSet("setup weixin", flag.ExitOnError)
	configPath := fs.String("config", "", "config file path (default ~/.naozhi/config.yaml)")
	fs.Parse(args)

	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".naozhi", "config.yaml")
	}

	// Check claude CLI
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Println("Warning: claude CLI not found in PATH")
		fmt.Println("Install: https://claude.ai/code")
		fmt.Println()
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupPollTimeout)
	defer cancel()

	// Step 1: Get QR code
	fmt.Println("Fetching WeChat login QR code...")
	qr, err := setupGetQRCode(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get QR code: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("Scan with WeChat (use the bot account):")
	fmt.Printf("\n  %s\n\n", qr.QRCodeImgContent)
	fmt.Println("Waiting for confirmation...")

	// Step 2: Poll for status
	token, err := setupPollStatus(ctx, qr.QRCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nLogin failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nLogin successful!")

	// Step 3: Write config
	if err := setupWriteConfig(*configPath, token); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write config: %v\n", err)
		fmt.Println()
		fmt.Println("Add manually to config.yaml:")
		fmt.Println("  platforms:")
		fmt.Println("    weixin:")
		fmt.Printf("      token: \"%s\"\n", token)
		os.Exit(1)
	}

	fmt.Printf("\nConfig saved: %s\n", *configPath)
	fmt.Println()
	fmt.Printf("Start: naozhi --config %s\n", *configPath)
}

// --- QR Code API ---

type setupQRResp struct {
	Ret              int    `json:"ret"`
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type setupStatusResp struct {
	Status     string `json:"status"` // "wait" | "scaned" | "confirmed" | "expired"
	BotToken   string `json:"bot_token,omitempty"`
	ILinkBotID string `json:"ilink_bot_id,omitempty"`
}

func setupGetQRCode(ctx context.Context) (*setupQRResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		setupBaseURL+"/ilink/bot/get_bot_qrcode?bot_type=3", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("QR code API returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	var qr setupQRResp
	if err := json.Unmarshal(data, &qr); err != nil {
		return nil, err
	}
	if qr.Ret != 0 {
		return nil, fmt.Errorf("API error: ret=%d, body=%s", qr.Ret, string(data))
	}
	if qr.QRCode == "" || qr.QRCodeImgContent == "" {
		return nil, fmt.Errorf("empty QR code in response")
	}
	return &qr, nil
}

func setupPollStatus(ctx context.Context, qrcode string) (string, error) {
	ticker := time.NewTicker(setupPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for scan")
		case <-ticker.C:
			status, err := setupCheckStatus(ctx, qrcode)
			if err != nil {
				return "", err
			}
			switch status.Status {
			case "confirmed":
				if status.BotToken == "" {
					return "", fmt.Errorf("confirmed but no token returned")
				}
				return status.BotToken, nil
			case "expired":
				return "", fmt.Errorf("QR code expired, please retry")
			case "scaned":
				fmt.Print(".")
			}
		}
	}
}

func setupCheckStatus(ctx context.Context, qrcode string) (*setupStatusResp, error) {
	u := setupBaseURL + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-ClientVersion", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status check API returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	var s setupStatusResp
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// --- Config writing ---

func setupWriteConfig(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// New file: generate from template
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return writeFileAtomic(path, []byte(fmt.Sprintf(defaultConfigTemplate, token)), 0600)
	}

	// Existing file: update weixin token via yaml.Node (preserves comments)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	updated, err := updateWeixinToken(data, token)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, updated, 0600)
}

// writeFileAtomic writes data to path via a temp file + rename so an
// interrupted write (disk full, process killed) cannot leave a truncated
// config on disk. The temp file lives in the same directory so rename is
// guaranteed to be a same-filesystem atomic operation.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".setup-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op on successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func updateWeixinToken(data []byte, token string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}

	root := doc.Content[0]
	platforms := yamlFindOrCreateMap(root, "platforms")
	weixin := yamlFindOrCreateMap(platforms, "weixin")
	yamlSetScalar(weixin, "token", token)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlFindOrCreateMap finds a mapping child by key, or creates one.
func yamlFindOrCreateMap(parent *yaml.Node, key string) *yaml.Node {
	if parent.Kind != yaml.MappingNode {
		return parent
	}
	for i := 0; i < len(parent.Content)-1; i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1]
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}

// yamlSetScalar sets or creates a scalar value in a mapping node.
func yamlSetScalar(parent *yaml.Node, key, value string) {
	if parent.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(parent.Content)-1; i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1].Value = value
			parent.Content[i+1].Tag = ""
			parent.Content[i+1].Style = yaml.DoubleQuotedStyle
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: value, Style: yaml.DoubleQuotedStyle}
	parent.Content = append(parent.Content, keyNode, valNode)
}
