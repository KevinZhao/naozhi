package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashboardJS_ScratchAdmitEvent_SameMsReplay (#2456 review P0): the
// scratch drawer polls HandleEvents with ?after=<lastEventTime>, which is now
// inclusive of the watermark millisecond. renderNewEvents used to append
// everything the poll returned, so every idle tick would re-render the tail
// bubble(s) until a newer event arrived. scratchAdmitEvent must drop same-ms
// replays by uuid — including the echoed user event that matchesPendingEcho
// consumed on first sight — while still admitting a genuine same-ms sibling.
func TestDashboardJS_ScratchAdmitEvent_SameMsReplay(t *testing.T) {
	t.Parallel()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	djs, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	src := string(djs)
	block := extractContractBlock(t, src, "scratchAdmitEvent")

	// renderNewEvents must route every polled event through the gate and no
	// longer own the watermark itself.
	rn := src[strings.Index(src, "function renderNewEvents("):]
	rn = rn[:strings.Index(rn, "async function pollOnce(")]
	if !strings.Contains(rn, "if (!scratchAdmitEvent(state, e)) continue;") {
		t.Fatal("renderNewEvents must gate each event through scratchAdmitEvent(state, e)")
	}
	if strings.Contains(rn, "state.lastEventTime = e.time") {
		t.Fatal("renderNewEvents must not advance state.lastEventTime itself (scratchAdmitEvent owns the watermark)")
	}

	script := block + `
function eq(a, b, msg) {
  const ja = JSON.stringify(a), jb = JSON.stringify(b);
  if (ja !== jb) { console.error('FAIL ' + msg + ': got ' + ja + ' want ' + jb); process.exit(1); }
}
const st = { lastEventTime: 0, seenAtWM: new Set() };
const u = { time: 1000, uuid: 'u', type: 'user' };
const a = { time: 2000, uuid: 'a', type: 'thinking' };
const b = { time: 2000, uuid: 'b', type: 'text' };

// Initial page (after absent): everything admitted, watermark = 2000.
eq([u, a].map(e => scratchAdmitEvent(st, e)), [true, true], 'initial page admitted');
eq(st.lastEventTime, 2000, 'watermark after initial page');

// Idle tick replays the watermark ms: a is a replay, must be dropped.
eq(scratchAdmitEvent(st, a), false, 'same-ms replay dropped');
// Same-ms sibling appended after the last poll: admitted exactly once.
eq(scratchAdmitEvent(st, b), true, 'same-ms sibling admitted');
eq(scratchAdmitEvent(st, b), false, 'sibling replay dropped on the next tick');
eq(st.lastEventTime, 2000, 'watermark unchanged by same-ms siblings');

// Strictly older entries never re-render.
eq(scratchAdmitEvent(st, u), false, 'older entry dropped');

// Echo case: the user event is admitted (renderNewEvents then echo-drops it);
// the replay on the next tick must be dropped even though the optimistic
// bubble has no data-uuid in the DOM.
const echo = { time: 3000, uuid: 'e', type: 'user', detail: 'hi' };
eq(scratchAdmitEvent(st, echo), true, 'echoed user event admitted once');
eq(st.lastEventTime, 3000, 'watermark advanced to the echo');
eq(scratchAdmitEvent(st, echo), false, 'echo replay dropped');

// Newer event advances the watermark and resets the dedup set.
eq(scratchAdmitEvent(st, { time: 4000, uuid: 'r', type: 'result' }), true, 'newer admitted');
eq(st.seenAtWM.has('e'), false, 'dedup set reset on advance');
eq(st.seenAtWM.has('r'), true, 'dedup set holds the new watermark uuid');

// uuid-less legacy entries at the watermark are never swallowed.
eq(scratchAdmitEvent(st, { time: 4000, type: 'text' }), true, 'uuid-less same-ms entry admitted');
// time 0 entries pass through and do not move the watermark.
eq(scratchAdmitEvent(st, { type: 'text', uuid: 'z' }), true, 'time 0 admitted');
eq(st.lastEventTime, 4000, 'time 0 does not move watermark');
// A state object created before seenAtWM existed is upgraded lazily.
const legacy = { lastEventTime: 0 };
eq(scratchAdmitEvent(legacy, a), true, 'legacy state admitted');
eq(scratchAdmitEvent(legacy, a), false, 'legacy state dedups');
console.log('OK');
`
	path := filepath.Join(t.TempDir(), "scratch_admit_test.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(nodeBin, path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("node contract failed: %v\n%s", err, out)
	}
}
