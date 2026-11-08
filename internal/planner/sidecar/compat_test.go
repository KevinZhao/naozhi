package sidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// loadFixture copies testdata/name into a fresh store directory under the
// file name its key gives it, and returns the directory and the fixture's
// fields as raw JSON.
func loadFixture(t *testing.T, name string) (dir, key string, raw map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw["key"], &key); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName(key)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, key, raw
}

// TestSidecarCompatV064: a sidecar as v0.6.4 wrote it (every field it had)
// reads in full, and an update by this build keeps every one of them, so a
// fields-only addition (Report) needs no version bump.
func TestSidecarCompatV064(t *testing.T) {
	dir, key, raw := loadFixture(t, "sidecar_v0.6.4.json")
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	want := Meta{
		V: 1, Key: key, Purpose: PurposeTask, Parent: "planner:main", Title: "修 index.json 的 last_active",
		Workspace: "w1", Dir: "/fixture/naozhi", Model: "opus",
		MCPTools:  map[string][]string{"aws-outlook-mcp": {"email_send"}},
		Ultracode: true,
		Created:   ts("2026-11-08T08:00:00Z"),
		Retired:   ts("2026-11-08T12:00:00Z"),
		Mark:      &Mark{State: "done", By: "planner", Why: "PR 已合并", At: ts("2026-11-08T11:00:00Z")},
		Failure:   &Mark{State: "failed", By: "health", Why: "death_reason=shim_eof", At: ts("2026-11-08T09:00:00Z")},
		Turn:      &Turn{Ended: ts("2026-11-08T10:59:00Z"), Check: true, From: ts("2026-11-08T10:00:00Z"), Text: "做完了"},
		Custody:   &Custody{On: true, By: "auto", At: ts("2026-11-08T08:00:00Z")},
		Chain: &Chain{Sent: 2, Idle: 1, Errors: map[string]int{"overloaded": 1}, Progress: "head=abc",
			Stopped: "no_progress", StoppedAt: ts("2026-11-08T10:30:00Z")},
		Retry: &Retry{Failed: ts("2026-11-08T09:00:00Z"), Class: "overloaded", At: ts("2026-11-08T09:01:00Z"),
			Cred: ts("2026-11-08T20:00:00Z"), Held: true, Done: true},
		Reap: &Reap{At: ts("2026-11-08T12:00:00Z"), By: "planner", Why: "PR 已合并",
			SessionID: "11111111-2222-3333-4444-555555555555", Workspace: "/fixture/naozhi", LastPrompt: "继续",
			Worktrees: []Worktree{{Dir: "/fixture/naozhi-wt", GitDir: "/fixture/naozhi/.git", Branch: "fix/x",
				State: "removed", Why: "已合并", At: ts("2026-11-08T12:00:00Z"), Notified: "merged"}}},
	}
	s := Open(dir, nil)
	got, ok := s.Get(key)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("v0.6.4 sidecar read as\n%+v\nwant\n%+v", got, want)
	}

	report := Report{State: "done", Summary: "已修好。", At: ts("2026-11-08T11:30:00Z")}
	if ok, err := s.Update(key, func(m *Meta) bool { m.Report = &report; return true }); !ok || err != nil {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName(key)))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	for k, v := range raw {
		if !jsonEqual(t, after[k], v) {
			t.Errorf("field %s after Update = %s, want %s", k, after[k], v)
		}
	}
	if len(after) != len(raw)+1 || after["report"] == nil {
		t.Errorf("fields after Update = %d (report %s), want the fixture's %d + report", len(after), after["report"], len(raw))
	}
	want.Report = &report
	if got, _ := Open(dir, nil).Get(key); !reflect.DeepEqual(got, want) {
		t.Fatalf("reread after Update = %+v", got)
	}
}

// TestTurnReportErrCompat: a turn as v0.6.4 wrote it reads with no
// ReportErr, and one with a ReportErr keeps every other field of the
// sidecar; "" is left out.
func TestTurnReportErrCompat(t *testing.T) {
	dir, key, raw := loadFixture(t, "sidecar_v0.6.4.json")
	s := Open(dir, nil)
	m, _ := s.Get(key)
	if m.Turn == nil || m.Turn.ReportErr != "" {
		t.Fatalf("turn = %+v", m.Turn)
	}
	turn := *m.Turn
	turn.ReportErr = "state 只能是 done、blocked、need_user 或 progress"
	if ok, err := s.Update(key, func(m *Meta) bool { m.Turn = &turn; return true }); !ok || err != nil {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName(key)))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	for k, v := range raw {
		if k != "turn" && !jsonEqual(t, after[k], v) {
			t.Errorf("field %s after Update = %s, want %s", k, after[k], v)
		}
	}
	if len(after) != len(raw) {
		t.Errorf("fields after Update = %d, want %d", len(after), len(raw))
	}
	if got, _ := Open(dir, nil).Get(key); got.Turn == nil || *got.Turn != turn {
		t.Errorf("reread turn = %+v, want %+v", got.Turn, turn)
	}
	turn.ReportErr = ""
	if b, _ := json.Marshal(turn); strings.Contains(string(b), "report_err") {
		t.Errorf("an empty ReportErr is written: %s", b)
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// TestSidecarSeenAtCompat: seen_at is a fields-only addition too. A
// v0.6.4 sidecar reads with no SeenAt; setting it keeps every field the
// fixture had and adds only seen_at, which a reread returns.
func TestSidecarSeenAtCompat(t *testing.T) {
	dir, key, raw := loadFixture(t, "sidecar_v0.6.4.json")
	s := Open(dir, nil)
	before, ok := s.Get(key)
	if !ok || !before.SeenAt.IsZero() {
		t.Fatalf("v0.6.4 sidecar: ok %v, SeenAt %v; want it read with no SeenAt", ok, before.SeenAt)
	}
	seen := time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)
	if ok, err := s.Update(key, func(m *Meta) bool { m.SeenAt = seen; return true }); !ok || err != nil {
		t.Fatalf("Update = %v, %v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName(key)))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	for k, v := range raw {
		if !jsonEqual(t, after[k], v) {
			t.Errorf("field %s after Update = %s, want %s", k, after[k], v)
		}
	}
	if len(after) != len(raw)+1 || string(after["seen_at"]) != `"2026-11-08T13:00:00Z"` {
		t.Errorf("fields after Update = %d (seen_at %s), want the fixture's %d + seen_at", len(after), after["seen_at"], len(raw))
	}
	want := before
	want.SeenAt = seen
	if got, _ := Open(dir, nil).Get(key); !reflect.DeepEqual(got, want) {
		t.Fatalf("reread after Update = %+v", got)
	}
}

// TestSidecarWatchCompat: watch and result_at are fields-only additions. A
// sidecar custody gave a session the user opened (purpose "", v0.6.4)
// reads with neither; setting them keeps every field it had and adds only
// those two, which a reread returns. Upsert on it changes it in place.
func TestSidecarWatchCompat(t *testing.T) {
	dir, key, raw := loadFixture(t, "sidecar_v0.6.4_chat.json")
	s := Open(dir, nil)
	before, ok := s.Get(key)
	if !ok || before.Watch != nil || !before.ResultAt.IsZero() || before.Custody == nil || !before.Custody.On {
		t.Fatalf("v0.6.4 chat sidecar read as %+v (%v)", before, ok)
	}
	at := time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)
	if err := s.Upsert(key, func(m *Meta) bool {
		m.Watch = &Custody{On: false, By: "user", At: at}
		m.ResultAt = at
		return true
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName(key)))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	for k, v := range raw {
		if !jsonEqual(t, after[k], v) {
			t.Errorf("field %s after Upsert = %s, want %s", k, after[k], v)
		}
	}
	if len(after) != len(raw)+2 || string(after["result_at"]) != `"2026-11-08T13:00:00Z"` ||
		!jsonEqual(t, after["watch"], json.RawMessage(`{"on":false,"by":"user","at":"2026-11-08T13:00:00Z"}`)) {
		t.Errorf("fields after Upsert = %d (watch %s, result_at %s), want the fixture's %d + watch + result_at",
			len(after), after["watch"], after["result_at"], len(raw))
	}
	want := before
	want.Watch, want.ResultAt = &Custody{On: false, By: "user", At: at}, at
	if got, _ := Open(dir, nil).Get(key); !reflect.DeepEqual(got, want) {
		t.Fatalf("reread after Upsert = %+v", got)
	}
}

// Upsert gives a session with no sidecar one only when fn changes
// something: purpose "", stamped; it refuses a newer naozhi's file.
func TestUpsert(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	s := Open(dir, func() time.Time { return now })
	const key = "feishu:direct:u:general"
	if err := s.Upsert(key, func(*Meta) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(key); ok {
		t.Fatal("an Upsert changing nothing created a sidecar")
	}
	if _, err := os.Stat(filepath.Join(dir, fileName(key))); !os.IsNotExist(err) {
		t.Fatalf("file written: %v", err)
	}
	if err := s.Upsert(key, func(m *Meta) bool { m.SeenAt = now; return true }); err != nil {
		t.Fatal(err)
	}
	m, ok := Open(dir, nil).Get(key)
	if !ok || m.V != Version || m.Purpose != "" || !m.Created.Equal(now) || !m.SeenAt.Equal(now) {
		t.Fatalf("reread = %+v (%v)", m, ok)
	}
	if err := s.Upsert("", func(*Meta) bool { return true }); err == nil {
		t.Error("Upsert with no key")
	}
	const newer = "slack:direct:n:general"
	if err := os.WriteFile(filepath.Join(dir, fileName(newer)), []byte(`{"v":99,"key":"`+newer+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(dir, nil)
	if err := s.Upsert(newer, func(m *Meta) bool { m.SeenAt = now; return true }); err != ErrNewer {
		t.Errorf("Upsert on a newer sidecar = %v, want ErrNewer", err)
	}
}
