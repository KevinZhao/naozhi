package cli

import (
	"os"
	"regexp"
	"testing"
)

// TestACPProtocol_AllocID_64BitContractDocumented locks R185-GO-L1: the
// narrowing from atomic.Int64 → int inside allocID is a platform-coupled
// contract. On 64-bit targets the conversion is lossless; on 32-bit it
// silently truncates and collides ids, which would corrupt the JSON-RPC
// request/response matching. Because naozhi only ships 64-bit builds we
// document the contract at the call site rather than adding a runtime
// guard. This test locks the documentation so a future refactor that
// removes the comment (or changes the nextID type to atomic.Uint64 etc.)
// fails CI and forces a conscious re-review.
func TestACPProtocol_AllocID_64BitContractDocumented(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("protocol_acp.go")
	if err != nil {
		t.Fatalf("read protocol_acp.go: %v", err)
	}
	src := string(data)

	// Anchor 1: the R185-GO-L1 anchor must appear in the godoc immediately
	// preceding allocID. Matching against a multi-line window tolerates
	// gofmt reflows of the comment block.
	pat1 := regexp.MustCompile(`(?s)//[^\n]*R185-GO-L1[^\n]*\n(?://[^\n]*\n){2,}func \(p \*ACPProtocol\) allocID\(\) int \{`)
	if !pat1.MatchString(src) {
		t.Error("allocID must have R185-GO-L1 godoc immediately above it")
	}

	// Anchor 2: the godoc must mention 64-bit so a reader quickly sees
	// the platform assumption. "64-bit" occurs nowhere else in the file;
	// the test is specific enough to catch accidental deletion.
	if !regexp.MustCompile(`64-bit`).MatchString(src) {
		t.Error("allocID godoc must mention 64-bit platform assumption")
	}

	// Anchor 3: the godoc must warn about 32-bit truncation explicitly so
	// no one reads the comment and concludes "but naozhi doesn't support
	// 32-bit so this is fine" without seeing the specific failure mode.
	if !regexp.MustCompile(`32-bit`).MatchString(src) {
		t.Error("allocID godoc must call out 32-bit truncation footgun")
	}

	// Anchor 4: nextID is still atomic.Int64 (not Uint64/int). The type
	// choice is load-bearing for the narrowing to compile and for the
	// sign-flip argument in the comment to hold.
	if !regexp.MustCompile(`nextID\s+atomic\.Int64`).MatchString(src) {
		t.Error("nextID must remain atomic.Int64 for R185-GO-L1 contract")
	}
}
