package ccmodels

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/naozhisettings"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// The dashboard's model editor works on one settings document by hand, one
// model at a time, where `naozhi models sync` rewrites the three keys from the
// toolbox snapshot. Two facts shape it:
//
//   - The toolbox wrapper injects the company's availableModels over the
//     file's, so a model outside that list only runs when modelOverrides maps
//     some OTHER alias to its profile id ("claude-fable-5-2[1m]" →
//     "global.anthropic.claude-fable-5-2[1m]"); a key equal to its value does
//     not count. AddModel writes that mapping.
//   - Every other mapping, row and key belongs to the operator: RemoveModel
//     drops only the selected model's own entries.

// defaultPool is the inference pool a bare alias is mapped onto, as the
// existing rows are.
const defaultPool = "global.anthropic."

// Model is one entry of the editor's list.
type Model struct {
	// ID is the availableModels entry, what --model and the picker carry.
	ID string `json:"id"`
	// Profile is the Bedrock profile it runs on ("" when unknown).
	Profile string `json:"profile,omitempty"`
	// Label is its picker row's label ("" when it has none).
	Label string `json:"label,omitempty"`
	// Default marks the document's default model.
	Default bool `json:"default,omitempty"`
}

// ListModels reads availableModels in document order with each one's profile
// and picker label.
func ListModels(doc []byte) ([]Model, error) {
	avail, over, rows, def, err := readEditable(doc)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(avail))
	for _, id := range avail {
		m := Model{ID: id, Profile: profileOf(id, over), Default: id == def}
		for _, r := range rows {
			if r.model == id {
				m.Label = r.label
				break
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// AddModel appends model to availableModels, maps an alias to it in
// modelOverrides when it is a profile id (or maps a bare alias onto the
// global pool when nothing maps it yet), and adds a picker row when the
// document has a modelPicker. label and behaves default from the id.
func AddModel(doc []byte, model, label, behaves string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model is empty")
	}
	if err := tuningspec.ValidateModel("model", model); err != nil {
		return nil, err
	}
	if behaves != "" {
		if err := tuningspec.ValidateModel("behavesAs", behaves); err != nil {
			return nil, err
		}
	}
	avail, over, _, _, err := readEditable(doc)
	if err != nil {
		return nil, err
	}
	if slices.Contains(avail, model) {
		return nil, fmt.Errorf("%s is already in the list", model)
	}
	updates := map[string]json.RawMessage{}

	alias := poolPrefixRe.ReplaceAllString(model, "")
	if alias != model {
		// A profile id: it needs a different alias mapped to it.
		if !hasAliasFor(over, model) {
			if cur, ok := over[alias]; ok {
				return nil, fmt.Errorf("modelOverrides already maps %s to %s", alias, cur)
			}
			raw, err := setObjectKey(doc, keyOverrides, alias, model)
			if err != nil {
				return nil, err
			}
			updates[keyOverrides] = raw
		}
	} else if _, ok := over[model]; !ok {
		raw, err := setObjectKey(doc, keyOverrides, model, defaultPool+model)
		if err != nil {
			return nil, err
		}
		updates[keyOverrides] = raw
	}

	availRaw, err := json.Marshal(append(avail, model))
	if err != nil {
		return nil, err
	}
	updates[keyAvailable] = availRaw

	if pickRaw, ok, err := pickerWithRow(doc, model, alias, label, behaves); err != nil {
		return nil, err
	} else if ok {
		updates[keyPicker] = pickRaw
	}
	return naozhisettings.SetTopLevel(doc, updates)
}

// RemoveModel drops model from availableModels, its picker row, and the one
// modelOverrides entry that exists for it (alias → model for a profile id,
// model → profile for an alias) unless a remaining model still uses that
// entry. The default model is refused.
func RemoveModel(doc []byte, model string) ([]byte, error) {
	avail, over, _, def, err := readEditable(doc)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(avail, model) {
		return nil, fmt.Errorf("%s is not in the list", model)
	}
	if model == def {
		return nil, fmt.Errorf("%s is the default model; pick another default first", model)
	}
	rest := slices.DeleteFunc(slices.Clone(avail), func(s string) bool { return s == model })
	updates := map[string]json.RawMessage{}
	availRaw, err := json.Marshal(rest)
	if err != nil {
		return nil, err
	}
	updates[keyAvailable] = availRaw

	// The model's own mapping: alias → model for a profile id, model → its
	// profile for an alias.
	var key string
	if alias := poolPrefixRe.ReplaceAllString(model, ""); alias != model {
		if over[alias] == model {
			key = alias
		}
	} else if _, ok := over[model]; ok {
		key = model
	}
	if key != "" && !slices.Contains(rest, key) && !slices.Contains(rest, over[key]) {
		raw, err := deleteObjectKey(doc, keyOverrides, key)
		if err != nil {
			return nil, err
		}
		updates[keyOverrides] = raw
	}

	if pickRaw, ok, err := pickerWithoutRow(doc, model); err != nil {
		return nil, err
	} else if ok {
		updates[keyPicker] = pickRaw
	}
	return naozhisettings.SetTopLevel(doc, updates)
}

// pickerRowRef is the part of a picker row the editor reads.
type pickerRowRef struct {
	model, label string
}

// readEditable reads the keys the editor works on, tolerating any absent.
func readEditable(doc []byte) (avail []string, over map[string]string, rows []pickerRowRef, def string, err error) {
	var d struct {
		Available []string          `json:"availableModels"`
		Overrides map[string]string `json:"modelOverrides"`
		Model     string            `json:"model"`
		Picker    struct {
			Options []struct {
				Model string `json:"model"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"modelPicker"`
	}
	if len(bytes.TrimSpace(doc)) > 0 {
		if err := json.Unmarshal(doc, &d); err != nil {
			return nil, nil, nil, "", fmt.Errorf("parse settings: %w", err)
		}
	}
	for _, o := range d.Picker.Options {
		rows = append(rows, pickerRowRef{model: o.Model, label: o.Label})
	}
	if d.Overrides == nil {
		d.Overrides = map[string]string{}
	}
	return d.Available, d.Overrides, rows, d.Model, nil
}

// profileOf is the profile id model runs on.
func profileOf(model string, over map[string]string) string {
	if p, ok := over[model]; ok {
		return p
	}
	if poolPrefixRe.MatchString(model) {
		return model
	}
	return ""
}

// hasAliasFor reports whether some alias other than profile maps to it.
func hasAliasFor(over map[string]string, profile string) bool {
	for k, v := range over {
		if v == profile && k != profile {
			return true
		}
	}
	return false
}

// rawKey returns doc's top-level key, or nil when absent.
func rawKey(doc []byte, key string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(doc)) == 0 {
		return nil, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return nil, fmt.Errorf("parse settings: %w", err)
	}
	return top[key], nil
}

// setObjectKey returns the object at doc's top-level obj with k set to v,
// the other entries kept in order.
func setObjectKey(doc []byte, obj, k, v string) (json.RawMessage, error) {
	raw, err := rawKey(doc, obj)
	if err != nil {
		return nil, err
	}
	val, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return naozhisettings.SetTopLevel(raw, map[string]json.RawMessage{k: val})
}

// deleteObjectKey is setObjectKey removing k.
func deleteObjectKey(doc []byte, obj, k string) (json.RawMessage, error) {
	raw, err := rawKey(doc, obj)
	if err != nil {
		return nil, err
	}
	return naozhisettings.SetTopLevel(raw, map[string]json.RawMessage{k: nil})
}

// pickerOptions returns modelPicker's raw object and its options; ok=false
// when the document has no modelPicker.
func pickerOptions(doc []byte) (picker json.RawMessage, options []json.RawMessage, ok bool, err error) {
	picker, err = rawKey(doc, keyPicker)
	if err != nil || picker == nil {
		return nil, nil, false, err
	}
	var p struct {
		Options []json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(picker, &p); err != nil {
		return nil, nil, false, fmt.Errorf("parse %s: %w", keyPicker, err)
	}
	return picker, p.Options, true, nil
}

// pickerWithRow returns modelPicker with a row for model appended.
func pickerWithRow(doc []byte, model, alias, label, behaves string) (json.RawMessage, bool, error) {
	picker, options, ok, err := pickerOptions(doc)
	if err != nil || !ok {
		return nil, false, err
	}
	a := Alias{Name: alias}
	if label == "" {
		label = pickerLabel(a)
	}
	desc := model
	if a.OneM() {
		desc += " · 1M context"
	}
	if behaves == "" {
		behaves = cmp.Or(behavesAs(alias), alias)
		if behaves == model {
			behaves = ""
		}
	}
	row, err := json.Marshal(PickerRow{Model: model, Label: label, Description: desc, BehavesAs: behaves})
	if err != nil {
		return nil, false, err
	}
	return setPickerOptions(picker, append(options, row))
}

// pickerWithoutRow returns modelPicker without model's rows.
func pickerWithoutRow(doc []byte, model string) (json.RawMessage, bool, error) {
	picker, options, ok, err := pickerOptions(doc)
	if err != nil || !ok {
		return nil, false, err
	}
	kept := options[:0:0]
	for _, o := range options {
		var r struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(o, &r) == nil && r.Model == model {
			continue
		}
		kept = append(kept, o)
	}
	return setPickerOptions(picker, kept)
}

func setPickerOptions(picker json.RawMessage, options []json.RawMessage) (json.RawMessage, bool, error) {
	if options == nil {
		options = []json.RawMessage{}
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return nil, false, err
	}
	out, err := naozhisettings.SetTopLevel(picker, map[string]json.RawMessage{"options": raw})
	return out, err == nil, err
}
