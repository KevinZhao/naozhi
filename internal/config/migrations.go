package config

// Schema migrations: the one place a deprecated key is understood.
//
// Load runs the chain in memory on every read and reports each rewrite as a
// config-deprecated diag; `naozhi config migrate -write` applies the same
// rewrites to the FILE and bumps its schema_version, after which those diags
// stop because the deprecated keys are gone. Config itself describes only the
// current schema.
//
// Migrations operate on a yaml.Node document, not on Config: the point is to
// preserve the operator's comments, key order and formatting. A migration that
// round-tripped through Config would hand back a machine-formatted file with
// every comment stripped.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/naozhi/naozhi/internal/cli"
)

// migration rewrites a config document from schema version From to From+1.
// Apply mutates root (the document's root mapping) and returns what it
// changed; none on an already-clean document is normal (an operator may have
// written the modern keys by hand).
type migration struct {
	From  int
	Desc  string
	Apply func(root *yaml.Node) ([]change, error)
}

// change is one rewrite a migration made, in the shape Load reports it: Key is
// the dotted path of the deprecated key, Action what became of it.
type change struct {
	Key, Action, Reason string
}

func (c change) diag() cli.SpawnDiag {
	return cli.SpawnDiag{Layer: "config-deprecated", Key: c.Key, Action: c.Action, Reason: c.Reason}
}

// migrations is the chain, ordered by From. Each entry moves exactly one
// version, so upgrading v1 → v3 runs two of them in sequence and a future
// reader can see what each version changed.
var migrations = []migration{
	{
		From: 1,
		Desc: "drop the deprecated aliases: nodes → workspaces, session.workspace → session.cwd, remove the dead session.auto_chain block, lift --append-system-prompt out of agents[].args",
		Apply: func(root *yaml.Node) ([]change, error) {
			var changes []change
			// Renames keep the key's own comment, since only the key scalar's
			// Value changes.
			switch renameKey(root, "nodes", "workspaces") {
			case renamed:
				changes = append(changes, change{"nodes", "rewritten", "'nodes' is deprecated, please rename to 'workspaces'"})
			case droppedForModern:
				changes = append(changes, change{"nodes", "ignored", "both 'nodes' and 'workspaces' configured; using 'workspaces'"})
			}
			if session := yamlChildMap(root, "session"); session != nil {
				switch renameKey(session, "workspace", "cwd") {
				case renamed:
					changes = append(changes, change{"session.workspace", "rewritten", "'session.workspace' is deprecated, please rename to 'session.cwd'"})
				case droppedForModern:
					changes = append(changes, change{"session.workspace", "ignored", "both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'"})
				}
				if _, did := removeKey(session, "auto_chain"); did {
					changes = append(changes, change{"session.auto_chain", "ignored", "'session.auto_chain' is deprecated and has no effect; remove this block from config"})
				}
			}
			lifted, err := liftAgentSystemPrompts(root)
			return append(changes, lifted...), err
		},
	},
}

// MigrateDocument runs the migration chain (runMigrations) and bumps
// schema_version to CurrentSchemaVersion. It returns the descriptions of the
// migrations that changed something, so a caller can print what it would do
// (or did).
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
	applied, _, err = runMigrations(root)
	if err != nil {
		return nil, err
	}
	if from < CurrentSchemaVersion {
		setSchemaVersion(root, CurrentSchemaVersion)
		applied = append(applied, fmt.Sprintf("schema_version: %d → %d", from, CurrentSchemaVersion))
	}
	return applied, nil
}

// runMigrations applies every migration from unversionedSchema up to
// CurrentSchemaVersion, whatever version the document declares: each is a
// no-op on a document already past its shape
// (TestMigrations_AreNoOpsOnTheirTargetShape), so a file that declares v2 but
// still carries a v1 key is repaired, not mis-read (#2897 D10). It returns the
// description of each migration that changed something and the changes.
func runMigrations(root *yaml.Node) (applied []string, changes []change, err error) {
	for v := unversionedSchema; v < CurrentSchemaVersion; v++ {
		m := migrationFrom(v)
		if m == nil {
			return applied, changes, fmt.Errorf("no migration from schema_version %d to %d; this binary cannot upgrade the file", v, v+1)
		}
		cs, err := m.Apply(root)
		if err != nil {
			return applied, changes, fmt.Errorf("migrate v%d → v%d: %w", v, v+1, err)
		}
		if len(cs) > 0 {
			applied = append(applied, fmt.Sprintf("v%d → v%d: %s", v, v+1, m.Desc))
			changes = append(changes, cs...)
		}
	}
	return applied, changes, nil
}

func migrationFrom(v int) *migration {
	for i := range migrations {
		if migrations[i].From == v {
			return &migrations[i]
		}
	}
	return nil
}

// unversionedSchema is the version a document without schema_version is read
// as, and where the chain starts. Every config written before versioning has
// none, so absent is the oldest shape, not the newest; migrate writes only
// with -write.
const unversionedSchema = 1

// documentSchemaVersion reads schema_version from the document; absent (or a
// non-positive value) is unversionedSchema.
func documentSchemaVersion(root *yaml.Node) (int, error) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "schema_version" {
			continue
		}
		var v int
		// The decode error is not wrapped: it echoes the value, which Load has
		// already ${VAR}-expanded.
		if err := root.Content[i+1].Decode(&v); err != nil {
			return 0, fmt.Errorf("config schema_version is not an integer")
		}
		if v <= 0 {
			return unversionedSchema, nil
		}
		return v, nil
	}
	return unversionedSchema, nil
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

// renameOutcome is what renameKey did.
type renameOutcome int

const (
	absent           renameOutcome = iota // the old key is absent
	renamed                               // the old key now carries the new name
	droppedForModern                      // both are present; the old one is dropped
)

// renameKey renames a key in place, keeping its position, its comments and its
// value node. When both names are present it removes the OLD one and keeps the
// new: the modern key already holds the value the operator meant.
func renameKey(m *yaml.Node, from, to string) renameOutcome {
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
		return absent
	}
	if toIdx >= 0 {
		m.Content = append(m.Content[:fromIdx], m.Content[fromIdx+2:]...)
		return droppedForModern
	}
	m.Content[fromIdx].Value = to
	return renamed
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
// agents[<id>].args into agents[<id>].system_prompt. The flag is denylisted at
// spawn, so leaving it in args means it silently never reaches the CLI. A bare
// trailing flag has nothing to lift and is only dropped.
//
// It refuses rather than guesses in the one ambiguous case: args carry the flag
// AND system_prompt is already set to something different.
func liftAgentSystemPrompts(root *yaml.Node) ([]change, error) {
	agents := yamlChildMap(root, "agents")
	if agents == nil {
		return nil, nil
	}
	var changes []change
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
			return changes, fmt.Errorf("agents[%s].args: %w", id, err)
		}
		kept, lifted, found := splitLegacySystemPromptArgs(args)
		if !found {
			continue
		}
		existing := yamlChildScalar(agent, "system_prompt")
		if lifted != "" && existing != nil && existing.Value != "" && existing.Value != lifted {
			return changes, fmt.Errorf("agents[%s]: both system_prompt and %s in args are set to different values; resolve it by hand", id, legacySystemPromptFlag)
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
		field := fmt.Sprintf("agents[%s]", id)
		if lifted == "" {
			changes = append(changes, change{field + ".args", "dropped",
				"bare " + legacySystemPromptFlag + " with no value; the dangling token is removed"})
		} else {
			changes = append(changes, change{field + ".args", "rewritten",
				fmt.Sprintf("%s under args is denied at spawn; its %d bytes were lifted into %s.system_prompt — move it in config.yaml",
					legacySystemPromptFlag, len(lifted), field)})
		}
	}
	return changes, nil
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
