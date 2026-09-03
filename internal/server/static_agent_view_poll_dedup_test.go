package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractContractBlock returns the JS between `// @contract-begin <name>` and
// `// @contract-end <name>` markers so a pure helper can be executed under
// node without loading the whole browser module.
func extractContractBlock(t *testing.T, src, name string) string {
	t.Helper()
	begin := "// @contract-begin " + name
	end := "// @contract-end " + name
	i := strings.Index(src, begin)
	j := strings.Index(src, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("agent_view.js: missing %q / %q markers", begin, end)
	}
	return src[i+len(begin) : j]
}

// TestAgentViewJS_DedupAgentPollBatch_SameMsReplay (#2432 item 5): the
// agent_events HTTP poll fallback now receives `Time >= after`, so the page
// after a poll boundary replays the entries already rendered at the
// watermark ms. dedupAgentPollBatch must drop exactly those replays (by
// content key — transcript entries carry no uuid) while keeping the
// legitimate same-ms sibling and advancing the watermark correctly.
func TestAgentViewJS_DedupAgentPollBatch_SameMsReplay(t *testing.T) {
	t.Parallel()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	av, err := agentViewJS.ReadFile("static/agent_view.js")
	if err != nil {
		t.Fatalf("read agent_view.js: %v", err)
	}
	block := extractContractBlock(t, string(av), "dedupAgentPollBatch")

	script := block + `
function eq(a, b, msg) {
  const ja = JSON.stringify(a), jb = JSON.stringify(b);
  if (ja !== jb) { console.error('FAIL ' + msg + ': got ' + ja + ' want ' + jb); process.exit(1); }
}
const thinking = { time: 2000, type: 'thinking', summary: 'hmm', detail: 'hmm' };
const text = { time: 2000, type: 'text', summary: 'answer', detail: 'answer' };
const later = { time: 3000, type: 'tool_use', tool: 'Read', summary: 'Read', detail: 'x' };

// Page 1 (after=0): only the thinking entry made it under the page limit.
let b = dedupAgentPollBatch([thinking], 0, []);
eq(b.events.map(e => e.type), ['thinking'], 'page1 renders thinking');
eq(b.afterMS, 2000, 'page1 watermark');

// Page 2 (after=2000, server re-admits the ms): thinking is a replay, text is
// the same-ms sibling that the strict cursor used to lose.
b = dedupAgentPollBatch([thinking, text], b.afterMS, b.seenKeys);
eq(b.events.map(e => e.type), ['text'], 'page2 drops replay, keeps same-ms sibling');
eq(b.afterMS, 2000, 'page2 watermark unchanged');

// Page 3: both same-ms entries replayed again, nothing new -> render nothing.
b = dedupAgentPollBatch([thinking, text], b.afterMS, b.seenKeys);
eq(b.events.length, 0, 'page3 idle poll renders nothing');

// Page 4: watermark moves; keys reset to the new ms only.
b = dedupAgentPollBatch([thinking, text, later], b.afterMS, b.seenKeys);
eq(b.events.map(e => e.type), ['tool_use'], 'page4 renders only the newer entry');
eq(b.afterMS, 3000, 'page4 watermark advances');
eq(b.seenKeys.length, 1, 'page4 keys hold only the new watermark ms');

// Two distinct same-ms text blocks with different content are both kept.
b = dedupAgentPollBatch([{ time: 5000, type: 'text', summary: 'a' }, { time: 5000, type: 'text', summary: 'b' }], 3000, []);
eq(b.events.length, 2, 'distinct same-ms siblings both render');

// time===0 never advances the watermark (predates the field).
b = dedupAgentPollBatch([{ type: 'text', summary: 'legacy' }], 3000, []);
eq(b.afterMS, 3000, 'time 0 does not move watermark');
eq(b.events.length, 1, 'time 0 entry still renders');
console.log('OK');
`
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup_test.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(nodeBin, path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("node contract failed: %v\n%s", err, out)
	}
}
