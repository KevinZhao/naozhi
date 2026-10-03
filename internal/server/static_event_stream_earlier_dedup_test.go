package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventStreamJS_DedupEarlierPage_SameMsCursor runs the real
// dedupEarlierPage under node against a table of load-earlier pages (#3030).
// The request sends before=cursor+1, so each page re-admits the cursor ms:
// the entries held there must drop out, a sibling the previous page did not
// carry must stay, and the held keys follow the cursor. The browser half (the
// request URLs, prepend, trim) is test/e2e/load_earlier_same_ms.test.js.
func TestEventStreamJS_DedupEarlierPage_SameMsCursor(t *testing.T) {
	t.Parallel()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := eventStreamJS.ReadFile("static/event_stream.js")
	if err != nil {
		t.Fatalf("read event_stream.js: %v", err)
	}
	block := extractContractBlock(t, string(src), "dedupEarlierPage")

	script := block + `
function eq(a, b, msg) {
  const ja = JSON.stringify(a), jb = JSON.stringify(b);
  if (ja !== jb) { console.error('FAIL ' + msg + ': got ' + ja + ' want ' + jb); process.exit(1); }
}
const ev = (id, time, extra) => Object.assign({ uuid: id, time, type: 'text', detail: id }, extra);
const ids = page => page.events.map(e => e.uuid || e.detail);
const a = ev('a', 2000), b = ev('b', 2000), c = ev('c', 2000), old = ev('old', 1000);

// A full page seeds the cursor at its oldest ms and holds every entry there,
// whatever the order the page lists them in.
let p = dedupEarlierPage([b, c, ev('n', 3000)], 0, []);
eq(ids(p), ['b', 'c', 'n'], 'seed keeps the whole page');
eq([p.oldestMS, p.seenKeys], [2000, ['u:b', 'u:c']], 'seed cursor');
eq(dedupEarlierPage([ev('n', 3000), b], 0, []).oldestMS, 2000, 'seed takes the minimum time, not events[0]');

// The next page re-admits 2000: b and c are replays, a is the sibling the
// previous page cut off.
p = dedupEarlierPage([old, a, b, c], 2000, ['u:b', 'u:c']);
eq(ids(p), ['old', 'a'], 'replays dropped, same-ms sibling kept');
eq([p.oldestMS, p.seenKeys], [1000, ['u:old']], 'cursor moves, keys reset to the new ms');

// A page that stays at the cursor ms carries the old keys forward.
p = dedupEarlierPage([a, b, c], 2000, ['u:b', 'u:c']);
eq(ids(p), ['a'], 'still at the cursor ms');
eq([p.oldestMS, p.seenKeys], [2000, ['u:b', 'u:c', 'u:a']], 'keys merge when the cursor does not move');
eq(dedupEarlierPage([a, b, c], 2000, ['u:a', 'u:b', 'u:c']).events, [], 'a page of replays is empty');

// Entries newer than the cursor are already held (a server ignoring before=).
p = dedupEarlierPage([ev('n', 3000), a], 2000, ['u:a']);
eq([ids(p), p.oldestMS, p.seenKeys], [[], 2000, ['u:a']], 'newer than the cursor dropped, cursor kept');

// No uuid: the key is time|type|detail, so the same content at the cursor ms
// is a replay and different content is a sibling.
const k1 = { time: 2000, type: 'text', detail: 'one' }, k2 = { time: 2000, type: 'text', detail: 'two' };
p = dedupEarlierPage([k1], 0, []);
eq(p.seenKeys, ['k:2000|text|one'], 'uuid-less key');
p = dedupEarlierPage([old, k1, k2], p.oldestMS, p.seenKeys);
eq(ids(p), ['old', 'two'], 'uuid-less replay dropped, distinct uuid-less sibling kept');
// The same content at another ms is another entry.
eq(ids(dedupEarlierPage([{ time: 1500, type: 'text', detail: 'one' }], 2000, ['k:2000|text|one'])), ['one'], 'key includes the ms');
console.log('OK');
`
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup_earlier_test.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(nodeBin, path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("node contract failed: %v\n%s", err, out)
	}
}
