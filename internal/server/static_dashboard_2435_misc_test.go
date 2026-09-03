package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests for #2435 items 2, 5 and 6 (dashboard.js):
//
//  2. running-banner line 1 duplicated the tool name for MCP tools
//     ("使用 mcp__x mcp__x: {...}") because the detail's first line already
//     starts with the tool name;
//  5. the 30s voice cap left the gesture handlers driving a recorder that
//     was already finalized (swipe "cancel" still sent, lift hid the
//     transcribing overlay, timer kept toasting, auto-send replaced a draft);
//  6. #rb-elapsed showed the previous turn's final time when a new banner
//     opened and was anchored at the first event rather than the send.
//
// Behavioural cases run the extracted pure functions under node (skipped
// without node); the static contracts always run.

// TestDashboardJS_ToolSummaryLine_StripsToolNamePrefix pins item 2.
func TestDashboardJS_ToolSummaryLine_StripsToolNamePrefix(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if !strings.Contains(js, "summary: toolSummaryLine(ev.tool || ev.summary, ev.detail)") {
		t.Fatal("applyEventToTurnState tool_use branch must derive currentTool.summary via toolSummaryLine (#2435)")
	}
	script := "const toolVerbs = { Bash: '执行', Read: '读取' };\n" +
		extractJSFunction(t, js, "toolVerb") +
		extractJSFunction(t, js, "toolSummaryLine") +
		`const cases = [
  ['mcp__x__y', 'mcp__x__y: {"a":1}\nsecond line'],
  ['mcp__x__y', 'mcp__x__y'],
  ['Bash', 'Bash ls -la'],
  ['Read', '/tmp/a.txt'],
  ['Bash', ''],
  ['Bash', 'Bash: ' + 'x'.repeat(100)],
];
process.stdout.write(JSON.stringify(cases.map(([tool, detail]) => toolVerb(tool, toolSummaryLine(tool, detail)))));
`
	var got []string
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	want := []string{
		`使用 mcp__x__y {"a":1}`,
		`使用 mcp__x__y...`,
		`执行 ls -la`,
		`读取 /tmp/a.txt`,
		`执行...`,
		`执行 ` + strings.Repeat("x", 60),
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("case %d: got %q, want %q", i, got, want[i])
		}
	}
}

// TestDashboardJS_TurnElapsed_ResetsAndPaintsImmediately pins item 6:
// startTurnTimer paints 0:00 synchronously and resetTurnState blanks the
// chip, so a fresh banner never opens on the previous turn's final time.
func TestDashboardJS_TurnElapsed_ResetsAndPaintsImmediately(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	script := `const el = { textContent: '4:59' };
const document = { getElementById: (id) => id === 'rb-elapsed' ? el : null };
let intervals = 0;
const setInterval = () => ++intervals;
const clearInterval = () => {};
function refreshBanner() {}
let turnState = { turnStartTime: 0, timerId: null };
` + extractJSFunction(t, js, "paintTurnElapsed") +
		extractJSFunction(t, js, "startTurnTimer") +
		extractJSFunction(t, js, "resetTurnState") + `
const out = {};
startTurnTimer();
out.afterStart = el.textContent;
out.anchoredNow = Math.abs(Date.now() - turnState.turnStartTime) < 1000;
turnState.turnStartTime = Date.now() - 125000;
paintTurnElapsed();
out.painted = el.textContent;
resetTurnState();
out.afterReset = el.textContent;
out.startTimeCleared = turnState.turnStartTime === 0;
process.stdout.write(JSON.stringify(out));
`
	var got struct {
		AfterStart, Painted, AfterReset string
		AnchoredNow, StartTimeCleared   bool
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if got.AfterStart != "0:00" {
		t.Errorf("startTurnTimer must paint 0:00 immediately, got %q", got.AfterStart)
	}
	if !got.AnchoredNow {
		t.Error("startTurnTimer must anchor turnStartTime at Date.now()")
	}
	if got.Painted != "2:05" {
		t.Errorf("paintTurnElapsed = %q, want 2:05", got.Painted)
	}
	if got.AfterReset != "" {
		t.Errorf("resetTurnState must blank #rb-elapsed, got %q", got.AfterReset)
	}
	if !got.StartTimeCleared {
		t.Error("resetTurnState must clear turnStartTime")
	}
}

// TestDashboardJS_TurnTimer_AnchoredAtSend pins the other half of item 6:
// the optimistic running flip at send time starts the timer before the
// banner is shown, so the elapsed chip counts CLI spawn latency too.
func TestDashboardJS_TurnTimer_AnchoredAtSend(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := extractJSFunction(t, js, "markSessionOptimisticRunning")
	start := strings.Index(body, "startTurnTimer();")
	show := strings.Index(body, "updateSendButton('running');")
	if start < 0 || show < 0 || start > show {
		t.Error("markSessionOptimisticRunning must call startTurnTimer() before updateSendButton('running') (#2435)")
	}
}

// TestDashboardJS_VoiceCap_StateMachine pins item 5's static shape; the
// gesture sequence itself is exercised in test/e2e/voice_cap_gesture.test.js.
func TestDashboardJS_VoiceCap_StateMachine(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if !strings.Contains(js, "\nlet voiceState = 'idle';") {
		t.Fatal("dashboard.js must declare the voiceState lifecycle (idle/recording/finalizing) (#2435)")
	}
	stop := extractJSFunction(t, js, "stopVoiceRecording")
	if !strings.Contains(stop, "if (voiceState === 'finalizing') return;") {
		t.Error("stopVoiceRecording must ignore calls once the recording is finalizing")
	}
	if !strings.Contains(stop, "clearInterval(voiceRecTimer)") {
		t.Error("stopVoiceRecording must clear voiceRecTimer so the cap toast fires once")
	}
	for _, fn := range []string{"voiceTouchEnd", "voiceTouchCancel"} {
		if !strings.Contains(extractJSFunction(t, js, fn), "finishVoiceGesture(") {
			t.Errorf("%s must route through finishVoiceGesture", fn)
		}
	}
	if !strings.Contains(extractJSFunction(t, js, "hideVoiceOverlay"), "voiceState = 'idle';") {
		t.Error("hideVoiceOverlay must return the lifecycle to idle")
	}
	move := extractJSFunction(t, js, "voiceTouchMove")
	if !strings.Contains(move, "if (voiceState !== 'recording') return;") {
		t.Error("voiceTouchMove must ignore the cancel gesture once finalizing")
	}
	tr := extractJSFunction(t, js, "transcribeAudio")
	if strings.Contains(tr, "autoSend ? data.text") {
		t.Error("transcribeAudio must append the transcript to the draft instead of replacing it on auto-send")
	}
}
