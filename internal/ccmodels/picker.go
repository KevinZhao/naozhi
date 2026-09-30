package ccmodels

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PickerRow is one row of cc's `modelPicker.options`.
//
// cc's own `/model` lineup is one row per family, keyed off
// ANTHROPIC_DEFAULT_<FAMILY>_MODEL, so availableModels never widens it: an
// older version or a [1m] variant has no row to appear in. These rows are the
// only way to offer the whole reconciled list interactively.
type PickerRow struct {
	Model       string `json:"model"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// BehavesAs names a catalog spelling cc definitely knows, set only when Model
	// is not one. Without it cc drops the row for a model its catalog lacks.
	BehavesAs string `json:"behavesAs,omitempty"`
}

// datedTailRe matches the release-date segment cc appends when it expands a bare
// family alias ("claude-haiku-4-5-20251001").
var datedTailRe = regexp.MustCompile(`-\d{8}$`)

// PickerRows renders the offered aliases as picker rows, in plan order.
func (p Plan) PickerRows() []PickerRow {
	out := make([]PickerRow, 0, len(p.Aliases))
	for _, a := range p.Aliases {
		out = append(out, PickerRow{
			Model:       a.Name,
			Label:       pickerLabel(a),
			Description: pickerDescription(a, p.Windows[a.Name]),
			BehavesAs:   behavesAs(a.Name),
		})
	}
	return out
}

// pickerLabel renders "Opus 5 · 1M" from an alias, preferring a readable family
// and version over the raw id, which the subtitle carries in full.
func pickerLabel(a Alias) string {
	family, version := splitAlias(canonicalName(a.Name))
	if family == "" {
		return a.Name
	}
	label := strings.ToUpper(family[:1]) + family[1:]
	if len(version) > 0 {
		parts := make([]string, 0, len(version))
		for _, n := range version {
			parts = append(parts, strconv.Itoa(n))
		}
		label += " " + strings.Join(parts, ".")
	}
	if a.OneM() {
		label += " · 1M"
	}
	return label
}

// pickerDescription carries the exact id cc will send, plus the window when the
// probe measured one — the suffix advertises 1M but only a turn proves it.
func pickerDescription(a Alias, window int) string {
	if window > 0 {
		return fmt.Sprintf("%s · %s context", a.Name, formatWindow(window))
	}
	return a.Name
}

func formatWindow(window int) string {
	if window >= 1_000_000 && window%1_000_000 == 0 {
		return fmt.Sprintf("%dM", window/1_000_000)
	}
	return fmt.Sprintf("%dk", window/1000)
}

// behavesAs returns the catalog spelling to vouch for name, or "" when name is
// already one. A [1m] suffix and a release-date tail are the two spellings cc's
// catalog does not carry.
func behavesAs(name string) string {
	if c := canonicalName(name); c != name {
		return c
	}
	return ""
}

func canonicalName(name string) string {
	return datedTailRe.ReplaceAllString(strings.TrimSuffix(name, oneMSuffix), "")
}
