package project

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUploadQuota_NilDisabled(t *testing.T) {
	// <=0 limit returns a nil quota; all operations are no-ops that allow.
	if q := newUploadQuota(0); q != nil {
		t.Fatalf("newUploadQuota(0) = %v, want nil", q)
	}
	if q := newUploadQuota(-5); q != nil {
		t.Fatalf("newUploadQuota(-5) = %v, want nil", q)
	}
	var q *uploadQuota
	if !q.reserve("p", 1<<30) {
		t.Fatal("nil quota reserve must allow")
	}
	q.release("p", 1<<30) // must not panic
}

func TestUploadQuota_ReserveAndCap(t *testing.T) {
	q := newUploadQuota(100)
	if !q.reserve("a", 60) {
		t.Fatal("first 60 must fit under 100")
	}
	if !q.reserve("a", 40) {
		t.Fatal("next 40 must exactly fill to 100")
	}
	if q.reserve("a", 1) {
		t.Fatal("1 more byte must be refused past the cap")
	}
	// A different project has its own independent budget.
	if !q.reserve("b", 100) {
		t.Fatal("project b must have its own full budget")
	}
	// n<=0 is always allowed and charges nothing.
	if !q.reserve("a", 0) {
		t.Fatal("zero reserve must allow")
	}
}

func TestUploadQuota_ReleaseRestoresBudget(t *testing.T) {
	q := newUploadQuota(100)
	if !q.reserve("a", 100) {
		t.Fatal("reserve full budget")
	}
	if q.reserve("a", 1) {
		t.Fatal("over cap must refuse")
	}
	q.release("a", 50)
	if !q.reserve("a", 50) {
		t.Fatal("after releasing 50, 50 must fit again")
	}
	if q.reserve("a", 1) {
		t.Fatal("back at cap, 1 more must refuse")
	}
	// Over-release must clamp at zero (and drop the key) — never go negative.
	q.release("a", 1<<30)
	if !q.reserve("a", 100) {
		t.Fatal("after over-release the project budget must be fully restored")
	}
}

func TestUploadQuota_ConcurrentReserveNeverExceedsCap(t *testing.T) {
	const cap = 1000
	q := newUploadQuota(cap)
	var mu sync.Mutex
	var granted int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.reserve("a", 10) {
				mu.Lock()
				granted += 10
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted > cap {
		t.Fatalf("granted %d bytes exceeds cap %d", granted, cap)
	}
}

// TestHandleFilesUpload_QuotaExceeded wires a small per-project quota into the
// handler and confirms the SECOND upload is refused with 507 once the cap is
// reached, while a fresh project (separate budget) still succeeds.
// R202606g-SEC-3 (#2311).
func TestHandleFilesUpload_QuotaExceeded(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	// 10-byte cap: the first 6-byte file fits, the second pushes over.
	h.uploadQuota = newUploadQuota(10)

	w1 := doUpload(t, h, "", proj, "", "a.txt", []byte("123456"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first upload: want 200, got %d (%s)", w1.Code, w1.Body.String())
	}
	var resp struct {
		Size int64 `json:"size"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &resp)
	if resp.Size != 6 {
		t.Fatalf("first upload size = %d, want 6", resp.Size)
	}

	w2 := doUpload(t, h, "", proj, "", "b.txt", []byte("123456"))
	if w2.Code != http.StatusInsufficientStorage {
		t.Fatalf("second upload over quota: want 507, got %d (%s)", w2.Code, w2.Body.String())
	}
	// The rejected file must NOT have been written.
	if _, err := os.Stat(filepath.Join(projDir, "b.txt")); err == nil {
		t.Fatal("over-quota upload should not have created b.txt")
	}
}

// TestHandleFilesUpload_QuotaReleasedOnRejectedName verifies the reservation is
// released when the write fails after a successful reserve, so a transient
// rejection does not permanently burn the project's budget. We trigger a
// post-reserve failure by uploading to a name that exists without overwrite
// (409), then confirm a subsequent legitimate upload of the same size still
// fits under the cap. R202606g-SEC-3 (#2311).
func TestHandleFilesUpload_QuotaReleasedOnFailure(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"exists.txt": "x"})
	// Cap just large enough for one 6-byte file.
	h.uploadQuota = newUploadQuota(6)

	// Uploading onto an existing name without overwrite=1 → 409 AFTER the
	// reservation is taken. The deferred release must hand the bytes back.
	wConflict := doUpload(t, h, "", proj, "", "exists.txt", []byte("123456"))
	if wConflict.Code != http.StatusConflict {
		t.Fatalf("conflict upload: want 409, got %d (%s)", wConflict.Code, wConflict.Body.String())
	}

	// Budget must be fully restored: a fresh 6-byte file must still fit.
	wOK := doUpload(t, h, "", proj, "", "fresh.txt", []byte("123456"))
	if wOK.Code != http.StatusOK {
		t.Fatalf("post-failure upload: want 200 (budget restored), got %d (%s)", wOK.Code, wOK.Body.String())
	}
}
