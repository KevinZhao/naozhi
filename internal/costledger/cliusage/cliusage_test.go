package cliusage

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// leafFields flattens embedded structs so costledger.ModelUsage's embedded
// Tokens counts as its six fields, matching clievent.ModelUsage's flat shape.
func leafFields(v reflect.Value, out map[string]reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		if f.Anonymous && fv.Kind() == reflect.Struct {
			leafFields(fv, out)
			continue
		}
		out[f.Name] = fv
	}
}

// TestCumulativeCarriesEveryField is the gate that makes one mapping worth
// having: it fails when a field is added to clievent.ModelUsage and not carried
// through. Before J1 the mapping existed three times, none of them asserting
// pass-through, so a new dimension could land in one copy and silently read zero
// in the other two.
func TestCumulativeCarriesEveryField(t *testing.T) {
	// Fill every source field with a distinct non-zero value.
	src := clievent.ModelUsage{}
	srcFields := map[string]reflect.Value{}
	leafFields(reflect.ValueOf(&src).Elem(), srcFields)
	n := int64(0)
	for _, fv := range srcFields {
		n++
		switch fv.Kind() {
		case reflect.Int64:
			fv.SetInt(n * 1000)
		case reflect.Float64:
			fv.SetFloat(float64(n) * 1.5)
		case reflect.String:
			fv.SetString("v" + string(rune('a'+n)))
		default:
			t.Fatalf("clievent.ModelUsage has a field of unhandled kind %v; extend this test", fv.Kind())
		}
	}

	got := Cumulative(9.75, map[string]clievent.ModelUsage{"m": src})
	if got.USD != 9.75 {
		t.Fatalf("USD = %v, want 9.75", got.USD)
	}
	row, ok := got.Models["m"]
	if !ok {
		t.Fatal("Models[m] missing")
	}

	dstFields := map[string]reflect.Value{}
	leafFields(reflect.ValueOf(row), dstFields)
	if len(dstFields) != len(srcFields) {
		t.Fatalf("clievent.ModelUsage has %d fields but costledger.ModelUsage has %d: a usage dimension is being dropped or invented",
			len(srcFields), len(dstFields))
	}
	for name, fv := range dstFields {
		if fv.IsZero() {
			t.Errorf("costledger.ModelUsage.%s is zero: every source field was set non-zero, so this one is not mapped", name)
		}
	}
}

// TestCumulativeLeavesModelsNilWhenEmpty pins the distinction costledger.Delta
// relies on: no rows means "no per-model detail", not "all zero".
func TestCumulativeLeavesModelsNilWhenEmpty(t *testing.T) {
	for _, in := range []map[string]clievent.ModelUsage{nil, {}} {
		got := Cumulative(1.25, in)
		if got.Models != nil {
			t.Errorf("Models = %v, want nil for input %v", got.Models, in)
		}
		if got.USD != 1.25 {
			t.Errorf("USD = %v, want 1.25", got.USD)
		}
	}
}
