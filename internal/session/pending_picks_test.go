package session

import (
	"reflect"
	"testing"
)

// The failure mode pendingPicks exists to prevent: a per-session pick that some
// key-manipulating path forgets. RenameSession used to move three maps by hand
// and terminal removal deleted three separately, so a fourth pick added later
// would be maintained in some paths and not others — and the stale entry then
// gets consumed by whatever session next holds that key.
//
// These tests walk the struct by REFLECTION rather than naming the three maps, so
// adding a field without teaching renameLocked/dropAllLocked about it fails here
// instead of silently leaking.

// mapFields returns the name and reflect.Value of every map field on p.
func mapFields(t *testing.T, p *pendingPicks) map[string]reflect.Value {
	t.Helper()
	v := reflect.ValueOf(p).Elem()
	out := map[string]reflect.Value{}
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Map {
			out[v.Type().Field(i).Name] = v.Field(i)
		}
	}
	if len(out) == 0 {
		t.Fatal("pendingPicks has no map fields; this scan would pass vacuously")
	}
	return out
}

// seedAllPicks puts a distinguishable entry under key in every map. Hand-written
// because reflection cannot WRITE through an unexported field — it can only read.
// TestPendingPicks_SeederCoversEveryMap asserts this function covers every map
// field, so a field added later forces an update here rather than quietly
// escaping the tests below.
func seedAllPicks(p *pendingPicks, key string) int {
	p.backend[key] = "seeded-backend"
	p.accessProfile[key] = "seeded-profile"
	p.tuning[key] = pendingTuning{Model: "seeded-model"}
	return 3
}

// TestPendingPicks_SeederCoversEveryMap is the tripwire that makes the two sweeps
// below trustworthy: if pendingPicks grows a map that seedAllPicks does not fill,
// those sweeps would pass while the new pick goes unmaintained.
func TestPendingPicks_SeederCoversEveryMap(t *testing.T) {
	var p pendingPicks
	p.initLocked()
	seeded := seedAllPicks(&p, "k")
	if got := len(mapFields(t, &p)); got != seeded {
		t.Fatalf("pendingPicks has %d map fields but seedAllPicks fills %d. Add the new field "+
			"to seedAllPicks — and to renameLocked / dropAllLocked, which is what the sweeps "+
			"below then verify.", got, seeded)
	}
}

// TestPendingPicks_RenameMovesEveryMap: renameLocked must move every pick, not
// the three that happened to exist when it was written.
func TestPendingPicks_RenameMovesEveryMap(t *testing.T) {
	var p pendingPicks
	p.initLocked()
	const oldKey, newKey = "chat:old:general", "chat:new:general"
	seedAllPicks(&p, oldKey)

	p.renameLocked(oldKey, newKey)

	for name, f := range mapFields(t, &p) {
		if f.MapIndex(reflect.ValueOf(oldKey)).IsValid() {
			t.Errorf("%s still has an entry under the OLD key after renameLocked; "+
				"a pick left behind is consumed by whatever session next holds that key", name)
		}
		if !f.MapIndex(reflect.ValueOf(newKey)).IsValid() {
			t.Errorf("%s has no entry under the NEW key after renameLocked; "+
				"renameLocked must handle every map field, including ones added after it was written", name)
		}
	}
}

// TestPendingPicks_DropAllClearsEveryMap: terminal removal must leave nothing
// behind for a future session reusing the key.
func TestPendingPicks_DropAllClearsEveryMap(t *testing.T) {
	var p pendingPicks
	p.initLocked()
	const key = "chat:gone:general"
	seedAllPicks(&p, key)

	p.dropAllLocked(key)

	for name, f := range mapFields(t, &p) {
		if f.MapIndex(reflect.ValueOf(key)).IsValid() {
			t.Errorf("%s still has an entry after dropAllLocked; an abandoned pick must not "+
				"survive terminal removal", name)
		}
	}
}

// TestPendingPicks_DropBackendLeavesTheConsumedOnes pins the ResetChat asymmetry
// as DELIBERATE. /new returns to the default backend, while accessProfile and
// tuning are consumed on the first spawn and still apply to this key.
//
// Written as an explicit expectation rather than a reflection sweep: this is the
// one method that must NOT touch every field, so a generic "clears everything"
// assertion would be wrong here.
func TestPendingPicks_DropBackendLeavesTheConsumedOnes(t *testing.T) {
	var p pendingPicks
	p.initLocked()
	const key = "chat:reset:general"
	p.backend[key] = "kiro"
	p.accessProfile[key] = "restricted"
	p.tuning[key] = pendingTuning{Model: "opus", Effort: "high"}

	p.dropBackendLocked(key)

	if _, ok := p.backend[key]; ok {
		t.Error("backend pick survived dropBackendLocked; /new must return to the default backend")
	}
	if got, ok := p.accessProfile[key]; !ok || got != "restricted" {
		t.Errorf("accessProfile pick = (%q, %v), want it preserved: it is consumed on the first "+
			"spawn and still applies to this key after /new", got, ok)
	}
	if got, ok := p.tuning[key]; !ok || got.Model != "opus" {
		t.Errorf("tuning pick = (%+v, %v), want it preserved for the same reason", got, ok)
	}
}

// TestPendingPicks_RenameOfAbsentKeyIsANoop: renameLocked runs on every rename,
// including the common case where the session made no dashboard picks at all. It
// must not fabricate entries under the new key.
func TestPendingPicks_RenameOfAbsentKeyIsANoop(t *testing.T) {
	var p pendingPicks
	p.initLocked()
	p.renameLocked("chat:absent:general", "chat:new:general")
	for name, f := range mapFields(t, &p) {
		if f.Len() != 0 {
			t.Errorf("%s gained %d entries renaming a key with no picks", name, f.Len())
		}
	}
}

// TestPendingPicks_BackendStoreHoldsNoSessionKeyedMaps is the structural half:
// backendStore is keyed by BACKEND ID. A map keyed by session key belongs here
// instead, which is why these three moved (G2 #2666).
//
// Checked by field name because that is the only signal available — Go cannot
// distinguish map[backendID]string from map[sessionKey]string. The names that
// used to live in backendStore are listed explicitly so re-adding one is what
// fails, rather than any new map.
func TestPendingPicks_BackendStoreHoldsNoSessionKeyedMaps(t *testing.T) {
	var bs backendStore
	v := reflect.ValueOf(&bs).Elem()
	moved := map[string]bool{
		"backendOverrides":       true,
		"accessProfileOverrides": true,
		"tuningOverrides":        true,
	}
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if moved[name] {
			t.Errorf("backendStore.%s is back: it is keyed by SESSION key, so every path that "+
				"manipulates a session key has to remember it. pendingPicks owns those, and "+
				"renameLocked/dropAllLocked keep them consistent.", name)
		}
	}
}
