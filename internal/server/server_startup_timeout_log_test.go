package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestServerStartupLogsTimeouts pins R244-ARCH-16 (#1054): the "server
// starting" log line must surface the Server's effective turn timeouts so an
// operator can confirm the active values from journalctl without reading
// config or hitting /health. Source-level (mirrors the CTX1 derivation tests)
// because Start binds a real listener; we only need to lock the log shape.
func TestServerStartupLogsTimeouts(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "server.go")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(raw)

	// The "server starting" slog.Info call must carry both timeout fields.
	startLog := regexp.MustCompile(`(?s)slog\.Info\(\s*"server starting".*?\)`).FindString(body)
	if startLog == "" {
		t.Fatal("server.go: could not locate the \"server starting\" slog.Info call")
	}
	for _, key := range []string{`"no_output_timeout"`, `s.noOutputTimeout`, `"total_timeout"`, `s.totalTimeout`} {
		if !regexp.MustCompile(regexp.QuoteMeta(key)).MatchString(startLog) {
			t.Errorf("R244-ARCH-16 regression: \"server starting\" log must include %s for operator timeout visibility; got:\n%s", key, startLog)
		}
	}
}
