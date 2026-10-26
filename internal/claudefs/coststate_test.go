package claudefs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sid.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func costLine(sid string, usd float64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "cost-state", "sessionId": sid, "totalCostUSD": usd,
		"modelUsage": map[string]any{"claude-opus-5-5[1m]": map[string]any{"outputTokens": 7, "costUSD": usd}},
	})
	return string(b)
}

// The CLI restores the LAST cost-state, so that is the one returned: earlier
// ones, other sessions' ones and look-alike lines must not win.
func TestLastCostState_ReturnsTheLastOwnLine(t *testing.T) {
	big := `{"type":"user","message":{"content":"` + strings.Repeat("x", 3<<20) + ` \"cost-state\""}}`
	p := writeTranscript(t,
		costLine("sid", 1.5),
		`{"type":"assistant","message":{"content":"mentions \"cost-state\" in passing"}}`,
		big, // over the decode bound: skipped unread
		costLine("sid", 929.98),
		costLine("other-session", 5000),
		`{"type":"user","message":{"content":"cost-state"}}`,
		`{"type":"cost-state","totalCostUSD":"not a number"}`,
	)
	st, found, err := LastCostState(p, "sid")
	if err != nil || !found {
		t.Fatalf("LastCostState = found %v, err %v; want the restored line", found, err)
	}
	if st.TotalCostUSD != 929.98 {
		t.Errorf("TotalCostUSD = %v, want 929.98 (the last line of this session)", st.TotalCostUSD)
	}
	var models map[string]struct {
		CostUSD float64 `json:"costUSD"`
	}
	if err := json.Unmarshal(st.ModelUsage, &models); err != nil || models["claude-opus-5-5[1m]"].CostUSD != 929.98 {
		t.Errorf("ModelUsage = %s (%v), want the line's per-model rows", st.ModelUsage, err)
	}
}

// A line without a sessionId is taken as the transcript's own, and the last
// line is read even without a trailing newline.
func TestLastCostState_UnownedLineAndNoTrailingNewline(t *testing.T) {
	p := writeTranscript(t, `{"type":"user"}`, `{"type":"cost-state","totalCostUSD":2.25}`)
	st, found, err := LastCostState(p, "sid")
	if err != nil || !found || st.TotalCostUSD != 2.25 {
		t.Fatalf("LastCostState = %+v, found %v, err %v; want 2.25", st, found, err)
	}
}

// No cost-state means the CLI restores nothing: found=false, no error.
func TestLastCostState_NoneFound(t *testing.T) {
	p := writeTranscript(t, `{"type":"user"}`, `{"type":"assistant"}`)
	if _, found, err := LastCostState(p, "sid"); err != nil || found {
		t.Fatalf("found %v, err %v; want not found and no error", found, err)
	}
}

// A missing transcript is an error: the caller is about to resume it, and
// "no cost-state" would wrongly mean "the CLI restores nothing".
func TestLastCostState_MissingFileIsAnError(t *testing.T) {
	_, found, err := LastCostState(filepath.Join(t.TempDir(), "gone.jsonl"), "sid")
	if !errors.Is(err, fs.ErrNotExist) || found {
		t.Fatalf("found %v, err %v; want ErrNotExist", found, err)
	}
}
