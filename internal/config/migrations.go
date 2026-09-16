package config

// Schema migrations: how a deprecated key actually LEAVES an operator's
// config.yaml.
//
// Before this, four renames/removals were handled at load time only — nodes →
// workspaces, session.workspace → session.cwd, the dead session.auto_chain
// block, and `--append-system-prompt` inside agents[].args. Every one of them
// reported itself on every boot and then stayed on disk forever, because nothing
// could rewrite the file. CurrentSchemaVersion existed but only ever refused a
// NEWER version; there were zero migration functions.
//
// The load path is unchanged: a v1 document still loads, still gets the
// in-memory rename, still reports the diag. What is new is that
// `naozhi config migrate --write` can apply the same rewrites to the FILE and
// bump its schema_version, after which those diags stop because the deprecated
// keys are gone.
//
// Migrations operate on a yaml.Node document, not on Config: the point is to
// preserve the operator's comments, key order and formatting. A migration that
// round-tripped through Config would hand back a machine-formatted file with
// every comment stripped.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// migration rewrites a config document from schema version From to From+1.
// Apply mutates root (the document's root mapping) and reports whether it
// changed anything; a no-op migration on an already-clean document is normal
// (an operator may have written the modern keys by hand).
type migration struct {
	From  int
	Desc  string
	Apply func(root *yaml.Node) (changed bool, err error)
}

// migrations is the chain, ordered by From. Each entry moves exactly one
// version, so upgrading v1 → v3 runs two of them in sequence and a future
// reader can see what each version changed.
var migrations = []migration{
	{
		From: 1,
		Desc: "drop the deprecated aliases: nodes → workspaces, session.workspace → session.cwd, remove the dead session.auto_chain block, lift --append-system-prompt out of agents[].args",
		Apply: func(root *yaml.Node) (bool, error) {
			changed := false
			// Renames keep the key's own comment, since only the key scalar's
			// Value changes.
			if did, err := renameKey(root, "nodes", "workspaces"); err != nil {
				return changed, err
			} else if did {
				changed = true
			}
			if session := yamlChildMap(root, "session"); session != nil {
				if did, err := renameKey(session, "workspace", "cwd"); err != nil {
					return changed, err
				} else if did {
					changed = true
				}
				if _, did := removeKey(session, "auto_chain"); did {
					changed = true
				}
			}
			if did, err := liftAgentSystemPrompts(root); err != nil {
				return changed, err
			} else if did {
				changed = true
			}
			return changed, nil
		},
	},
}

// MigrateDocument runs every migration from the document's schema_version up to
// CurrentSchemaVersion, in order, and bumps schema_version to match. It returns
// the descriptions of the migrations that changed something, so a caller can
// print what it would do (or did).
//
// A document with no schema_version is treated as CurrentSchemaVersion, exactly
// as the load path treats it: absent means "current", so an unversioned file is
// not silently rewritten by a future migration it was never written for.
func MigrateDocument(root *yaml.Node) (applied []string, err error) {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root is not a mapping")
	}
	from, err := documentSchemaVersion(root)
	if err != nil {
		return nil, err
	}
	if from > CurrentSchemaVersion {
		return nil, fmt.Errorf("config schema_version %d is newer than this binary supports (max %d)", from, CurrentSchemaVersion)
	}
	for v := from; v < CurrentSchemaVersion; v++ {
		m := migrationFrom(v)
		if m == nil {
			return applied, fmt.Errorf("no migration from schema_version %d to %d; this binary cannot upgrade the file", v, v+1)
		}
		changed, err := m.Apply(root)
		if err != nil {
			return applied, fmt.Errorf("migrate v%d → v%d: %w", v, v+1, err)
		}
		if changed {
			applied = append(applied, fmt.Sprintf("v%d → v%d: %s", v, v+1, m.Desc))
		}
	}
	if from < CurrentSchemaVersion {
		setSchemaVersion(root, CurrentSchemaVersion)
		applied = append(applied, fmt.Sprintf("schema_version: %d → %d", from, CurrentSchemaVersion))
	}
	return applied, nil
}

func migrationFrom(v int) *migration {
	for i := range migrations {
		if migrations[i].From == v {
			return &migrations[i]
		}
	}
	return nil
}

// documentSchemaVersion reads schema_version from the document; absent is
// CurrentSchemaVersion (the load path's rule).
func documentSchemaVersion(root *yaml.Node) (int, error) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "schema_version" {
			continue
		}
		var v int
		if err := root.Content[i+1].Decode(&v); err != nil {
			return 0, fmt.Errorf("schema_version is not an integer: %w", err)
		}
		if v <= 0 {
			return CurrentSchemaVersion, nil
		}
		return v, nil
	}
	return CurrentSchemaVersion, nil
}

// setSchemaVersion writes schema_version, adding it as the FIRST key when
// absent so the version an operator reads is at the top of their file.
func setSchemaVersion(root *yaml.Node, v int) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "schema_version" {
			root.Content[i+1].Kind = yaml.ScalarNode
			root.Content[i+1].Tag = "!!int"
			root.Content[i+1].Value = fmt.Sprint(v)
			return
		}
	}
	root.Content = append([]*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "schema_version"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)},
	}, root.Content...)
}

// renameKey renames a key in place, keeping its position, its comments and its
// value node. When both names are present it removes the OLD one and keeps the
// new: that matches the load path, which prefers cwd over the deprecated
// workspace and workspaces over nodes.
func renameKey(m *yaml.Node, from, to string) (bool, error) {
	fromIdx, toIdx := -1, -1
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case from:
			fromIdx = i
		case to:
			toIdx = i
		}
	}
	if fromIdx < 0 {
		return false, nil
	}
	if toIdx >= 0 {
		// Both present: the modern key already holds the effective value, so
		// the deprecated one is dropped rather than overwriting it.
		m.Content = append(m.Content[:fromIdx], m.Content[fromIdx+2:]...)
		return true, nil
	}
	m.Content[fromIdx].Value = to
	return true, nil
}

// removeKey drops a key and its value, reporting whether it was there. It
// returns the removed KEY node so a caller that is replacing one key with
// another can carry the operator's comments across (see takeComments).
func removeKey(m *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			removed := m.Content[i]
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return removed, true
		}
	}
	return nil, false
}

// takeComments moves from's comments onto to. A migration that replaces one key
// with another must not silently delete the operator's note: the note may now be
// stale (this one described a flag that no longer sits in args), but deleting
// text nobody asked us to delete is worse than leaving something to edit — and
// the dry run shows exactly what moved.
func takeComments(to, from *yaml.Node) {
	if to == nil || from == nil {
		return
	}
	if to.HeadComment == "" {
		to.HeadComment = from.HeadComment
	}
	if to.LineComment == "" {
		to.LineComment = from.LineComment
	}
	if to.FootComment == "" {
		to.FootComment = from.FootComment
	}
}

// liftAgentSystemPrompts moves `--append-system-prompt <text>` out of every
// agents[<id>].args into agents[<id>].system_prompt, which is what the load path
// does in memory (liftLegacySystemPromptArgs). The flag is denylisted at spawn,
// so leaving it in args means it silently never reaches the CLI.
//
// It refuses rather than guesses in the one ambiguous case: args carry the flag
// AND system_prompt is already set to something different. That is the same
// conflict the load path rejects.
func liftAgentSystemPrompts(root *yaml.Node) (bool, error) {
	agents := yamlChildMap(root, "agents")
	if agents == nil {
		return false, nil
	}
	changed := false
	for i := 0; i+1 < len(agents.Content); i += 2 {
		id, agent := agents.Content[i].Value, agents.Content[i+1]
		if agent.Kind != yaml.MappingNode {
			continue
		}
		argsNode := yamlChildSeq(agent, "args")
		if argsNode == nil {
			continue
		}
		var args []string
		if err := argsNode.Decode(&args); err != nil {
			return changed, fmt.Errorf("agents[%s].args: %w", id, err)
		}
		kept, lifted, found := splitLegacySystemPromptArgs(args)
		if !found {
			continue
		}
		existing := yamlChildScalar(agent, "system_prompt")
		if lifted != "" && existing != nil && existing.Value != "" && existing.Value != lifted {
			return changed, fmt.Errorf("agents[%s]: both system_prompt and %s in args are set to different values; resolve it by hand", id, legacySystemPromptFlag)
		}
		// Rewrite args (or drop the key when nothing is left) and set the
		// dedicated field. When args goes away entirely its comments move to
		// system_prompt, which is where the operator will look next.
		var orphanedComments *yaml.Node
		if len(kept) == 0 {
			orphanedComments, _ = removeKey(agent, "args")
		} else {
			argsNode.Content = argsNode.Content[:0]
			for _, a := range kept {
				argsNode.Content = append(argsNode.Content, &yaml.Node{
					Kind: yaml.ScalarNode, Value: a, Style: yaml.DoubleQuotedStyle,
				})
			}
		}
		if lifted != "" {
			style := yaml.DoubleQuotedStyle
			if strings.ContainsAny(lifted, "\n\"") {
				style = yaml.LiteralStyle
			}
			if existing != nil {
				existing.Value = lifted
				existing.Style = style
			} else {
				key := &yaml.Node{Kind: yaml.ScalarNode, Value: "system_prompt"}
				takeComments(key, orphanedComments)
				agent.Content = append(agent.Content, key,
					&yaml.Node{Kind: yaml.ScalarNode, Value: lifted, Style: style})
			}
		}
		changed = true
	}
	return changed, nil
}

// yamlChildSeq returns the sequence node for key, or nil.
func yamlChildSeq(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.SequenceNode {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlChildScalar returns the scalar node for key, or nil.
func yamlChildScalar(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1]
		}
	}
	return nil
}
