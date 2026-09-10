package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sendEnginePkg is the set of files rule 3b-send reads. A "" value writes
// nothing so a missing-file case can be exercised.
type sendEnginePkg struct {
	hub, engine, send, ownerLoop, handler string
}

func writeSendEnginePkg(t *testing.T, p sendEnginePkg) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"wshub.go":           p.hub,
		"send_engine.go":     p.engine,
		"send.go":            p.send,
		"send_owner_loop.go": p.ownerLoop,
		"dashboard_send.go":  p.handler,
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

const hubOK = "package server\ntype Hub struct {\n\tengine *sendEngine\n\trouter HubRouter\n}\nfunc (h *Hub) Shutdown() {}\n"

const engineOK = `package server
type sendEngine struct {
	queue MessageEnqueuer
	guard *session.Guard
	wg sync.WaitGroup
	allowedRoot string
	notify sendNotifier
}
func (e *sendEngine) TrackSend() {}
func (e *sendEngine) sessionSend() {}
func (e *sendEngine) validateWorkspace(p string) (string, error) { return p, nil }
`

// handlerOK calls engine methods only — the shape #2632 landed.
const handlerOK = `package server
type SendHandler struct {
	engine *sendEngine
	uploadStore *uploadStore
}
func (h *SendHandler) handleBind() {
	h.engine.sessionSend()
	if _, err := h.engine.validateWorkspace("x"); err != nil {
		return
	}
	release, shuttingDown := h.engine.TrackSend()
	_ = release
	_ = shuttingDown
}
`

func cleanPkg() sendEnginePkg {
	return sendEnginePkg{
		hub:       hubOK,
		engine:    engineOK,
		send:      "package server\nfunc (e *sendEngine) sessionSendLegacy() {}\n",
		ownerLoop: "package server\nfunc (e *sendEngine) ownerLoop() {}\n",
		handler:   handlerOK,
	}
}

func msgs(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestSendEngineOwnership_CleanLayout is the negative control: the shape #2632
// landed must produce nothing, or every other case here proves nothing.
func TestSendEngineOwnership_CleanLayout(t *testing.T) {
	dir := writeSendEnginePkg(t, cleanPkg())
	if vs := scanSendEngineOwnership(dir); len(vs) != 0 {
		t.Errorf("clean layout reported %d violation(s):\n%s", len(vs), msgs(vs))
	}
}

// TestSendEngineOwnership_FlagsHubFieldOnEngine is check A for the engine: a
// `hub *Hub` back-pointer is the regression RFC §2.1 rules out, and the one a
// six-name field blocklist could never see.
func TestSendEngineOwnership_FlagsHubFieldOnEngine(t *testing.T) {
	p := cleanPkg()
	p.engine = "package server\ntype sendEngine struct {\n\thub *Hub\n\tqueue MessageEnqueuer\n}\nfunc (e *sendEngine) sessionSend() {}\nfunc (e *sendEngine) validateWorkspace(p string) (string, error) { return p, nil }\nfunc (e *sendEngine) TrackSend() (func(), bool) { return nil, false }\n"
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
	if len(vs) != 1 {
		t.Fatalf("want 1 violation, got %d:\n%s", len(vs), msgs(vs))
	}
	if !strings.Contains(vs[0].Message, `sendEngine declares field "hub" of type *Hub`) {
		t.Errorf("unexpected message: %s", vs[0].Message)
	}
	if vs[0].Line == 0 {
		t.Errorf("field violation has no line number: %+v", vs[0])
	}
}

// TestSendEngineOwnership_FlagsHubFieldOnSendHandler is check A for the HTTP
// side, including the embedded form (`*Hub` with no field name).
func TestSendEngineOwnership_FlagsHubFieldOnSendHandler(t *testing.T) {
	p := cleanPkg()
	p.handler = "package server\ntype SendHandler struct {\n\tengine *sendEngine\n\t*Hub\n}\n"
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
	if len(vs) != 1 {
		t.Fatalf("want 1 violation, got %d:\n%s", len(vs), msgs(vs))
	}
	if !strings.Contains(vs[0].Message, "SendHandler declares embedded *Hub") {
		t.Errorf("unexpected message: %s", vs[0].Message)
	}
}

// TestSendEngineOwnership_FlagsEngineFieldReadInHandler is check C: the handler
// reading engine state instead of calling a method. Method calls in the same
// file must NOT be flagged, otherwise the rule forbids the intended shape.
func TestSendEngineOwnership_FlagsEngineFieldReadInHandler(t *testing.T) {
	p := cleanPkg()
	p.handler = `package server
type SendHandler struct {
	engine *sendEngine
}
func (h *SendHandler) handleSend() {
	h.engine.sessionSend()                 // method: fine
	root := h.engine.allowedRoot           // field: flagged
	h.engine.notify.BroadcastSessionsUpdate() // field then method: flagged (notify)
	_ = root
}
`
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
	if len(vs) != 2 {
		t.Fatalf("want 2 violations (allowedRoot, notify), got %d:\n%s", len(vs), msgs(vs))
	}
	for _, want := range []string{`reads engine field "allowedRoot"`, `reads engine field "notify"`} {
		if !strings.Contains(msgs(vs), want) {
			t.Errorf("missing %q in:\n%s", want, msgs(vs))
		}
	}
	for _, v := range vs {
		if v.Line == 0 {
			t.Errorf("field-read violation has no line number: %+v", v)
		}
	}
}

// TestSendEngineOwnership_FlagsHubReceiverInPipelineFile is check B: a pipeline
// method written back onto the Hub. Also the shape lookupNode had — a non-send
// method parked in a send file, which is what made #2551's method count read 13
// instead of 12.
func TestSendEngineOwnership_FlagsHubReceiverInPipelineFile(t *testing.T) {
	p := cleanPkg()
	p.send = "package server\nfunc (e *sendEngine) sessionSendLegacy() {}\nfunc (h *Hub) sneakyHelper() {}\n"
	p.ownerLoop = "package server\nfunc (h *Hub) ownerLoop() {}\n"
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
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
	p := cleanPkg()
	p.engine = "package server\ntype somethingElse struct{}\n"
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "type sendEngine not found") {
		t.Fatalf("want a single 'sendEngine not found' violation, got %d:\n%s", len(vs), msgs(vs))
	}
}

// TestSendEngineOwnership_FlagsMissingHandlerFile: check C must not pass
// vacuously when dashboard_send.go is gone.
func TestSendEngineOwnership_FlagsMissingHandlerFile(t *testing.T) {
	p := cleanPkg()
	p.handler = ""
	vs := scanSendEngineOwnership(writeSendEnginePkg(t, p))
	var sawType, sawFile bool
	for _, v := range vs {
		sawType = sawType || strings.Contains(v.Message, "type SendHandler not found")
		sawFile = sawFile || strings.Contains(v.Message, "dashboard_send.go not found")
	}
	if !sawType || !sawFile {
		t.Fatalf("want 'SendHandler not found' and 'dashboard_send.go not found', got:\n%s", msgs(vs))
	}
}
