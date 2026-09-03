package cli

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
// shimMsg only had `line`) was the first silent drift. The four tests in
// this file are the contract that makes any future drift a CI failure:
//
//  1. TestShimMsgIsSubsetOfServerMsg — every shimMsg field's json tag must
//     name a ServerMsg field of a compatible type (identical, or the
//     shimMsgCode ↔ *int pair). A phantom / renamed field on the cli side
//     fails here.
//  2. TestShimMsgRoundTripsEveryShimFrame — one sample ServerMsg per frame
//     type the shim emits, encoded through the shim's real marshal paths,
//     decoded through both decoders, compared field-by-field. Any
//     ServerMsg field the shim populates that shimMsg cannot see must be
//     listed in liveDecoderIgnoredFields, so a new shim field is a
//     conscious decision (add to shimMsg, or declare it ignored) rather
//     than a silent drop.
//  3. TestServerMsgFieldsPartitionedByLiveDecoder — structural reverse
//     check independent of sample values: every ServerMsg key is either
//     decoded by shimMsg or declared ignored, so a new ServerMsg field is
//     flagged the moment it is added.
//  4. TestShimMsgContractCoversEveryEmittedFrameType — AST-scans
//     internal/shim for `ServerMsg{Type: "..."}` literals and requires the
//     sample table to cover exactly that set, so a new frame type cannot
//     ship without a contract sample.
//
// Production code is intentionally untouched: shimMsg stays a separate
// struct (perf) and is pinned to ServerMsg by these tests instead.

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

var (
	shimMsgCodeType = reflect.TypeOf(shimMsgCode{})
	intPtrType      = reflect.TypeOf((*int)(nil))
)

// decodeTypesCompatible reports whether a shimMsg field of type cliT can
// legitimately decode the same JSON value as a ServerMsg field of type
// shimT. Identical types always qualify; the only sanctioned divergence is
// the allocation-free shimMsgCode standing in for *int (R222-PERF-13).
func decodeTypesCompatible(cliT, shimT reflect.Type) bool {
	if cliT == shimT {
		return true
	}
	return cliT == shimMsgCodeType && shimT == intPtrType
}

// TestShimMsgIsSubsetOfServerMsg pins cli.shimMsg as a strict projection of
// shim.ServerMsg: every json key shimMsg decodes must exist on ServerMsg
// with a decode-compatible type. Adding a field to ServerMsg alone is fine
// (see the round-trip test for the "did cli notice" half); adding, renaming
// or retyping a field on shimMsg that ServerMsg does not carry fails here.
func TestShimMsgIsSubsetOfServerMsg(t *testing.T) {
	t.Parallel()
	serverFields := serverMsgFieldsByTag(t)
	cliT := reflect.TypeOf(shimMsg{})
	for i := 0; i < cliT.NumField(); i++ {
		cf := cliT.Field(i)
		key := jsonTagName(t, "cli.shimMsg", cf)
		sf, ok := serverFields[key]
		if !ok {
			t.Errorf("cli.shimMsg.%s decodes json key %q which shim.ServerMsg does not emit — "+
				"the live decoder has drifted from the wire contract", cf.Name, key)
			continue
		}
		if !decodeTypesCompatible(cf.Type, sf.Type) {
			t.Errorf("json key %q: cli.shimMsg.%s is %s but shim.ServerMsg.%s is %s — "+
				"decode-incompatible types (only shimMsgCode↔*int is sanctioned)",
				key, cf.Name, cf.Type, sf.Name, sf.Type)
		}
	}
}

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

// TestServerMsgFieldsPartitionedByLiveDecoder is the structural half of the
// reverse direction: every shim.ServerMsg json key must be either decoded by
// cli.shimMsg or declared in liveDecoderIgnoredFields (for some frame type).
// Unlike the round-trip test this does not depend on sample values, so a
// field added to ServerMsg is flagged even before any sample populates it —
// the author must add it to shimMsg or record why the live path ignores it.
func TestServerMsgFieldsPartitionedByLiveDecoder(t *testing.T) {
	t.Parallel()
	decoded := make(map[string]bool)
	cliT := reflect.TypeOf(shimMsg{})
	for i := 0; i < cliT.NumField(); i++ {
		decoded[jsonTagName(t, "cli.shimMsg", cliT.Field(i))] = true
	}
	ignored := make(map[string]bool)
	for _, keys := range liveDecoderIgnoredFields {
		for k := range keys {
			ignored[k] = true
		}
	}
	for key, sf := range serverMsgFieldsByTag(t) {
		switch {
		case decoded[key] && ignored[key]:
			t.Errorf("json key %q (ServerMsg.%s) is both decoded by cli.shimMsg and listed in liveDecoderIgnoredFields — drop the stale ignore entry", key, sf.Name)
		case !decoded[key] && !ignored[key]:
			t.Errorf("json key %q (ServerMsg.%s) is neither decoded by cli.shimMsg nor declared in liveDecoderIgnoredFields — "+
				"add it to shimMsg or record the ignore decision", key, sf.Name)
		}
	}
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
			t.Fatalf("json key %q missing on shim.ServerMsg (subset test should have caught this)", key)
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
			t.Fatalf("json key %q: type mismatch %s vs %s (subset test should have caught this)", key, cf.Type, sf.Type)
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

// emittedServerMsgTypes AST-scans the non-test sources of internal/shim for
// composite literals `ServerMsg{... Type: "<lit>" ...}` and returns the set
// of frame type strings the shim can put on the wire.
func emittedServerMsgTypes(t *testing.T) map[string]bool {
	t.Helper()
	// `go test` runs each package's tests with cwd = the package directory
	// (internal/cli), so the sibling shim package is one level up. Same
	// precedent as internal/dispatch/no_cron_import_test.go; if the layout
	// changes the len(types)==0 guard below fails loudly instead of passing.
	shimDir := filepath.Join("..", "shim")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, shimDir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", shimDir, err)
	}
	types := make(map[string]bool)
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if !isServerMsgType(lit.Type) {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, ok := kv.Key.(*ast.Ident)
					if !ok || k.Name != "Type" {
						continue
					}
					bl, ok := kv.Value.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						t.Errorf("%s: ServerMsg{Type: <non-literal>} — frame types must be string literals so this contract can enumerate them",
							fset.Position(kv.Pos()))
						continue
					}
					v, err := strconv.Unquote(bl.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", fset.Position(bl.Pos()), bl.Value, err)
					}
					types[v] = true
				}
				return true
			})
		}
	}
	if len(types) == 0 {
		t.Fatalf("no ServerMsg{Type: ...} literals found under %s — scan path broken?", shimDir)
	}
	return types
}

// isServerMsgType matches both the unqualified `ServerMsg{...}` used inside
// package shim and a qualified `shim.ServerMsg{...}` should a file under
// internal/shim ever construct it via a selector (e.g. a nested package).
func isServerMsgType(expr ast.Expr) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name == "ServerMsg"
	case *ast.SelectorExpr:
		return x.Sel.Name == "ServerMsg"
	}
	return false
}

func sortedFrameTypes(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestShimMsgContractCoversEveryEmittedFrameType requires the sample table
// to match exactly the frame types internal/shim constructs. A new
// ServerMsg{Type: "x"} in the shim without a sample here fails CI; a stale
// sample for a type the shim no longer emits fails too.
func TestShimMsgContractCoversEveryEmittedFrameType(t *testing.T) {
	t.Parallel()
	emitted := emittedServerMsgTypes(t)
	sampled := make(map[string]bool)
	for _, s := range shimFrameSamples() {
		sampled[s.msg.Type] = true
	}
	for _, ty := range sortedFrameTypes(emitted) {
		if !sampled[ty] {
			t.Errorf("shim emits frame type %q but shimFrameSamples has no sample for it — add one so the live decoder is contract-tested", ty)
		}
	}
	for _, ty := range sortedFrameTypes(sampled) {
		if !emitted[ty] {
			t.Errorf("shimFrameSamples has type %q but no ServerMsg{Type: %q} literal exists in internal/shim — stale sample", ty, ty)
		}
	}
}
