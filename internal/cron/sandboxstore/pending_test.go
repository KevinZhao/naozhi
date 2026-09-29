package sandboxstore

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/textutil"
)

func TestPending_WriteReadListRemove(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	p := Pending{JobID: hexID(t), RunID: hexID(t), RuntimeSessionID: "run-ab-1", StartedAtMS: 42}
	path := st.WritePending(p, slog.Default())
	if path == "" {
		t.Fatal("WritePending returned no path")
	}
	if got, state := st.ReadPending(path); state != PendingOK || got != p {
		t.Fatalf("ReadPending = (%+v, %v), want the record back", got, state)
	}
	entries, err := st.ListPending()
	if err != nil || len(entries) != 1 || entries[0].Rec != p || entries[0].Path != path {
		t.Fatalf("ListPending = %+v, %v", entries, err)
	}
	if err := st.RemovePending(path); err != nil {
		t.Fatal(err)
	}
	if _, state := st.ReadPending(path); state != PendingGone {
		t.Errorf("after remove: %v, want PendingGone", state)
	}
	if err := st.RemovePending(path); err != nil {
		t.Errorf("removing an already-removed record: %v", err)
	}
	if st.WritePending(Pending{RunID: "../x"}, slog.Default()) != "" {
		t.Error("a non-hex run id was written")
	}
}

// Records that cannot be trusted are reported, not followed: a corrupt one as
// such, an oversized or symlinked one as unreadable, and a path outside the
// pending directory is neither read nor removed.
func TestPending_UntrustedRecords(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	dir := st.pendingDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte(`{"job_id":"ab","run_id":"cd","runtime_session_id":"r","started_at_ms":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	corrupt := write("aa.json", "{not json")
	big := write("bb.json", `{"job_id":"`+strings.Repeat("a", maxPendingRecordBytes)+`"}`)
	link := filepath.Join(dir, "cc.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for path, want := range map[string]PendingState{corrupt: PendingCorrupt, big: PendingUnreadable, link: PendingUnreadable, outside: PendingUnreadable} {
		if _, got := st.ReadPending(path); got != want {
			t.Errorf("ReadPending(%s) = %v, want %v", filepath.Base(path), got, want)
		}
	}
	entries, err := st.ListPending()
	if err != nil || len(entries) != 3 {
		t.Fatalf("ListPending = %+v, %v; want the three untrusted records listed", entries, err)
	}
	if err := st.RemovePending(outside); err == nil {
		t.Error("RemovePending removed a path outside the pending directory")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the outside file is gone: %v", err)
	}
}

// A record the bounded read refuses still blocks replay (GetAttention fails
// on it), so the queue lists it for the operator to clear.
func TestListAttention_ListsRecordsTheReadRefuses(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	dir := st.attentionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bigRun, linkRun, linkJob := hexID(t), hexID(t), hexID(t)
	if err := os.WriteFile(filepath.Join(dir, bigRun+".json"), []byte(`{"pad":"`+strings.Repeat("x", maxAttentionRecordBytes)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "t.json")
	if err := os.WriteFile(target, []byte(`{"job_id":"`+linkJob+`","run_id":"`+linkRun+`","reason":"transport"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, linkRun+".json")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	items := st.ListAttention()
	if len(items) != 2 || st.AttentionCount() != 2 {
		t.Fatalf("items = %+v (count %d), want both records listed", items, st.AttentionCount())
	}
	for _, it := range items {
		if !it.Unreadable || it.JobID != "" {
			t.Errorf("item %+v: want Unreadable with no job", it)
		}
	}
	// Deleting by job never follows the symlink to read the job it names.
	st.DeleteJobAttention(linkJob)
	if _, err := os.Lstat(filepath.Join(dir, linkRun+".json")); err != nil {
		t.Errorf("DeleteJobAttention removed a record it could not read: %v", err)
	}
}

func TestSnapshotPrompt_RefusesWhatIsNotTheBlob(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	blobs := filepath.Join(st.snapshotDir(), "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	hashOf := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	good := hashOf("the prompt")
	if err := os.WriteFile(filepath.Join(blobs, good), []byte("the prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SnapshotPrompt(good); err != nil || got != "the prompt" {
		t.Fatalf("SnapshotPrompt = (%q, %v)", got, err)
	}
	swapped := hashOf("other")
	if err := os.WriteFile(filepath.Join(blobs, swapped), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SnapshotPrompt(swapped); err == nil {
		t.Error("a blob whose bytes do not hash to its name was returned")
	}
	target := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(target, []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := hashOf("linked")
	if err := os.Symlink(target, filepath.Join(blobs, linked)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := st.SnapshotPrompt(linked); err == nil {
		t.Error("a symlinked blob was followed")
	}
}

func TestSnapshotPrompt_RefusesAnOversizedBlob(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	blobs := filepath.Join(st.snapshotDir(), "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("p", textutil.MaxCronPromptBytes+1)
	sum := sha256.Sum256([]byte(big))
	name := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(blobs, name), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SnapshotPrompt(name); err == nil {
		t.Error("a blob over the prompt cap was read")
	}
}
