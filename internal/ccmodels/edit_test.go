package ccmodels

import (
	"encoding/json"
	"strings"
	"testing"
)

// A document shaped like ~/.naozhi/naozhi-settings.json: Fable 5.2 runs only
// through its alias → profile mapping, and keys the editor does not know.
const editDoc = `{
  "includeCoAuthoredBy": false,
  "model": "claude-opus-5-5[1m]",
  "availableModels": [
    "claude-opus-5-5[1m]",
    "claude-sonnet-5-5[1m]",
    "global.anthropic.claude-fable-5-2[1m]"
  ],
  "enforceAvailableModels": true,
  "modelOverrides": {
    "claude-opus-5-5[1m]": "global.anthropic.claude-opus-5-5[1m]",
    "claude-sonnet-5-5[1m]": "global.anthropic.claude-sonnet-5-5[1m]",
    "claude-fable-5-2[1m]": "global.anthropic.claude-fable-5-2[1m]"
  },
  "language": "chinese",
  "modelPicker": {
    "options": [
      {"model": "claude-opus-5-5[1m]", "label": "Opus 5.5 · 1M", "description": "x", "behavesAs": "claude-opus-5-5"},
      {"model": "global.anthropic.claude-fable-5-2[1m]", "label": "Fable 5.2 · 1M · global", "description": "y", "behavesAs": "claude-fable-5-1", "extra": 1}
    ],
    "replaceBuiltInOptions": true
  }
}`

func decode(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, doc)
	}
	return m
}

func TestListModels(t *testing.T) {
	ms, err := ListModels([]byte(editDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || !ms[0].Default || ms[2].Profile != "global.anthropic.claude-fable-5-2[1m]" || ms[2].Label != "Fable 5.2 · 1M · global" {
		t.Errorf("models = %+v", ms)
	}
}

// Adding a profile id outside the company list writes an alias → profile
// mapping with a different key, a picker row, and touches nothing else.
func TestAddModel_ProfileID(t *testing.T) {
	const id = "us.anthropic.claude-haiku-5-5[1m]"
	out, err := AddModel([]byte(editDoc), id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	over := m["modelOverrides"].(map[string]any)
	if over["claude-haiku-5-5[1m]"] != id {
		t.Errorf("mapping = %v, want claude-haiku-5-5[1m] → %s", over, id)
	}
	if over["claude-fable-5-2[1m]"] != "global.anthropic.claude-fable-5-2[1m]" || len(over) != 4 {
		t.Errorf("other mappings changed: %v", over)
	}
	avail := m["availableModels"].([]any)
	if avail[len(avail)-1] != id {
		t.Errorf("availableModels = %v", avail)
	}
	opts := m["modelPicker"].(map[string]any)["options"].([]any)
	last := opts[len(opts)-1].(map[string]any)
	if last["model"] != id || last["label"] != "Haiku 5.5 · 1M" || last["behavesAs"] != "claude-haiku-5-5" {
		t.Errorf("picker row = %v", last)
	}
	if opts[1].(map[string]any)["extra"] != float64(1) || m["language"] != "chinese" || m["modelPicker"].(map[string]any)["replaceBuiltInOptions"] != true {
		t.Error("an unknown key was dropped")
	}
	// Document order of the untouched keys is kept.
	if strings.Index(string(out), `"includeCoAuthoredBy"`) > strings.Index(string(out), `"language"`) {
		t.Error("top-level key order changed")
	}
	if _, err := AddModel(out, id, "", ""); err == nil {
		t.Error("adding the same model twice succeeded")
	}
}

// A bare alias is mapped onto the global pool unless something maps it.
func TestAddModel_Alias(t *testing.T) {
	out, err := AddModel([]byte(editDoc), "claude-opus-5[1m]", "Opus 5", "")
	if err != nil {
		t.Fatal(err)
	}
	over := decode(t, out)["modelOverrides"].(map[string]any)
	if over["claude-opus-5[1m]"] != "global.anthropic.claude-opus-5[1m]" {
		t.Errorf("mapping = %v", over)
	}
}

// Removing a model drops its list entry, its picker row and its own mapping
// only; Fable 5.2's mapping stays, and the default model is refused.
func TestRemoveModel(t *testing.T) {
	out, err := RemoveModel([]byte(editDoc), "claude-sonnet-5-5[1m]")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	over := m["modelOverrides"].(map[string]any)
	if _, ok := over["claude-sonnet-5-5[1m]"]; ok || len(over) != 2 || over["claude-fable-5-2[1m]"] != "global.anthropic.claude-fable-5-2[1m]" {
		t.Errorf("mappings after remove = %v", over)
	}
	if len(m["availableModels"].([]any)) != 2 {
		t.Errorf("availableModels = %v", m["availableModels"])
	}

	out, err = RemoveModel([]byte(editDoc), "global.anthropic.claude-fable-5-2[1m]")
	if err != nil {
		t.Fatal(err)
	}
	m = decode(t, out)
	over = m["modelOverrides"].(map[string]any)
	if _, ok := over["claude-fable-5-2[1m]"]; ok || len(over) != 2 {
		t.Errorf("fable mapping not removed with fable: %v", over)
	}
	if opts := m["modelPicker"].(map[string]any)["options"].([]any); len(opts) != 1 {
		t.Errorf("picker rows = %v", opts)
	}

	if _, err := RemoveModel([]byte(editDoc), "claude-opus-5-5[1m]"); err == nil {
		t.Error("removing the default model succeeded")
	}
}
