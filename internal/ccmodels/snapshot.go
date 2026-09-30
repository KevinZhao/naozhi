package ccmodels

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SnapshotPath is the toolbox wrapper's recommendation record, relative to the
// cc home directory. The wrapper rewrites it whenever it re-applies settings,
// which makes its mtime a usable "the recommendation moved" signal.
const SnapshotPath = ".amzn/state/recommendation-snapshot.json"

// overridePrefix is the snapshot key namespace carrying one entry per alias.
const overridePrefix = "modelOverrides."

// keyRegion and keyCredExport are the two non-model snapshot keys a prober
// needs: which Bedrock region the recommendation is scoped to, and the command
// that mints credentials for it.
const (
	keyRegion     = "env.AWS_REGION"
	keyCredExport = "awsCredentialExport"
)

// Snapshot is the toolbox wrapper's model recommendation.
type Snapshot struct {
	Aliases []Alias // sorted by sortAliases
	Model   string  // recommended top-level "model", "" when absent

	// Region is the Bedrock region the profiles below live in. Probing another
	// region would test profiles nobody selected.
	Region string
	// CredExport is a shell-quoted command line printing credentials as
	// {"Credentials":{...}}. Taken from the snapshot rather than reconstructed
	// so a wrapper that relocates keeps working.
	CredExport string
}

// ParseSnapshot reads a recommendation snapshot. The wrapper writes a flat
// object whose model entries are "modelOverrides.<alias>" keys; every other key
// (env, statusLine, permissions...) is ignored here. Absence of any override
// key is not an error — it means the wrapper recommends no model list, and the
// caller must decide whether that is a reason to leave the targets alone.
func ParseSnapshot(data []byte) (Snapshot, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("parse recommendation snapshot: %w", err)
	}
	var snap Snapshot
	for key, val := range raw {
		switch key {
		case "model":
			_ = json.Unmarshal(val, &snap.Model)
			continue
		case keyRegion:
			_ = json.Unmarshal(val, &snap.Region)
			continue
		case keyCredExport:
			_ = json.Unmarshal(val, &snap.CredExport)
			continue
		}
		alias, ok := strings.CutPrefix(key, overridePrefix)
		if !ok || alias == "" {
			continue
		}
		var profile string
		if err := json.Unmarshal(val, &profile); err != nil || profile == "" {
			continue
		}
		snap.Aliases = append(snap.Aliases, Alias{Name: alias, Profile: profile})
	}
	sortAliases(snap.Aliases)
	return snap, nil
}
