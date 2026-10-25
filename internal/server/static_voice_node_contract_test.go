package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Source-level contracts for the voice input (voice.js) and remote-node
// paths of the dashboard. Most JS source-shape tests were removed; these are
// kept because voice transcription and multi-node are features under active
// development. Behavioural cases run extracted functions under node (skipped
// when node is absent).

// extractJSAsyncFunction is extractJSFunction for `async function name(`,
// exported or not.
func extractJSAsyncFunction(t *testing.T, js, name string) string {
	t.Helper()
	i := -1
	for _, prefix := range []string{"\nasync function ", "\nexport async function "} {
		if j := strings.Index(js, prefix+name+"("); j >= 0 {
			i = j + len(prefix) - len("async function ")
			break
		}
	}
	if i < 0 {
		t.Fatalf("dashboard.js: async function %s not found", name)
	}
	rest := js[i:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("dashboard.js: async function %s has no column-0 closing brace", name)
	}
	return rest[:end+2] + "\n"
}

// wsOnHandler returns the source of the wsm.on(...) call for type key: the
// unconditional registration, or with claim set the first one passing a claim
// (a third argument). Bracket depth bounds the call; the scan does not skip
// string contents, which the registrations do not need.
func wsOnHandler(t *testing.T, js, key string, claim bool) string {
	t.Helper()
	head := "wsm.on(NZ_CONTRACT.WS." + key + ","
	for from := 0; ; {
		i := strings.Index(js[from:], head)
		if i < 0 {
			t.Fatalf("no wsm.on registration for %s (claim=%v)", key, claim)
		}
		start := from + i
		depth, commas, end := 0, 0, -1
		for j := start + len("wsm.on"); j < len(js) && end < 0; j++ {
			switch js[j] {
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
				if depth == 0 {
					end = j + 1
				}
			case ',':
				if depth == 1 {
					commas++
				}
			}
		}
		if end < 0 {
			t.Fatalf("unbounded wsm.on registration for %s", key)
		}
		if (commas == 2) == claim {
			return js[start:end]
		}
		from = end
	}
}

// jsFuncBody extracts the source of `function <name>(...)` from js up to the
// first column-0 closing brace. Fails the test cleanly (instead of panicking
// on a negative slice index) when the function is missing.
func jsFuncBody(t *testing.T, js, name string) string {
	t.Helper()
	decl := "function " + name + "("
	start := strings.Index(js, decl)
	if start < 0 {
		t.Fatalf("could not find %q in dashboard.js", decl)
	}
	end := strings.Index(js[start:], "\n}")
	if end < 0 {
		t.Fatalf("could not bound %s body", name)
	}
	return js[start : start+end]
}

func discoveredKeyWant(pid int, node string) string {
	return "_discovered:" + strconv.Itoa(pid) + ":" + node
}

// TestDashboardJS_VoiceCap_StateMachine pins item 5's static shape; the
// gesture sequence itself is exercised in test/e2e/voice_cap_gesture.test.js.
func TestDashboardJS_VoiceCap_StateMachine(t *testing.T) {
	t.Parallel()
	// #2558 D4-2: the voice module moved to voice.js.
	data, err := voiceJS.ReadFile("static/voice.js")
	if err != nil {
		t.Fatalf("read voice.js: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, "\n  state: 'idle',\n};") {
		t.Fatal("voice.js must declare the voiceState lifecycle (idle/recording/finalizing) (#2435)")
	}
	stop := extractJSFunction(t, js, "stopVoiceRecording")
	if !strings.Contains(stop, "if (voiceRec.state === 'finalizing') return;") {
		t.Error("stopVoiceRecording must ignore calls once the recording is finalizing")
	}
	if !strings.Contains(stop, "clearInterval(voiceRec.timer)") {
		t.Error("stopVoiceRecording must clear voiceRecTimer so the cap toast fires once")
	}
	for _, fn := range []string{"voiceTouchEnd", "voiceTouchCancel"} {
		if !strings.Contains(extractJSFunction(t, js, fn), "finishVoiceGesture(") {
			t.Errorf("%s must route through finishVoiceGesture", fn)
		}
	}
	if !strings.Contains(extractJSFunction(t, js, "hideVoiceOverlay"), "voiceRec.state = 'idle';") {
		t.Error("hideVoiceOverlay must return the lifecycle to idle")
	}
	move := extractJSFunction(t, js, "voiceTouchMove")
	if !strings.Contains(move, "if (voiceRec.state !== 'recording') return;") {
		t.Error("voiceTouchMove must ignore the cancel gesture once finalizing")
	}
	tr := extractJSFunction(t, js, "transcribeAudio")
	if strings.Contains(tr, "autoSend ? data.text") {
		t.Error("transcribeAudio must append the transcript to the draft instead of replacing it on auto-send")
	}
}

// TestDashboardJS_VoiceFinalizing_HasEscapeHatches pins review F2/F3 on
// #2469: finalizing must not be terminal (transcribe timeout, mode toggle)
// and the cancel flag is cleared only when a recording really starts.
func TestDashboardJS_VoiceFinalizing_HasEscapeHatches(t *testing.T) {
	t.Parallel()
	// #2558 D4-2: the voice module moved to voice.js.
	data, err := voiceJS.ReadFile("static/voice.js")
	if err != nil {
		t.Fatalf("read voice.js: %v", err)
	}
	js := string(data)
	tr := extractJSFunction(t, js, "transcribeAudio")
	for _, want := range []string{"new AbortController()", "ac.abort(), TRANSCRIBE_TIMEOUT_MS", "signal: ac.signal", "clearTimeout(timeoutId)"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcribeAudio must bound the fetch with an AbortController timeout, missing %q", want)
		}
	}
	if !strings.Contains(extractJSFunction(t, js, "toggleInputMode"), "hideVoiceOverlay();") {
		t.Error("toggleInputMode must return voiceState to idle via hideVoiceOverlay")
	}
	for _, fn := range []string{"voiceTouchStart", "voiceMouseDown"} {
		body := extractJSFunction(t, js, fn)
		call := strings.Index(body, "startVoiceRecording();")
		if call < 0 {
			t.Fatalf("%s must call startVoiceRecording", fn)
		}
		// Only the press path matters; voiceMouseDown's move closure legitimately
		// toggles the flag while recording.
		if strings.Contains(body[:call], "voiceRec.cancelled = false;") {
			t.Errorf("%s must not clear voiceCancelled before startVoiceRecording's guard (F3)", fn)
		}
	}
	start := extractJSFunction(t, js, "startVoiceRecording")
	guard := strings.Index(start, "voiceRec.state === 'finalizing') return;")
	clear := strings.Index(start, "voiceRec.cancelled = false;")
	if guard < 0 || clear < 0 || clear < guard {
		t.Error("startVoiceRecording must clear voiceCancelled only after its early-return guard (F3)")
	}
}

// Item 4: the palette's 「快速新建」 row must describe the selected node's
// default workspace, not the local defaultWorkspace path when a remote node
// is selected (the backend provides no per-node default workspace, so the
// remote hint is "<node> · 默认工作区").
func TestDashboardJS_QuickRow_FollowsSelectedNode(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	row := extractJSFunction(t, js, "buildQuickRow")
	if !strings.Contains(row, "quickRowHint(") {
		t.Errorf("buildQuickRow must derive its subtitle from quickRowHint(selectedNode), got:\n%s", row)
	}
	script := `
// quickRowHint lives in auth_modal.js — shared state arrives via the
// state.js objects and shortPath / getNodeDisplayName are imports (from
// utilities.js and session_ident.js); stub both surfaces.
const serverInfo = { defaultWorkspace: '/home/ec2-user/workspace/naozhi' };
const sessionList = { nodesData: { n1: { display_name: 'GPU 盒子' }, n2: {} } };
const shortPath = (p) => p.replace('/home/ec2-user', '~');
const getNodeDisplayName = (id) => {
  if (!id || id === 'local') return '本地';
  const nd = sessionList.nodesData[id];
  return nd && nd.display_name ? nd.display_name : id;
};
` + extractJSFunction(t, js, "quickRowHint") + `
const out = { local: quickRowHint('local'), empty: quickRowHint(''), n1: quickRowHint('n1'), n2: quickRowHint('n2') };
serverInfo.defaultWorkspace = '';
out.localNoWs = quickRowHint('local');
process.stdout.write(JSON.stringify(out));
`
	var res map[string]string
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res["local"] != "~/workspace/naozhi" || res["empty"] != "~/workspace/naozhi" {
		t.Errorf("local node must show the (shortened) local defaultWorkspace: %+v", res)
	}
	if res["localNoWs"] != "" {
		t.Errorf("local node with no defaultWorkspace must show nothing, got %q", res["localNoWs"])
	}
	for _, id := range []string{"n1", "n2"} {
		if strings.Contains(res[id], "naozhi") || strings.Contains(res[id], "~") {
			t.Errorf("remote node %s must not show the LOCAL default workspace: %q", id, res[id])
		}
		if !strings.Contains(res[id], "默认工作区") {
			t.Errorf("remote node %s hint must say 默认工作区: %q", id, res[id])
		}
	}
	if !strings.Contains(res["n1"], "GPU 盒子") {
		t.Errorf("remote hint must name the node via getNodeDisplayName: %q", res["n1"])
	}
	if !strings.Contains(res["n2"], "n2") {
		t.Errorf("remote hint falls back to the node id: %q", res["n2"])
	}
}

// Item 6: a null / failed remote backends manifest must not be cached for
// 60s — the next call has to refetch so a transient proxy error does not pin
// the picker to "no backends" for a minute.
func TestDashboardJS_FetchCLIBackends_DoesNotCacheNullRemote(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	script := `
let NZ_CONTRACT;
let cliBackends = null, cliBackendsFetchedAt = 0;
// fetchCLIBackends lives in backend_catalog.js — it imports applyFeatureGates
// and keeps the backend caches in serverInfo.
const serverInfo = { cliBackends: null, cliBackendsFetchedAt: 0, cliBackendsByNode: {} };
function applyFeatureGates() {}
const good = { backends: [{ id: 'claude' }, { id: 'codex' }], default: 'claude' };
let calls = 0;
const responses = [null, () => { throw new Error('502'); }, good, good];
async function fetchJSON() {
  calls++;
  const r = responses.shift();
  return typeof r === 'function' ? r() : r;
}
` + extractJSAsyncFunction(t, js, "fetchCLIBackends") + `
(async () => {
  ({ NZ_CONTRACT } = await import(require('url').pathToFileURL(require('path').resolve('static/contract.js')).href));
  const a = await fetchCLIBackends('n1');
  const b = await fetchCLIBackends('n1');
  const c = await fetchCLIBackends('n1');
  const d = await fetchCLIBackends('n1'); // cached
  process.stdout.write(JSON.stringify({ a, b, c, d, calls, cached: !!serverInfo.cliBackendsByNode.n1 }));
})();
`
	var res struct {
		A, B   json.RawMessage
		C, D   *struct{ Default string }
		Calls  int
		Cached bool
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if string(res.A) != "null" || string(res.B) != "null" {
		t.Errorf("first two calls must surface the failures (null), got a=%s b=%s", res.A, res.B)
	}
	if res.C == nil || res.C.Default != "claude" {
		t.Errorf("third call must REFETCH after a null result instead of serving the cached null; got %+v (calls=%d)", res.C, res.Calls)
	}
	if res.Calls != 3 {
		t.Errorf("fetch count = %d, want 3 (null, error, good; fourth served from cache)", res.Calls)
	}
	if !res.Cached || res.D == nil {
		t.Errorf("a good manifest must still be cached per node (cached=%v d=%+v)", res.Cached, res.D)
	}
	// Static: the picker slot must offer a retry when a remote manifest fails.
	refresh := extractJSFunction(t, js, "refreshBackendPicker")
	if !strings.Contains(refresh, "renderBackendFetchFailed(") {
		t.Error("refreshBackendPicker must render renderBackendFetchFailed(...) (retry affordance) when a remote manifest is null (#2429)")
	}
	// The FIRST-open paths seed the slot from renderBackendPicker(null) -> ''
	// and only repaint via refreshBackendPicker on a node switch; both must
	// route a null remote manifest through refreshBackendPicker on open too.
	palette := extractJSFunction(t, js, "openProjectPalette")
	if !strings.Contains(palette, "if (!backendsData && (selection.node || 'local') !== 'local') refreshBackendPicker('cp-backend-slot');") {
		t.Error("openProjectPalette must call refreshBackendPicker('cp-backend-slot') when opened with a null manifest on a remote node (#2429 M1)")
	}
	create := extractJSFunction(t, js, "createNewSession")
	if !strings.Contains(create, "if (!backendsData && (selection.node || 'local') !== 'local') refreshBackendPicker('new-backend-slot');") {
		t.Error("createNewSession no-projects modal must call refreshBackendPicker('new-backend-slot') when opened with a null manifest on a remote node (#2429 M1)")
	}
}

func TestDashboard_DiscoveredKeyIncludesNode(t *testing.T) {
	js := readDashboardJS(t)

	// Static contract: every discovered lookup/removal goes through the
	// node-aware helpers; no pid-only find/filter and only one literal key
	// concatenation (inside discoveredKey itself).
	for _, bad := range []string{
		"discoveredItems.find(x => x.pid === pid)",
		"discoveredItems.filter(x => x.pid !== pid)",
		"discoveredItems.filter(d => d.pid !== pid)",
		"discoveredItems.filter(d => d.pid !== pd.pid)",
		"pendingDiscovered.pid === pid)",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("dashboard.js still keys discovered items by pid only: %q (#2431 item 6)", bad)
		}
	}
	if n := strings.Count(js, "'_discovered:' + "); n != 1 {
		t.Errorf("'_discovered:' literal concatenations = %d, want exactly 1 (discoveredKey helper)", n)
	}

	script := extractJSFunction(t, js, "discoveredKey") +
		extractJSFunction(t, js, "sameDiscovered") +
		extractJSFunction(t, js, "parseDiscoveredPid") +
		extractJSFunction(t, js, "findDiscovered") +
		extractJSFunction(t, js, "dropDiscovered") + `
// These helpers live in discovery.js, where the discovered set is read
// from sessionList — mirror that surface.
const sessionList = { discoveredItems: [
  { pid: 4242, node: 'local', session_id: 'a' },
  { pid: 4242, node: 'remote1', session_id: 'b' },
  { pid: 7, session_id: 'c' },
] };
const out = {};
out.keyLocal = discoveredKey(4242, 'local');
out.keyRemote = discoveredKey(4242, 'remote1');
out.keyDefault = discoveredKey(7, '');
out.pid = parseDiscoveredPid(out.keyRemote);
out.findRemote = (findDiscovered(4242, 'remote1') || {}).session_id || null;
out.findLocal = (findDiscovered(4242, 'local') || {}).session_id || null;
out.findDefault = (findDiscovered(7, undefined) || {}).session_id || null;
out.findMissing = findDiscovered(4242, 'nope');
dropDiscovered(4242, 'local');
out.afterDrop = sessionList.discoveredItems.map(d => d.session_id);
process.stdout.write(JSON.stringify(out));
`
	var got struct {
		KeyLocal, KeyRemote, KeyDefault string
		Pid                             int
		FindRemote, FindLocal           string
		FindDefault                     string
		FindMissing                     interface{}
		AfterDrop                       []string
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if got.KeyLocal == got.KeyRemote {
		t.Errorf("discoveredKey must differ across nodes for the same pid: %q", got.KeyLocal)
	}
	if got.KeyDefault != discoveredKeyWant(7, "local") {
		t.Errorf("discoveredKey(7, '') = %q, want %q", got.KeyDefault, discoveredKeyWant(7, "local"))
	}
	if got.Pid != 4242 {
		t.Errorf("parseDiscoveredPid = %d, want 4242", got.Pid)
	}
	if got.FindRemote != "b" || got.FindLocal != "a" || got.FindDefault != "c" {
		t.Errorf("findDiscovered picked wrong items: remote=%q local=%q default=%q", got.FindRemote, got.FindLocal, got.FindDefault)
	}
	if got.FindMissing != nil {
		t.Errorf("findDiscovered on unknown node = %v, want null", got.FindMissing)
	}
	if strings.Join(got.AfterDrop, ",") != "b,c" {
		t.Errorf("dropDiscovered(4242,'local') left %v, want [b c]", got.AfterDrop)
	}
}

// TestDashboardJS_ErrorFrameGuardsPendingSubscribeKey pins that a generic
// `error` frame only clears the pending-subscribe bookkeeping when it is
// about that subscription. agent_subscribe validation failures and the
// node-disconnect broadcast (error{node, "node disconnected"}) are also
// `error` frames and used to wipe an unrelated in-flight subscribe.
func TestDashboardJS_ErrorFrameGuardsPendingSubscribeKey(t *testing.T) {
	t.Parallel()
	errCase := wsOnHandler(t, readDashboardJS(t), "error", false)
	if !strings.Contains(errCase, "msg.key === sessionStream._pendingSubscribeKey") {
		t.Error("error case must compare msg.key against sessionStream._pendingSubscribeKey before clearing pending state")
	}
	if !strings.Contains(errCase, "if (!msg.key && msg.node && msg.error === 'node disconnected')") {
		t.Error("error case must recognise the PurgeNodeSubscriptions frame (keyless + msg.node + 'node disconnected')")
	}
	if !strings.Contains(errCase, "reconcileSelectedNode()") {
		t.Error("node-disconnected error must run reconcileSelectedNode() so selection.node snaps back to local")
	}
}

func TestDashboardJS_NodeDisconnectDeselectsStaleSession(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := wsOnHandler(t, js, "error", false)
	idx := strings.Index(body, "if (!msg.key && msg.node && msg.error === 'node disconnected')")
	if idx < 0 {
		t.Fatal("node-disconnected branch missing")
	}
	branch := body[idx:]
	if end := strings.Index(branch, "reconcileSelectedNode();"); end > 0 {
		branch = branch[:end]
	} else {
		t.Fatal("node-disconnected branch must still call reconcileSelectedNode()")
	}
	// Ownership must come from the session store (sessionsData / perSession.nodes
	// keyed by the dead node), never from selectedNode: that global is the
	// dispatch target and wireNodePicker rewrites it on the new-session
	// picker's change event, so a local session with the picker on n1 would
	// otherwise be wiped when n1 disconnects. Pending sessions stay alone.
	if !strings.Contains(branch, "(sessionList.sessionsData[sid(selection.key, msg.node)] || perSession.nodes[selection.key] === msg.node)") {
		t.Error("node-disconnected branch must resolve the selected session's node from sessionList.sessionsData/perSession.nodes, not selection.node")
	}
	if strings.Contains(branch, "selection.node === msg.node") {
		t.Error("node-disconnected branch must not infer session ownership from selection.node (dispatch target, rewritten by wireNodePicker)")
	}
	if !strings.Contains(branch, "perSession.workspaces[selection.key] === undefined") {
		t.Error("node-disconnected branch must skip pending (never-sent) sessions")
	}
	if !strings.Contains(branch, "deselectNodeSession(msg.node);") {
		t.Error("node-disconnected branch must call deselectNodeSession")
	}
	fn := jsFuncBody(t, js, "deselectNodeSession")
	// deselectNodeSession lives in system_view.js, which reads dashboard state
	// from the state.js objects and imports its helpers.
	for _, want := range []string{
		"if (draft) perSession.drafts[selection.key] = draft;", // keep the operator's text
		"selection.key = null;",
		"main.innerHTML = mainEmptyHtml();",
		"wireQuickAskInput();",
		"已断开",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("deselectNodeSession missing %q", want)
		}
	}
	for _, forbid := range []string{"removePendingSession", "delete perSession.workspaces", "fetch(", "DELETE"} {
		if strings.Contains(fn, forbid) {
			t.Errorf("deselectNodeSession must not touch pending sessions or the backend (%q)", forbid)
		}
	}
}

func TestDashboardJS_HistoryUnavailableOffersRetry(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := wsOnHandler(t, js, "error", false)
	// Only the selected session's own failure: same key AND same node.
	if !strings.Contains(body, "if (msg.error === 'history unavailable' && msg.key === selection.key && msg.node === selection.node) showHistoryRetry();") {
		t.Error("error handler must mount the history retry for the selected session's keyed 'history unavailable'")
	}
	fn := jsFuncBody(t, js, "showHistoryRetry")
	for _, want := range []string{
		"if (el && !el.querySelector('.event')) el.replaceChildren();", // a blank or placeholder pane shows only the retry
		"btn.remove();",
		"if (sessionStream._initialSubscribe) sessionStream.lastEventTimeWs = 0;", // a retry before the opening page asks for it again
		"sessionStream.subscribe(selection.key, selection.node);",
		"历史记录加载失败",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("showHistoryRetry missing %q", want)
		}
	}
}

// TestDashboardJS_SubscribedAckKeepsNodeForNonPendingTab pins the follow-up to
// #2421 review F1. After the relay rebuilds a dropped remote subscription (or
// reconnects), the remote's `subscribed` ack is fanned out to EVERY local tab
// on the key — the relay injects "node" into each forwarded frame and
// ReverseConn sets Node explicitly. A tab that was not pending must therefore
// take the node from the frame, not fall back to 'local': with subscribedNode
// rewritten to 'local', the subscription_timeout handler's node-match guard
// fails and the bookkeeping is never cleared, so the tab never re-subscribes —
// the exact bug the rebuild fixes.
//
// Invariant before: non-pending `subscribed` → subscribedNode = 'local'
// (drops the node of a remote key).
// Invariant after:  subscribedNode = pending node, else the frame's node,
// else 'local' — a remote key never collapses to 'local'.
func TestDashboardJS_SubscribedAckKeepsNodeForNonPendingTab(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	body := wsOnHandler(t, js, "subscribed", false)
	if !strings.Contains(body, "sessionStream.subscribedNode = sessionStream._pendingSubscribeNode || msg.node || 'local'") {
		t.Error("subscribed handler must resolve subscribedNode as `_pendingSubscribeNode || msg.node || 'local'` — a fanned-out remote ack must not rewrite a remote key's node to 'local'")
	}
	if strings.Contains(body, "sessionStream.subscribedNode = sessionStream._pendingSubscribeNode || 'local'") {
		t.Error("subscribed handler must not fall straight back to 'local' when not pending — see rationale above")
	}
}

// TestDashboardJS_RemoteExportPagesHistory: a remote session's node serves the
// before= pages, so the export pager walks them too and every cursor request
// keeps carrying &node=; returning early for remote drops older history.
func TestDashboardJS_RemoteExportPagesHistory(t *testing.T) {
	t.Parallel()
	pager := jsFuncBody(t, readDashboardJS(t), "fetchAllSessionEvents")
	if !strings.Contains(pager, "(remote ? '&node=' + encodeURIComponent(node) : '')") {
		t.Error("fetchAllSessionEvents must carry &node= on a remote session's requests")
	}
	if !strings.Contains(pager, "base + '&before='") {
		t.Error("fetchAllSessionEvents before= pages must be built from the node-carrying base URL")
	}
	if strings.Contains(pager, "if (remote) return") {
		t.Error("fetchAllSessionEvents returns a remote session's ring without paging — its node serves before= pages, so older history is dropped")
	}
}

// TestDashboardJS_LoadEarlierStaleGuardIncludesNode: the same key on another
// node is a different session, so a node switch while a page is in flight
// must drop the page instead of grafting it into the new transcript.
func TestDashboardJS_LoadEarlierStaleGuardIncludesNode(t *testing.T) {
	t.Parallel()
	body := jsFuncBody(t, readDashboardJS(t), "loadEarlierEvents")
	for _, want := range []string{
		"const node = selection.node;",
		"const stale = () => selection.key !== key || selection.node !== node || gen !== transcript.earlierGen;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("loadEarlierEvents missing stale guard piece: %q", want)
		}
	}
}
