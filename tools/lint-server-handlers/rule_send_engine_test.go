package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSendEnginePkg lays out the four files rule 3b-send reads. Each argument
// is the file's full source; "" writes nothing so a missing-file case can be
// exercised.
func writeSendEnginePkg(t *testing.T, hub, engine, send, ownerLoop string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"wshub.go":           hub,
		"send_engine.go":     engine,
		"send.go":            send,
		"send_owner_loop.go": ownerLoop,
	} {
		if src == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const engineOK = `package server
type sendEngine struct {
	queue MessageEnqueuer
	guard *session.Guard
	wg sync.WaitGroup
	trackMu sync.Mutex
	closed bool
	legacyInvokes atomic.Int64
}
func (e *sendEngine) TrackSend() {}
`

func msgs(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestSendEngineOwnership_CleanLayout is the negative control: the shape #2551
// landed must produce nothing, or every other case here proves nothing.
func TestSendEngineOwnership_CleanLayout(t *testing.T) {
	dir := writeSendEnginePkg(t,
		"package server\ntype Hub struct {\n\tengine *sendEngine\n\trouter HubRouter\n}\nfunc (h *Hub) Shutdown() {}\n",
		engineOK,
		"package server\nfunc (e *sendEngine) sessionSend() {}\n",
		"package server\nfunc (e *sendEngine) ownerLoop() {}\n",
	)
	if vs := scanSendEngineOwnership(dir); len(vs) != 0 {
		t.Errorf("clean layout reported %d violation(s):\n%s", len(vs), msgs(vs))
	}
}

// TestSendEngineOwnership_FlagsFieldBackOnHub covers the regression the rule
// exists for: a send field reappearing on Hub, which gives the WS and HTTP paths
// two copies of the same state.
func TestSendEngineOwnership_FlagsFieldBackOnHub(t *testing.T) {
	dir := writeSendEnginePkg(t,
		"package server\ntype Hub struct {\n\tengine *sendEngine\n\tqueue MessageEnqueuer\n\tguard *session.Guard\n}\n",
		engineOK,
		"package server\n", "package server\n",
	)
	vs := scanSendEngineOwnership(dir)
	if len(vs) != 2 {
		t.Fatalf("want 2 violations (queue + guard), got %d:\n%s", len(vs), msgs(vs))
	}
	for _, want := range []string{`Hub declares "queue"`, `Hub declares "guard"`} {
		if !strings.Contains(msgs(vs), want) {
			t.Errorf("missing %q in:\n%s", want, msgs(vs))
		}
	}
}

// TestSendEngineOwnership_FlagsMissingEngineField catches the other direction: a
// field deleted from sendEngine without the rule being updated, which would
// otherwise leave the check silently vacuous.
func TestSendEngineOwnership_FlagsMissingEngineField(t *testing.T) {
	dir := writeSendEnginePkg(t,
		"package server\ntype Hub struct {\n\tengine *sendEngine\n}\n",
		"package server\ntype sendEngine struct {\n\tqueue MessageEnqueuer\n}\n",
		"package server\n", "package server\n",
	)
	vs := scanSendEngineOwnership(dir)
	// guard / wg / trackMu / closed / legacyInvokes are all absent.
	if len(vs) != 5 {
		t.Fatalf("want 5 violations for the absent fields, got %d:\n%s", len(vs), msgs(vs))
	}
	if !strings.Contains(msgs(vs), `sendEngine no longer declares "guard"`) {
		t.Errorf("expected the guard message, got:\n%s", msgs(vs))
	}
}

// TestSendEngineOwnership_FlagsHubReceiverInPipelineFile is check B: a pipeline
// method written back onto the Hub. Also the shape lookupNode had — a non-send
// method parked in a send file, which is what made #2551's method count read 13
// instead of 12.
func TestSendEngineOwnership_FlagsHubReceiverInPipelineFile(t *testing.T) {
	dir := writeSendEnginePkg(t,
		"package server\ntype Hub struct {\n\tengine *sendEngine\n}\n",
		engineOK,
		"package server\nfunc (e *sendEngine) sessionSend() {}\nfunc (h *Hub) sneakyHelper() {}\n",
		"package server\nfunc (h *Hub) ownerLoop() {}\n",
	)
	vs := scanSendEngineOwnership(dir)
	if len(vs) != 2 {
		t.Fatalf("want 2 violations (one per file), got %d:\n%s", len(vs), msgs(vs))
	}
	for _, want := range []string{"sneakyHelper has a *Hub receiver", "ownerLoop has a *Hub receiver"} {
		if !strings.Contains(msgs(vs), want) {
			t.Errorf("missing %q in:\n%s", want, msgs(vs))
		}
	}
	// A violation without a line number is unclickable in CI annotations.
	for _, v := range vs {
		if v.Line == 0 {
			t.Errorf("receiver violation has no line number: %+v", v)
		}
	}
}

// TestSendEngineOwnership_FlagsMissingEngineType guards against the rule going
// quietly vacuous if sendEngine is renamed or moved: no type, no checks, so it
// must say so loudly instead of reporting a clean package.
func TestSendEngineOwnership_FlagsMissingEngineType(t *testing.T) {
	dir := writeSendEnginePkg(t,
		"package server\ntype Hub struct {\n\tengine *sendEngine\n}\n",
		"package server\ntype somethingElse struct{}\n",
		"package server\n", "package server\n",
	)
	vs := scanSendEngineOwnership(dir)
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "type sendEngine not found") {
		t.Fatalf("want a single 'sendEngine not found' violation, got %d:\n%s", len(vs), msgs(vs))
	}
}
