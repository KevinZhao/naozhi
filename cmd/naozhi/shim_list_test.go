package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

func shimEntry(v shim.StateVerdict, key string, pid int) shim.StateEntry {
	return shim.StateEntry{
		Path:    "/state/" + key + ".json",
		Verdict: v,
		State:   shim.State{Key: key, ShimPID: pid, CLIPID: pid + 1, CLIAlive: true, SessionID: "sess_0123456789abcdef"},
	}
}

func TestWriteShimList_StatusAndStaleFooter(t *testing.T) {
	noSID := shimEntry(shim.StateLive, "feishu:d:carol:general", 300)
	noSID.State.SessionID, noSID.State.CLIAlive = "", false
	unverified := shimEntry(shim.StateLive, "feishu:d:dave:general", 400)
	unverified.IdentityErr = errors.New("ps failed")
	entries := []shim.StateEntry{
		shimEntry(shim.StateLive, "feishu:d:alice:general", 100),
		shimEntry(shim.StateBinaryMismatch, "feishu:d:bob:general", 200),
		noSID,
		unverified,
		shimEntry(shim.StateSocketMissing, "feishu:d:erin:general", 500),
		shimEntry(shim.StateDeadPID, "feishu:d:frank:general", 600),
		{Path: "/state/corrupt.json", Verdict: shim.StateCorrupt, Err: errors.New("bad json")},
	}
	var b strings.Builder
	writeShimList(&b, entries, nil)
	want := `SHIM   CLI    ALIVE KEY                                      SESSION         STATUS
100    101    yes   feishu:d:alice:general                   sess_0123456... ok
200    201    yes   feishu:d:bob:general                     sess_0123456... foreign-bin
300    301    no    feishu:d:carol:general                   -               ok
400    401    yes   feishu:d:dave:general                    sess_0123456... unverified
500    501    yes   feishu:d:erin:general                    sess_0123456... no-socket

5 shim(s)
foreign-bin: started by a different naozhi binary than this one; run list/stop with the service's binary
2 stale state file(s) left for the service's reconcile to clean
`
	if got := b.String(); got != want {
		t.Errorf("writeShimList output:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteShimList_OnlyStale(t *testing.T) {
	var b strings.Builder
	writeShimList(&b, []shim.StateEntry{shimEntry(shim.StateDeadPID, "k", 1)}, nil)
	want := "no active shims\n1 stale state file(s) left for the service's reconcile to clean\n"
	if got := b.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestPlanShimStop: only targets whose PID is alive and runs this binary are
// signalled; an unconfirmed identity is skipped (it may be a reused PID), and
// non-targets never appear in the plan at all.
func TestPlanShimStop(t *testing.T) {
	unverified := shimEntry(shim.StateLive, "unverified", 400)
	unverified.IdentityErr = errors.New("ps failed")
	entries := []shim.StateEntry{
		shimEntry(shim.StateLive, "live", 100),
		shimEntry(shim.StateBinaryMismatch, "foreign", 200),
		shimEntry(shim.StateSocketMissing, "nosock", 300),
		unverified,
		shimEntry(shim.StateDeadPID, "dead", 500),
		{Path: "/state/corrupt.json", Verdict: shim.StateCorrupt},
	}
	keys := func(states []shim.State) (out []string) {
		for _, s := range states {
			out = append(out, s.Key)
		}
		return out
	}
	skippedKeys := func(es []shim.StateEntry) (out []string) {
		for _, e := range es {
			out = append(out, e.State.Key)
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		key           string
		all           bool
		targets, skip string
		stale         int
	}{
		{"all", "", true, "live,nosock", "foreign,unverified", 2},
		{"live key", "live", false, "live", "", 0},
		{"foreign key", "foreign", false, "", "foreign", 0},
		{"unverified key", "unverified", false, "", "unverified", 0},
		{"dead key", "dead", false, "", "", 1},
		{"unknown key", "nope", false, "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planShimStop(entries, tc.key, tc.all)
			if got := strings.Join(keys(p.targets), ","); got != tc.targets {
				t.Errorf("targets = %q, want %q", got, tc.targets)
			}
			if got := strings.Join(skippedKeys(p.skipped), ","); got != tc.skip {
				t.Errorf("skipped = %q, want %q", got, tc.skip)
			}
			if p.stale != tc.stale {
				t.Errorf("stale = %d, want %d", p.stale, tc.stale)
			}
		})
	}
}
