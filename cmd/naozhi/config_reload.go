package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/config"
)

// configReload is `naozhi config reload`: it asks the running naozhi to re-read
// its config.yaml (POST /api/system/config/reload) and prints what took
// effect. Exit codes: 0 reloaded and nothing needs a restart, 3 reloaded but
// some sections need a restart, 4 reloaded and a restricted platform is now
// open to every sender (wins over 3), 1 the server refused or is unreachable,
// 2 bad flags.
func configReload(args []string, out io.Writer) int {
	fs := newFlagSet("config reload")
	addr := fs.String("addr", envDefault("NAOZHI_BASE_URL", "http://127.0.0.1:8180"), "naozhi base URL (NAOZHI_BASE_URL)")
	tokenFlag := fs.String("token", "", "dashboard token; defaults to NAOZHI_DASHBOARD_TOKEN env or ~/.naozhi/env")
	timeout := fs.Duration("timeout", 10*time.Second, "request deadline")
	jsonOut := fs.Bool("json", false, "print the server's JSON result verbatim")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cleanAddr := strings.TrimRight(*addr, "/")
	if err := validateDoctorAddr(cleanAddr); err != nil {
		fmt.Fprintf(os.Stderr, "config reload: %v\n", err)
		return 2
	}
	token := *tokenFlag
	if token == "" {
		token = loadTokenBestEffort()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := postConfigReload(ctx, cleanAddr, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config reload: %v\n", err)
		return 1
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printReloadResult(out, res)
	}
	switch {
	case len(res.OpenedPlatforms) > 0:
		return 4
	case len(res.RestartRequired) > 0:
		return 3
	}
	return 0
}

// postConfigReload performs the request; a non-2xx body is returned as the
// error text so an invalid file's validation message reaches the operator.
func postConfigReload(ctx context.Context, addr, token string) (config.ReloadResult, error) {
	var res config.ReloadResult
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/system/config/reload", strings.NewReader("{}"))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return res, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return res, fmt.Errorf("decode response: %w", err)
	}
	return res, nil
}

func printReloadResult(out io.Writer, res config.ReloadResult) {
	fmt.Fprintf(out, "reloaded config %s (loaded %s)\n", shortSHA(res.SHA256), res.LoadedAt.Format(time.RFC3339))
	if len(res.Applied) == 0 {
		fmt.Fprintln(out, "applied: nothing changed in the hot sections")
	} else {
		fmt.Fprintf(out, "applied: %s\n", strings.Join(res.Applied, ", "))
	}
	if len(res.RestartRequired) > 0 {
		fmt.Fprintf(out, "restart required for: %s\n", strings.Join(res.RestartRequired, ", "))
	}
	if len(res.OpenedPlatforms) > 0 {
		fmt.Fprintf(out, "WARNING: now open to every sender (was restricted): %s; a misspelt im_access key reads as absent\n",
			strings.Join(res.OpenedPlatforms, ", "))
	}
}
