package cli

import (
	"io"
	"testing"
)

// stubProto is a minimal Protocol implementation without Capabilities().
// ProtocolCaps must fall back to SupportsReplay/SupportsPriority/Name.
type stubProto struct {
	name     string
	replay   bool
	priority bool
}

func (s *stubProto) Name() string                                 { return s.name }
func (s *stubProto) Clone() Protocol                              { return s }
func (s *stubProto) BuildArgs(SpawnOptions) []string              { return nil }
func (s *stubProto) Init(*JSONRW, string, string) (string, error) { return "", nil }
func (s *stubProto) WriteMessage(io.Writer, string, []ImageData) error {
	return nil
}
func (s *stubProto) WriteUserMessageLocked(io.Writer, string, string, []ImageData, string) error {
	return nil
}
func (s *stubProto) SupportsPriority() bool                  { return s.priority }
func (s *stubProto) SupportsReplay() bool                    { return s.replay }
func (s *stubProto) WriteInterrupt(io.Writer, string) error  { return nil }
func (s *stubProto) ReadEvent(string) ([]Event, bool, error) { return nil, false, nil }
func (s *stubProto) HandleEvent(io.Writer, Event) bool       { return false }

// stubProtoWithCaps embeds stubProto and overrides with a direct Capabilities().
// When present, ProtocolCaps must prefer it over the SupportsX() fallback.
type stubProtoWithCaps struct {
	stubProto
	caps Caps
}

func (s *stubProtoWithCaps) Capabilities() Caps { return s.caps }

func TestProtocolCaps_DefaultDerivation(t *testing.T) {
	t.Parallel()
	p := &stubProto{name: "stream-json", replay: true, priority: true}
	got := ProtocolCaps(p)
	want := Caps{Replay: true, Priority: true, SoftInterrupt: false, StreamJSON: true}
	if got != want {
		t.Fatalf("default derivation: got %+v want %+v", got, want)
	}
	// Name=="acp" should flip StreamJSON off even without Capabilities().
	p2 := &stubProto{name: "acp", replay: false, priority: false}
	got2 := ProtocolCaps(p2)
	if got2.StreamJSON {
		t.Fatalf("acp name should derive StreamJSON=false, got %+v", got2)
	}
}

func TestProtocolCaps_ImplementationWins(t *testing.T) {
	t.Parallel()
	// SupportsX say false, but Capabilities() returns all-true: the method must win.
	p := &stubProtoWithCaps{
		stubProto: stubProto{name: "acp", replay: false, priority: false},
		caps:      Caps{Replay: true, Priority: true, SoftInterrupt: true, StreamJSON: true},
	}
	got := ProtocolCaps(p)
	if got != p.caps {
		t.Fatalf("implementation Capabilities() must win: got %+v want %+v", got, p.caps)
	}
}

func TestProtocolCaps_Claude(t *testing.T) {
	t.Parallel()
	got := ProtocolCaps(&ClaudeProtocol{})
	want := Caps{Replay: true, Priority: true, SoftInterrupt: false, StreamJSON: true}
	if got != want {
		t.Fatalf("claude caps: got %+v want %+v", got, want)
	}
}

func TestProtocolCaps_ACP(t *testing.T) {
	t.Parallel()
	got := ProtocolCaps(&ACPProtocol{})
	want := Caps{Replay: false, Priority: false, SoftInterrupt: true, StreamJSON: false}
	if got != want {
		t.Fatalf("acp caps: got %+v want %+v", got, want)
	}
}
