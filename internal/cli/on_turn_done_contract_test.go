package cli

import (
	"os"
	"strings"
	"testing"
)

// TestOnTurnDoneIdempotencyContract locks the godoc that documents the
// idempotency contract for the onTurnDone callback (R183-CONCUR-M1).
//
// readLoop fans cb() from several arms:
//   - result + reconnectedMidTurn CAS (line ~647)
//   - killCh select immediately after (line ~660)
//   - cli_exited (line ~715)
//   - fall-out StateDead at function tail (line ~752)
//   - panic-recover defer (line ~485)
//
// The first two arms can fire cb() back-to-back inside a single readLoop
// iteration when Kill() races a mid-turn reconnect finish. Current session-layer
// callbacks (router.notifyChange / Send's NotifyIdle) are already idempotent
// because notifyChange loads an onChange atomic.Pointer and NotifyIdle calls
// sync.Cond.Broadcast — both are idempotent by construction. But this is a
// contract that future callbacks must honour, so we document it in the godoc
// and lock the documentation with this test so refactors that silently drop
// the contract break CI.
func TestOnTurnDoneIdempotencyContract(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("process.go")
	if err != nil {
		t.Fatalf("read process.go: %v", err)
	}
	body := string(src)

	// The onTurnDone field godoc must mention the idempotency contract.
	// We deliberately anchor on the exact R-number because that is how the
	// wider codebase references this constraint, and substring matching on
	// a common word like "idempotent" alone would silently pass if the
	// anchor comment were moved elsewhere in the file.
	fieldIdx := strings.Index(body, "onTurnDone func()")
	if fieldIdx < 0 {
		t.Fatal("onTurnDone field declaration not found — has the process struct been refactored? Update this test.")
	}
	// Look at the ~1.5 KB preceding the field for the godoc block.
	windowStart := fieldIdx - 1500
	if windowStart < 0 {
		windowStart = 0
	}
	fieldWindow := body[windowStart:fieldIdx]
	if !strings.Contains(fieldWindow, "R183-CONCUR-M1") {
		t.Errorf("onTurnDone field godoc must reference R183-CONCUR-M1 anchor; the idempotency contract documentation was removed.")
	}
	if !strings.Contains(fieldWindow, "idempotent") {
		t.Errorf("onTurnDone field godoc must document that callbacks must be idempotent; the word `idempotent` is missing from the ~1.5 KB preceding the field declaration.")
	}

	// The SetOnTurnDone setter godoc must echo the idempotency requirement
	// so users of the public API see it without reading the field.
	setterIdx := strings.Index(body, "func (p *Process) SetOnTurnDone(")
	if setterIdx < 0 {
		t.Fatal("SetOnTurnDone setter not found — has the public API been refactored? Update this test.")
	}
	setterStart := setterIdx - 1000
	if setterStart < 0 {
		setterStart = 0
	}
	setterWindow := body[setterStart:setterIdx]
	if !strings.Contains(setterWindow, "idempotent") {
		t.Errorf("SetOnTurnDone setter godoc must document the idempotency requirement; the word `idempotent` is missing from the setter's preceding godoc block.")
	}

	// The result + reconnectedMidTurn CAS path must carry an inline comment
	// that flags the possibility of a second cb() invocation right after,
	// so readers refactoring that path do not assume at-most-once semantics.
	casIdx := strings.Index(body, "reconnectedMidTurn.CompareAndSwap(true, false)")
	if casIdx < 0 {
		t.Fatal("reconnectedMidTurn CAS site not found — has readLoop been refactored? Update this test.")
	}
	// Scan the ~600 bytes following the CAS line for the R183 anchor.
	casEnd := casIdx + 600
	if casEnd > len(body) {
		casEnd = len(body)
	}
	casWindow := body[casIdx:casEnd]
	if !strings.Contains(casWindow, "R183-CONCUR-M1") {
		t.Errorf("the reconnectedMidTurn CAS cb() call site must carry the R183-CONCUR-M1 inline anchor warning about the back-to-back killCh invocation; the anchor is missing.")
	}
}
