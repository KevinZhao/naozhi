// unknown_keys.go — report config.yaml keys that the Config struct has no
// field for (#2639).
//
// Load decodes with yaml.Unmarshal, which SILENTLY IGNORES an unknown key. So
// an operator who misspells one, or who follows documentation for a key that
// was never wired, gets no error, no effect and no way to tell why. #2553 is
// the worked example: `server.debug_mode` and `projects.public_tmp` were both
// recorded as shipped and neither key existed, so configuring them did nothing
// for months.
//
// That is precisely the failure mode SpawnDiag was built to end ("a flag being
// stripped for months with only a log line as evidence", internal/cli/
// spawn_diag.go) — and it was the one instance the diag system could not see,
// because yaml.Unmarshal throws the information away before anything
// downstream runs.
//
// Two passes, each doing what it is good at:
//
//   - yaml.Decoder with KnownFields(true) decides WHAT is unknown. The decoder
//     is authoritative about the struct's own tags, so this cannot drift from
//     the real decode the way a hand-rolled reflection walk could.
//   - a yaml.Node walk supplies the dotted PATH, which KnownFields does not
//     report: its message names only the leaf ("field enabled not found in
//     type …") and this schema has same-named leaves under a dozen parents.
//
// Load's return value is untouched — unknown keys are reported, never fatal.
// Hard-failing the boot would turn one stale key in a live config.yaml into a
// service that refuses to start after an upgrade.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/naozhi/naozhi/internal/cli"
)

// unknownKey is one config key with no corresponding struct field.
type unknownKey struct {
	Path string // dotted path as an operator wrote it ("server.debug_moed")
	Line int    // 1-based line in the config file
}

// maxUnknownKeyDiags caps the per-load report. The file itself is capped at
// 1 MiB, which is room for tens of thousands of keys; a garbage file must not
// turn into tens of thousands of Warn lines. The overflow is summarised, so
// the count is never silently truncated.
const maxUnknownKeyDiags = 20

// knownFieldsErrRe matches the one yaml.v3 TypeError message shape acted on
// here. Every other entry in that error is dropped on purpose: type-mismatch
// messages echo the offending VALUE, which after ${VAR} expansion may be a
// secret — the same reason Load keeps yaml syntax errors out of its returned
// error. Those entries are also already fatal on Load's own decode, so
// nothing is lost by ignoring them.
var knownFieldsErrRe = regexp.MustCompile(`^line (\d+): field (\S+) not found in type`)

// findUnknownKeys re-decodes the same env-expanded bytes Load decoded, with
// KnownFields(true) turned on, and returns every key the struct cannot hold,
// earliest line first.
//
// Never returns an error. A file that fails to parse here already failed on
// Load's decode, and this is a diagnostic: it must not be able to turn a
// loadable config into a failure.
func findUnknownKeys(expanded []byte) []unknownKey {
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	// A throwaway target: the caller already has its populated Config, and
	// decoding into that one again would re-apply every value for no reason.
	var probe Config
	var te *yaml.TypeError
	if err := dec.Decode(&probe); !errors.As(err, &te) {
		return nil
	}

	var found []leafKey
	for _, msg := range te.Errors {
		m := knownFieldsErrRe.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		line, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		found = append(found, leafKey{name: m[2], line: line})
	}
	if len(found) == 0 {
		return nil
	}

	paths := keyPathsByLine(expanded)
	out := make([]unknownKey, 0, len(found))
	for _, f := range found {
		path := paths[f]
		if path == "" {
			// The walk could not place it (an alias target, say). The leaf name
			// plus the line number is still actionable.
			path = f.name
		}
		out = append(out, unknownKey{Path: path, Line: f.line})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// leafKey identifies a mapping key by name and source line, which is exactly
// what KnownFields reports.
type leafKey struct {
	name string
	line int
}

// keyPathsByLine walks the document as a yaml.Node and records the dotted path
// of every mapping key. Same parser as the decode, so the (name, line) pairs
// line up exactly.
//
// Line numbers are the operator's line numbers even though this walks the
// EXPANDED bytes: expandEnvVars refuses any value containing a newline
// (containsYAMLBreakingByte), so expansion cannot shift a line.
func keyPathsByLine(expanded []byte) map[leafKey]string {
	var doc yaml.Node
	if err := yaml.Unmarshal(expanded, &doc); err != nil {
		return nil
	}
	out := map[leafKey]string{}
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, prefix)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				path := k.Value
				if prefix != "" {
					path = prefix + "." + k.Value
				}
				out[leafKey{name: k.Value, line: k.Line}] = path
				walk(v, path)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, fmt.Sprintf("%s[%d]", prefix, i))
			}
		}
		// AliasNode is deliberately not followed: its target is walked where it
		// is defined, and following it would recurse forever on a self-alias.
	}
	walk(&doc, "")
	return out
}

// reportUnknownKeys emits one diag per unknown key, through the same
// EmitSpawnDiags path the deprecated-field reports use. That buys three things
// no bespoke warning would: `naozhi config check` sees them via its observer
// and exits 1, naozhi_spawn_diag_total{layer="config-unknown"} makes a typo
// countable instead of log-greppable, and the wording lands beside the other
// "you configured this and it did nothing" findings.
func reportUnknownKeys(expanded []byte) {
	unknown := findUnknownKeys(expanded)
	if len(unknown) == 0 {
		return
	}
	shown := unknown
	if len(shown) > maxUnknownKeyDiags {
		shown = shown[:maxUnknownKeyDiags]
	}
	diags := make([]cli.SpawnDiag, 0, len(shown)+1)
	for _, u := range shown {
		diags = append(diags, cli.SpawnDiag{
			Layer:  "config-unknown",
			Key:    u.Path,
			Action: "ignored",
			Reason: fmt.Sprintf("no such config key (line %d); it is silently ignored — check the spelling against config.example.yaml", u.Line),
		})
	}
	if rest := len(unknown) - len(shown); rest > 0 {
		diags = append(diags, cli.SpawnDiag{
			Layer:  "config-unknown",
			Key:    "(more)",
			Action: "ignored",
			Reason: fmt.Sprintf("%d further unknown key(s) not listed; fix the ones above and re-run `naozhi config check`", rest),
		})
	}
	cli.EmitSpawnDiags("config", diags)
}
