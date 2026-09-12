package datadir_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestSweeperOnARealisticDataDir mirrors what was measured on a live instance
// before J6: cli-debug/ with old per-session logs, shims/ with a five-month
// backlog of dead-pid logs plus the live one and the state JSON, sys-sessions/
// with old JSONLs. It asserts the whole registered set at once, because the risk
// is not one pass misbehaving — it is a pass reaching a neighbour's files.
func TestSweeperOnARealisticDataDir(t *testing.T) {
	root := t.TempDir()
	cliDebug := filepath.Join(root, "cli-debug")
	shims := filepath.Join(root, "shims")
	sysSessions := filepath.Join(root, "sys-sessions")
	for _, d := range []string{cliDebug, shims, sysSessions} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	age := func(dir, name string, d time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-d)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}

	sixWeeks, fiveMonths := 42*24*time.Hour, 150*24*time.Hour
	goneDebug := age(cliDebug, "aaaaaaaaaaaaaaaa.log", sixWeeks)
	liveDebug := age(cliDebug, "bbbbbbbbbbbbbbbb.log", time.Minute)
	deadShim := age(shims, shim.LogFilePrefix+"999999999.log", fiveMonths)
	ownShim := age(shims, shim.LogFilePrefix+pid()+".log", fiveMonths)
	shimState := age(shims, "a5ec6901ebba9c34.json", fiveMonths)
	freshDeadShim := age(shims, shim.LogFilePrefix+"999999998.log", time.Minute)
	oldJSONL := age(sysSessions, "old.jsonl", 30*24*time.Hour)
	freshJSONL := age(sysSessions, "new.jsonl", time.Minute)

	s := datadir.NewSweeper(0)
	s.Add(datadir.Pass{Name: "cli-debug", Dir: cliDebug, Ext: ".log", MaxAge: 7 * 24 * time.Hour})
	s.Add(datadir.Pass{Name: "shim-logs", Dir: shims, Ext: ".log", MaxAge: 24 * time.Hour, Keep: shim.LogFileIsLive})
	s.Add(datadir.Pass{Name: "sys-sessions", Dir: sysSessions, Ext: ".jsonl", MaxAge: 7 * 24 * time.Hour})
	got := s.RunOnce()

	for _, c := range []struct {
		path string
		want bool
		why  string
	}{
		{goneDebug, false, "six-week-old cli-debug log, no live CLI survives that"},
		{liveDebug, true, "fresh cli-debug log of a live session"},
		{deadShim, false, "dead pid, five months old"},
		{ownShim, true, "this process's own pid is alive"},
		{shimState, true, "the shim state JSON is not a .log"},
		{freshDeadShim, true, "dead pid but inside the 24h diagnosis grace"},
		{oldJSONL, false, "past the sys-sessions window"},
		{freshJSONL, true, "inside the sys-sessions window"},
	} {
		_, err := os.Stat(c.path)
		if (err == nil) != c.want {
			t.Errorf("%s: exists=%v, want %v (%s)", filepath.Base(c.path), err == nil, c.want, c.why)
		}
	}
	if got["cli-debug"].Removed != 1 || got["shim-logs"].Removed != 1 || got["sys-sessions"].Removed != 1 {
		t.Errorf("per-pass counts = %+v, want exactly one each", got)
	}
}

func pid() string {
	n := os.Getpid()
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
