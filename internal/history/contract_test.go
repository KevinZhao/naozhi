package history

// Registry-guard test, moved with the registry out of internal/cli (#2649 G1-d).
// It asserts the KEY was not written, which needs the map itself: PickFactory("")
// short-circuits on the empty ID, so it cannot tell "rejected" from "never
// looked up". That is why this lives beside the implementation rather than
// calling through the exported surface.

import "testing"

// TestRegisterFactory_RejectsEmptyOrNil pins the registration
// guard so a buggy backend's init() that passes "" or nil cannot
// silently overwrite a real factory or seed an empty-key entry.
func TestRegisterFactory_RejectsEmptyOrNil(t *testing.T) {
	t.Parallel()
	// Empty backend ID must be ignored.
	RegisterFactory("", func(s SessionView, deps Wiring) Source { return nil })
	factoryMu.RLock()
	_, ok := factoryRegistry[""]
	factoryMu.RUnlock()
	if ok {
		t.Errorf("empty backend ID accepted; registry now has empty-key entry")
	}

	// Nil function must be ignored. Use a unique key so an earlier
	// (or concurrent) test cannot pollute the assertion.
	RegisterFactory("cli-test-nilfn-guard", nil)
	factoryMu.RLock()
	_, ok = factoryRegistry["cli-test-nilfn-guard"]
	factoryMu.RUnlock()
	if ok {
		t.Errorf("nil factory accepted; registry now has cli-test-nilfn-guard entry")
	}
}
