package platform

import (
	"context"
	"net/http"
	"testing"
)

// fakePlat is a Platform stub used to exercise AsCapability. It only
// satisfies what is required: the Platform method set, with optional
// capability methods added on test-specific subtypes.
type fakePlat struct{}

func (fakePlat) Name() string                                               { return "fake" }
func (fakePlat) RegisterRoutes(_ *http.ServeMux, _ MessageHandler)          {}
func (fakePlat) Reply(_ context.Context, _ OutgoingMessage) (string, error) { return "", nil }
func (fakePlat) EditMessage(_ context.Context, _ string, _ string) error    { return nil }
func (fakePlat) MaxReplyLength() int                                        { return DefaultMaxReplyLen }

// reactorPlat additionally satisfies Reactor.
type reactorPlat struct{ fakePlat }

func (reactorPlat) AddReaction(_ context.Context, _ string, _ ReactionType) error    { return nil }
func (reactorPlat) RemoveReaction(_ context.Context, _ string, _ ReactionType) error { return nil }

// TestAsCapability_ReactorDiscriminator verifies the generic AsCapability
// discriminator detects the Reactor capability (and only it) — the single
// capability-extension path after the per-capability AsReactor /
// AsQuestionCardSender wrappers were removed. R214-ARCH-2 (#402) / R239-ARCH-H.
func TestAsCapability_ReactorDiscriminator(t *testing.T) {
	var pYes Platform = reactorPlat{}
	var pNo Platform = fakePlat{}

	rGen, okGen := AsCapability[Reactor](pYes)
	if !okGen {
		t.Fatal("AsCapability[Reactor] returned ok=false on a Reactor-capable platform")
	}
	if rGen == nil {
		t.Error("AsCapability[Reactor] returned a nil reactor on a capable platform")
	}

	if _, ok := AsCapability[Reactor](pNo); ok {
		t.Error("AsCapability[Reactor] returned ok=true on a non-Reactor platform")
	}
}

// interimPlat additionally satisfies InterimMessageCapable.
type interimPlat struct{ fakePlat }

func (interimPlat) SupportsInterimMessages() bool { return true }

// TestSupportsInterimMessages_UnifiedDiscriminator pins R214-ARCH-2 (#402):
// SupportsInterimMessages now routes through the same AsCapability[T]
// discriminator and the named InterimMessageCapable interface as the other
// optional capabilities, rather than an inline anonymous type-assert.
func TestSupportsInterimMessages_UnifiedDiscriminator(t *testing.T) {
	var capable Platform = interimPlat{}
	var plain Platform = fakePlat{}

	if !SupportsInterimMessages(capable) {
		t.Error("SupportsInterimMessages = false on a capable platform")
	}
	if SupportsInterimMessages(plain) {
		t.Error("SupportsInterimMessages = true on a platform without the capability")
	}

	// The helper and the generic discriminator must agree on detection.
	if _, ok := AsCapability[InterimMessageCapable](capable); !ok {
		t.Error("AsCapability[InterimMessageCapable] returned ok=false on a capable platform")
	}
	if _, ok := AsCapability[InterimMessageCapable](plain); ok {
		t.Error("AsCapability[InterimMessageCapable] returned ok=true on a plain platform")
	}
}

// TestAsCapability_NewCapabilityNoHelperNeeded asserts that adding a
// brand-new capability interface does NOT require a corresponding AsX
// helper — AsCapability[T] suffices. Pin via a locally-declared
// interface so future regressions in the generic path surface here.
func TestAsCapability_NewCapabilityNoHelperNeeded(t *testing.T) {
	type pingable interface {
		Platform
		Ping() string
	}

	if _, ok := AsCapability[pingable](fakePlat{}); ok {
		t.Error("non-pingable platform should not satisfy pingable")
	}
}
