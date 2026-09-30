package ccmodels

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/naozhi/naozhi/internal/naozhisettings"
)

// The three settings keys this package writes. Everything else in a target
// document belongs to the operator.
const (
	keyAvailable = "availableModels"
	keyOverrides = "modelOverrides"
	keyPicker    = "modelPicker"
)

// picker is cc's modelPicker value. replaceBuiltInOptions drops cc's per-family
// lineup, which would otherwise shadow these rows for the families it covers.
type picker struct {
	Options               []PickerRow `json:"options"`
	ReplaceBuiltInOptions bool        `json:"replaceBuiltInOptions"`
}

// PatchSettings returns doc with availableModels, modelOverrides and modelPicker
// set from plan. Every other key keeps its value and position, so a hand-tuned
// settings file survives a sync as a three-key diff.
func PatchSettings(doc []byte, plan Plan) ([]byte, error) {
	avail, err := json.Marshal(plan.Available())
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", keyAvailable, err)
	}
	over, err := json.Marshal(plan.Overrides())
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", keyOverrides, err)
	}
	pick, err := json.Marshal(picker{Options: plan.PickerRows(), ReplaceBuiltInOptions: true})
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", keyPicker, err)
	}
	out, err := naozhisettings.SetTopLevel(doc, map[string]json.RawMessage{
		keyAvailable: avail,
		keyOverrides: over,
		keyPicker:    pick,
	})
	if err != nil {
		return nil, fmt.Errorf("patch settings: %w", err)
	}
	return out, nil
}

// keyFallback names cc's silent substitute model. A probe must not have one:
// with a fallback configured, an alias cc cannot resolve still answers, from the
// wrong model, and the probe would call it healthy.
const keyFallback = "fallbackModel"

// ProbeSettings derives a settings document offering only one alias, for probing
// that alias in isolation. base supplies the operator's credential export and
// Bedrock env verbatim; the model keys are replaced and fallbackModel removed.
func ProbeSettings(base []byte, a Alias) ([]byte, error) {
	doc, err := PatchSettings(base, Plan{Aliases: []Alias{a}})
	if err != nil {
		return nil, err
	}
	out, err := naozhisettings.SetTopLevel(doc, map[string]json.RawMessage{keyFallback: nil})
	if err != nil {
		return nil, fmt.Errorf("drop %s: %w", keyFallback, err)
	}
	return out, nil
}

// Remap is an alias that stays offered but now points at a different profile.
type Remap struct {
	Alias, From, To string
}

// Diff is the operator-facing change a sync would make to one target document.
type Diff struct {
	Added    []string
	Removed  []string
	Remapped []Remap
	// PickerRows is set when the /model rows would change, including when only a
	// label or a measured window moved and the offered set did not.
	PickerRows bool
}

// Empty reports whether applying the plan would change nothing.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Remapped) == 0 && !d.PickerRows
}

// DiffSettings compares doc's current model pair against plan. A missing or
// unparsable pair reads as empty, so a fresh settings file diffs as all-added
// rather than erroring; only a doc that is not a JSON object is an error.
func DiffSettings(doc []byte, plan Plan) (Diff, error) {
	cur, curOver, curPicker, err := readPair(doc)
	if err != nil {
		return Diff{}, err
	}
	want := plan.Available()
	wantOver := plan.Overrides()

	inWant := make(map[string]bool, len(want))
	for _, a := range want {
		inWant[a] = true
	}
	var d Diff
	for _, a := range cur {
		if !inWant[a] {
			d.Removed = append(d.Removed, a)
		}
	}
	inCur := make(map[string]bool, len(cur))
	for _, a := range cur {
		inCur[a] = true
	}
	for _, a := range want {
		if !inCur[a] {
			d.Added = append(d.Added, a)
			continue
		}
		if from := curOver[a]; from != wantOver[a] {
			d.Remapped = append(d.Remapped, Remap{Alias: a, From: from, To: wantOver[a]})
		}
	}
	sort.Slice(d.Remapped, func(i, j int) bool { return d.Remapped[i].Alias < d.Remapped[j].Alias })
	d.PickerRows = !reflect.DeepEqual(curPicker, picker{Options: plan.PickerRows(), ReplaceBuiltInOptions: true})
	return d, nil
}

// readPair extracts the three keys this package owns from a settings document.
func readPair(doc []byte) (available []string, overrides map[string]string, pick picker, err error) {
	overrides = map[string]string{}
	if len(doc) == 0 {
		return nil, overrides, pick, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(doc, &raw); err != nil {
		return nil, nil, pick, fmt.Errorf("parse settings document: %w", err)
	}
	_ = json.Unmarshal(raw[keyAvailable], &available)
	_ = json.Unmarshal(raw[keyOverrides], &overrides)
	_ = json.Unmarshal(raw[keyPicker], &pick)
	if overrides == nil {
		overrides = map[string]string{}
	}
	return available, overrides, pick, nil
}
