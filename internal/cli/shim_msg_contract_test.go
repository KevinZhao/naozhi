package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// R202606f-ARCH-2 (#2299): the shim → naozhi wire frame has two decoders.
// shim.ServerMsg (internal/shim/protocol.go) is the authoritative contract,
// used by shim.Manager for the attach handshake / replay drain. cli.shimMsg
// (process_readloop.go) is a hand-maintained projection used on the live
// readLoop hot path and in wrapper.go's Spawn handshake; it exists so the
// `code` field can decode into shimMsgCode without the *int allocation
// json.Unmarshal would otherwise perform per cli_exited frame (R222-PERF-13).
//
// Nothing tied the two together: ARCH-1 (error frames carry text in `msg`,
// shimMsg only had `line`) was the first silent drift.
// TestShimMsgRoundTripsEveryShimFrame is the contract: one sample ServerMsg
// per frame type the shim emits, encoded through the shim's real marshal
// paths, decoded through both decoders and compared field-by-field. Every
// shimMsg key must exist on ServerMsg with a compatible type, and any
// ServerMsg field populated in shimFrameSamples that shimMsg cannot see must
// be listed in liveDecoderIgnoredFields. The reverse check only covers fields
// the samples populate: when adding a ServerMsg field, add it to
// shimFrameSamples too.
//
// Production code is intentionally untouched: shimMsg stays a separate
// struct (perf) and is pinned to ServerMsg by this test instead.

// jsonTagName returns the JSON key a struct field decodes from. Contract
// fields must carry an explicit tag; an untagged field is a drift signal.
func jsonTagName(t *testing.T, owner string, f reflect.StructField) string {
	t.Helper()
	tag, ok := f.Tag.Lookup("json")
	if !ok || tag == "" || tag == "-" {
		t.Fatalf("%s.%s has no usable json tag (%q); wire contract fields must be explicitly tagged", owner, f.Name, tag)
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		t.Fatalf("%s.%s json tag %q has empty key", owner, f.Name, tag)
	}
	return name
}

// serverMsgFieldsByTag indexes shim.ServerMsg fields by json key.
func serverMsgFieldsByTag(t *testing.T) map[string]reflect.StructField {
	t.Helper()
	rt := reflect.TypeOf(shim.ServerMsg{})
	out := make(map[string]reflect.StructField, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		out[jsonTagName(t, "shim.ServerMsg", f)] = f
	}
	return out
}

var shimMsgCodeType = reflect.TypeOf(shimMsgCode{})

// liveDecoderIgnoredFields lists, per frame type, the ServerMsg json keys
// the shim populates that cli.shimMsg deliberately does NOT decode.
//
// hello / replay_done are consumed by shim.Manager (manager.go attach +
// drainReplay) via ServerMsg before the connection is handed to
// cli.Process; readLoop never observes their payload. pong is a pure
// liveness signal for the heartbeat loop — its payload is unused.
//
// Adding a key here is a conscious "cli does not need this" decision and
// should be justified in the commit; the alternative is adding the field
// to shimMsg (see ARCH-1 for what happens when neither is done).
var liveDecoderIgnoredFields = map[string]map[string]bool{
	"hello": {
		"shim_pid":         true,
		"cli_pid":          true,
		"cli_alive":        true,
		"session_id":       true,
		"buffer_seq_start": true,
		"buffer_seq_end":   true,
		"protocol_version": true,
	},
	"replay_done": {"count": true},
	"pong":        {"cli_alive": true, "buffered": true},
}

// shimFrameSample is one representative frame the shim emits. encode
// defaults to (*ServerMsg).MarshalLine; stdout / replay override it with
// the shim's allocation-free fast encoders so the contract is exercised
// against the bytes the hot path actually produces.
type shimFrameSample struct {
	name   string
	msg    shim.ServerMsg
	encode func(shim.ServerMsg) ([]byte, error)
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// shimFrameSamples mirrors every ServerMsg construction in internal/shim
// (server.go, protocol.go). Values are non-zero wherever the shim sets the
// field so omitempty cannot hide a missing decode.
func shimFrameSamples() []shimFrameSample {
	return []shimFrameSample{
		{name: "hello", msg: shim.ServerMsg{
			Type: "hello", ShimPID: 4242, CLIPID: 4243, CLIAlive: boolPtr(true),
			SessionID: "sess-1", BufferSeqStart: 10, BufferSeqEnd: 20,
			ProtocolVersion: shim.ProtocolVersion,
		}},
		{name: "replay", msg: shim.ServerMsg{Type: "replay", Seq: 11, Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"héllo \"quoted\""}]}}`},
			encode: func(m shim.ServerMsg) ([]byte, error) { return shim.MarshalReplayLine(m.Seq, []byte(m.Line)) }},
		{name: "replay_done", msg: shim.ServerMsg{Type: "replay_done", Count: 10}},
		{name: "stdout", msg: shim.ServerMsg{Type: "stdout", Seq: 21, Line: `{"type":"result","subtype":"success","is_error":false}`},
			encode: func(m shim.ServerMsg) ([]byte, error) { return shim.MarshalStdoutLine(m.Seq, []byte(m.Line)) }},
		{name: "stderr", msg: shim.ServerMsg{Type: "stderr", Line: "warn: something\ttabbed"}},
		{name: "cli_exited/code", msg: shim.ServerMsg{Type: "cli_exited", Code: intPtr(3)}},
		// Explicit zero must survive as Present=true/Value=0, not "absent".
		{name: "cli_exited/code-zero", msg: shim.ServerMsg{Type: "cli_exited", Code: intPtr(0)}},
		{name: "cli_exited/code-negative", msg: shim.ServerMsg{Type: "cli_exited", Code: intPtr(-1)}},
		// Signal is declared on the wire contract and consumed by
		// handleShimCLIExited even though today's shim only sets Code.
		{name: "cli_exited/signal", msg: shim.ServerMsg{Type: "cli_exited", Signal: "SIGKILL"}},
		{name: "cli_exited/stderr-tail", msg: shim.ServerMsg{Type: "cli_exited", Code: intPtr(1),
			StderrTail: []string{"Error: No conversation found", "héllo \"quoted\""}}},
		{name: "pong", msg: shim.ServerMsg{Type: "pong", CLIAlive: boolPtr(false), Buffered: 12}},
		{name: "auth_failed", msg: shim.ServerMsg{Type: "auth_failed", Msg: "invalid token"}},
		{name: "error", msg: shim.ServerMsg{Type: "error", Msg: "another client is connected"}},
	}
}

// assertShimMsgProjects checks, for every shimMsg field, that the value the
// live decoder produced equals the corresponding ServerMsg field. It also
// walks the ServerMsg side to catch fields the shim populated that shimMsg
// has no counterpart for (unless declared in liveDecoderIgnoredFields).
func assertShimMsgProjects(t *testing.T, frameType string, sm shim.ServerMsg, cm shimMsg) {
	t.Helper()
	serverFields := serverMsgFieldsByTag(t)
	sv := reflect.ValueOf(sm)
	cv := reflect.ValueOf(cm)
	cliT := cv.Type()

	seen := make(map[string]bool, cliT.NumField())
	for i := 0; i < cliT.NumField(); i++ {
		cf := cliT.Field(i)
		key := jsonTagName(t, "cli.shimMsg", cf)
		sf, ok := serverFields[key]
		if !ok {
			t.Fatalf("json key %q missing on shim.ServerMsg", key)
		}
		seen[key] = true
		cfv := cv.Field(i)
		sfv := sv.FieldByIndex(sf.Index)

		if cf.Type == shimMsgCodeType {
			code := cfv.Interface().(shimMsgCode)
			ptr := sfv.Interface().(*int)
			if code.Present != (ptr != nil) {
				t.Errorf("%q: Code.Present=%v but ServerMsg.Code nil=%v", key, code.Present, ptr == nil)
			} else if ptr != nil && code.Value != int64(*ptr) {
				t.Errorf("%q: Code.Value=%d, ServerMsg.Code=%d", key, code.Value, *ptr)
			}
			continue
		}
		if cf.Type != sf.Type {
			t.Fatalf("json key %q: type mismatch %s vs %s", key, cf.Type, sf.Type)
		}
		if !reflect.DeepEqual(cfv.Interface(), sfv.Interface()) {
			t.Errorf("%q: shimMsg=%#v, ServerMsg=%#v", key, cfv.Interface(), sfv.Interface())
		}
	}

	// Reverse direction: anything the shim set that cli did not decode.
	for key, sf := range serverFields {
		if seen[key] {
			continue
		}
		if sv.FieldByIndex(sf.Index).IsZero() {
			continue
		}
		if !liveDecoderIgnoredFields[frameType][key] {
			t.Errorf("frame %q populates json key %q (ServerMsg.%s) which cli.shimMsg does not decode "+
				"and liveDecoderIgnoredFields does not declare ignored — add the field to shimMsg or "+
				"record the decision (R202606f-ARCH-1 class drift)", frameType, key, sf.Name)
		}
	}
}

// TestShimMsgRoundTripsEveryShimFrame encodes each sample through the
// shim's real marshal path and decodes it with both shim.ParseServerMsg
// (authoritative) and cli.shimMsg (live hot path), asserting the two agree
// on every field shimMsg carries and that nothing the shim populated is
// silently dropped.
func TestShimMsgRoundTripsEveryShimFrame(t *testing.T) {
	t.Parallel()
	for _, s := range shimFrameSamples() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			encode := s.encode
			if encode == nil {
				encode = func(m shim.ServerMsg) ([]byte, error) { return m.MarshalLine() }
			}
			line, err := encode(s.msg)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(line) == 0 || line[len(line)-1] != '\n' {
				t.Fatalf("encoder must emit one newline-terminated NDJSON frame, got %q", line)
			}
			payload := line[:len(line)-1]

			authoritative, err := shim.ParseServerMsg(payload)
			if err != nil {
				t.Fatalf("shim.ParseServerMsg: %v", err)
			}
			if !reflect.DeepEqual(authoritative, s.msg) {
				t.Fatalf("ServerMsg round-trip drift:\n got  %#v\n want %#v", authoritative, s.msg)
			}

			var live shimMsg
			if err := json.Unmarshal(payload, &live); err != nil {
				t.Fatalf("cli.shimMsg unmarshal: %v", err)
			}
			if live.Type != s.msg.Type {
				t.Fatalf("Type = %q, want %q", live.Type, s.msg.Type)
			}
			assertShimMsgProjects(t, s.msg.Type, authoritative, live)
		})
	}
}
