package runtelemetry

import (
	"slices"
	"testing"
)

// TestEnumWireFreezeComplete pins that the wire maps in wire_stability_test.go
// cover exactly the enum constants the package declares, read from its source
// by declaredEnums: a constant added without a frozen wire string, a frozen
// entry whose constant is gone, or a new string enum type with no map fails
// here. Adding a constant needs only its wire map entry.
func TestEnumWireFreezeComplete(t *testing.T) {
	t.Parallel()
	frozen := map[string][]string{
		"RunState":    wireKeys(wireRunStates),
		"ErrorClass":  wireKeys(wireErrorClasses),
		"TriggerKind": wireKeys(wireTriggerKinds),
		"Subsystem":   wireKeys(wireSubsystems),
	}
	declared := declaredEnums(t)
	for typ := range declared {
		if _, ok := frozen[typ]; !ok {
			t.Errorf("string type %s has no wire freeze map in wire_stability_test.go", typ)
		}
	}
	for typ, wire := range frozen {
		consts, ok := declared[typ]
		if !ok || len(consts) == 0 {
			t.Errorf("source scan found no %s constants; it has gone blind or the type moved", typ)
			continue
		}
		missing, extra := 0, 0
		for _, v := range consts {
			if !slices.Contains(wire, v) {
				missing++
				t.Errorf("%s constant %q has no frozen wire string in wire_stability_test.go", typ, v)
			}
		}
		for _, v := range wire {
			if !slices.Contains(consts, v) {
				extra++
				t.Errorf("%s wire entry %q matches no declared constant", typ, v)
			}
		}
		// A constant sharing an existing literal passes both set checks above.
		if missing == 0 && extra == 0 && len(consts) != len(wire) {
			t.Errorf("%s: %d constants declared, %d frozen — a constant duplicates another's wire string", typ, len(consts), len(wire))
		}
	}
}

func wireKeys[K ~string](m map[K]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	return out
}
