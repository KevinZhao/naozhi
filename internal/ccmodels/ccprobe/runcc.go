package ccprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/ccmodels"
)

// ccTurnTimeout bounds one probe turn. A healthy turn takes ~10s; the slack is
// for throttling retries inside cc, which should end as StatusUnknown rather
// than hanging a sync.
const ccTurnTimeout = 90 * time.Second

// probePrompt is short enough to keep the turn cheap and specific enough that a
// model answering it proves the round trip worked.
const probePrompt = "say ok"

// ProbeAlias runs one real cc turn on alias and reports what happened.
//
// base is the operator's settings document, reused so the probe inherits their
// credential export and Bedrock env; only the model keys and fallbackModel are
// replaced. Dropping fallbackModel is what makes this test meaningful: with a
// fallback configured, an alias cc cannot resolve answers from a substitute
// model and looks healthy.
func ProbeAlias(ctx context.Context, cliPath string, base []byte, a ccmodels.Alias) ccmodels.Verdict {
	v := ccmodels.Verdict{Alias: a.Name}

	path, cleanup, err := writeProbeSettings(base, a)
	if err != nil {
		v.Status, v.Detail = ccmodels.StatusUnknown, err.Error()
		return v
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, ccTurnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "--bare", "-p", probePrompt,
		"--model", a.Name,
		"--settings", path, "--setting-sources", "",
		"--output-format", "stream-json", "--verbose")
	out, runErr := cmd.Output()
	res, ok := lastResult(out)
	if !ok {
		if ctx.Err() != nil {
			v.Status, v.Detail = ccmodels.StatusUnknown, "probe turn timed out"
			return v
		}
		v.Status = ccmodels.StatusUnknown
		v.Detail = "cc produced no result event"
		if runErr != nil {
			v.Detail += ": " + runErr.Error()
		}
		return v
	}
	v.Window = res.window()
	v.Status, v.Detail = classifyCCResult(res)
	return v
}

// ccResult is the subset of cc's stream-json result event a probe reads.
type ccResult struct {
	IsError    bool   `json:"is_error"`
	Result     string `json:"result"`
	ModelUsage map[string]struct {
		ContextWindow int `json:"contextWindow"`
	} `json:"modelUsage"`
}

// window returns the largest context window any model on the turn reported.
func (r ccResult) window() int {
	best := 0
	for _, u := range r.ModelUsage {
		if u.ContextWindow > best {
			best = u.ContextWindow
		}
	}
	return best
}

// classifyCCResult maps a cc turn to a verdict. An invalid-identifier error is
// the alias-resolution failure this stage exists to catch; a denial from cc
// carries the same weight as one from Converse; anything else stays undecided.
func classifyCCResult(r ccResult) (ccmodels.Status, string) {
	if !r.IsError {
		return ccmodels.StatusOK, ""
	}
	msg := strings.TrimSpace(r.Result)
	switch {
	case strings.Contains(msg, "model identifier is invalid"),
		strings.Contains(msg, "Invalid model name"):
		return ccmodels.StatusBadAlias, msg
	case strings.Contains(msg, "AccessDenied"), strings.Contains(msg, "not authorized"):
		return ccmodels.StatusDenied, msg
	default:
		return ccmodels.StatusUnknown, msg
	}
}

// lastResult scans NDJSON for the final result event, tolerating the human-
// readable warnings cc interleaves on stdout.
func lastResult(out []byte) (ccResult, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var res ccResult
	var found bool
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, `{`) {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &head) != nil || head.Type != "result" {
			continue
		}
		var r ccResult
		if json.Unmarshal([]byte(line), &r) == nil {
			res, found = r, true
		}
	}
	return res, found
}

// writeProbeSettings materialises a 0600 settings file offering only a, in a
// 0700 temp dir removed by cleanup.
func writeProbeSettings(base []byte, a ccmodels.Alias) (path string, cleanup func(), err error) {
	doc, err := ccmodels.ProbeSettings(base, a)
	if err != nil {
		return "", func() {}, err
	}
	dir, err := os.MkdirTemp("", "naozhi-ccprobe-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create probe settings dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write probe settings: %w", err)
	}
	return path, cleanup, nil
}
