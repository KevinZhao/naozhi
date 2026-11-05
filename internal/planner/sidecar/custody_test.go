package sidecar

import (
	"reflect"
	"testing"
	"time"
)

// Custody's fields are written through Update and read back after a
// restart; Update leaves a key with no sidecar alone.
func TestUpdateCustodyFields(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	s := Open(dir, nil)
	if err := s.Put(Meta{Key: "dashboard:direct:u:general"}); err != nil {
		t.Fatal(err)
	}
	want := Meta{
		Custody: &Custody{On: true, By: "user", At: at},
		Chain:   &Chain{Sent: 2, Idle: 1, Errors: map[string]int{"network": 1}, Progress: "head=abc", Stopped: "连续 3 次没有进展", StoppedAt: at},
		Retry:   &Retry{Failed: at, Class: "network", At: at.Add(time.Minute)},
	}
	ok, err := s.Update("dashboard:direct:u:general", func(m *Meta) bool {
		m.Custody, m.Chain, m.Retry = want.Custody, want.Chain, want.Retry
		return true
	})
	if !ok || err != nil {
		t.Fatalf("Update = %v %v", ok, err)
	}
	if ok, err := s.Update("nobody", func(*Meta) bool { return true }); ok || err != nil {
		t.Errorf("Update of no sidecar = %v %v", ok, err)
	}
	got, _ := Open(dir, nil).Get("dashboard:direct:u:general")
	if !reflect.DeepEqual(got.Custody, want.Custody) || !reflect.DeepEqual(got.Chain, want.Chain) || !reflect.DeepEqual(got.Retry, want.Retry) {
		t.Errorf("after reopen: %+v %+v %+v", got.Custody, got.Chain, got.Retry)
	}
}
