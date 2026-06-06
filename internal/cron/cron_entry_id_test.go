package cron

import (
	"testing"

	robfigcron "github.com/robfig/cron/v3"
)

// TestCronEntryIDAlias pins R249-ARCH-11 (#977): cronEntryID is a transparent
// alias of robfigcron.EntryID, so a value of one type is assignable to the
// other without conversion. If a future change turns the alias into a defined
// type (type cronEntryID robfigcron.EntryID) this stops compiling, flagging
// that the pool type-assertions and s.cron.Remove/Entry call sites now need
// explicit conversions.
func TestCronEntryIDAlias(t *testing.T) {
	var local cronEntryID = 42
	var ext robfigcron.EntryID = local // assignable both ways == alias
	local = ext
	if int(local) != 42 || int(ext) != 42 {
		t.Fatalf("alias round-trip drifted: local=%d ext=%d", local, ext)
	}

	// The list pools store the aliased element type; assert a Job's entryID
	// flows through cronEntryGoneLocked's parameter without conversion.
	j := &Job{entryID: local}
	var _ cronEntryID = j.entryID
}
