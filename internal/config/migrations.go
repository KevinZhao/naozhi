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
// keep the operator's comments and key order. A migration that round-tripped
// through Config would hand back a file with every comment stripped. A comment
// on a node a migration removes moves to a neighbouring key; none is dropped.
// The yaml.v3 encoder still normalizes layout: blank lines, comment column
// alignment and indentation are not preserved, which the dry run shows.

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/spawndiag"
	"gopkg.in/yaml.v3"
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

func (c change) diag() spawndiag.Diag {
	return spawndiag.Diag{Layer: "config-deprecated", Key: c.Key, Action: c.Action, Reason: c.Reason}
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
			if si := yamlChildIndex(root, "session"); si >= 0 && root.Content[si+1].Kind == yaml.MappingNode {
				sessionKey, session := root.Content[si], root.Content[si+1]
				switch renameKey(session, "workspace", "cwd") {
				case renamed:
					changes = append(changes, change{"session.workspace", "rewritten", "'session.workspace' is deprecated, please rename to 'session.cwd'"})
				case droppedForModern:
					changes = append(changes, change{"session.workspace", "ignored", "both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'"})
				}
				if removeKey(session, sessionKey, "auto_chain") {
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
		// The modern key stays, so m cannot become empty and needs no owner.
		removeAt(m, nil, fromIdx)
		return droppedForModern
	}
	m.Content[fromIdx].Value = to
	return renamed
}

// removeKey drops a key and its value, reporting whether it was there. owner is
// the key m hangs under, the comments' last resort (see removeAt).
func removeKey(m, owner *yaml.Node, key string) bool {
	i := yamlChildIndex(m, key)
	if i < 0 {
		return false
	}
	removeAt(m, owner, i)
	return true
}

// removeAt drops the pair at m.Content[i] and re-homes every comment in it. A
// migration deletes keys nobody needs any more, never the operator's text: the
// note may now be stale, but deleting text nobody asked us to delete is worse
// than leaving something to edit, and the dry run shows where it moved.
func removeAt(m, owner *yaml.Node, i int) {
	var texts []string
	collectComments(m.Content[i], &texts)
	collectComments(m.Content[i+1], &texts)
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	rehomeComments(m, owner, i, texts)
}

// rehomeComments puts texts where a removed pair stood at index at of mapping
// m: above the next key, else below the previous one, else (m is now empty) on
// owner.
func rehomeComments(m, owner *yaml.Node, at int, texts []string) {
	switch {
	case len(texts) == 0:
	case at < len(m.Content):
		m.Content[at].HeadComment = joinComments(append(texts, m.Content[at].HeadComment))
	case len(m.Content) >= 2:
		prev := m.Content[len(m.Content)-2]
		prev.FootComment = joinComments(append([]string{prev.FootComment}, texts...))
	case owner != nil:
		owner.HeadComment = joinComments(append([]string{owner.HeadComment}, texts...))
	default:
		m.HeadComment = joinComments(append([]string{m.HeadComment}, texts...))
	}
}

// collectComments appends every comment in n's subtree to out, in document
// order.
func collectComments(n *yaml.Node, out *[]string) {
	*out = append(*out, n.HeadComment, n.LineComment)
	for _, c := range n.Content {
		collectComments(c, out)
	}
	*out = append(*out, n.FootComment)
}

// joinComments joins the non-empty comment blocks, one per line group.
func joinComments(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p)
	}
	return b.String()
}

// takeComments moves every comment in the srcs subtrees onto the pair key/val
// that replaces them, overwriting nothing: the first line comment fills val's
// line slot when the pair has none, foot comments append to key's foot, and
// the rest append to key's head in document order.
func takeComments(key, val *yaml.Node, srcs ...*yaml.Node) {
	var head, foot []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		head = append(head, n.HeadComment)
		if n.LineComment != "" && key.LineComment == "" && val.LineComment == "" {
			val.LineComment = n.LineComment
		} else {
			head = append(head, n.LineComment)
		}
		for _, c := range n.Content {
			walk(c)
		}
		foot = append(foot, n.FootComment)
	}
	for _, n := range srcs {
		walk(n)
	}
	key.HeadComment = joinComments(append([]string{key.HeadComment}, head...))
	key.FootComment = joinComments(append([]string{key.FootComment}, foot...))
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
		idKey, agent := agents.Content[i], agents.Content[i+1]
		id := idKey.Value
		if agent.Kind != yaml.MappingNode {
			continue
		}
		argsAt := yamlChildIndex(agent, "args")
		if argsAt < 0 || agent.Content[argsAt+1].Kind != yaml.SequenceNode {
			continue
		}
		argsKey, argsNode := agent.Content[argsAt], agent.Content[argsAt+1]
		var args []string
		if err := argsNode.Decode(&args); err != nil {
			return changes, fmt.Errorf("agents[%s].args: %w", id, err)
		}
		keptIdx, liftedIdx, lifted, found := splitLegacySystemPromptArgs(args)
		if !found {
			continue
		}
		var spKey, existing *yaml.Node
		if j := yamlChildIndex(agent, "system_prompt"); j >= 0 && agent.Content[j+1].Kind == yaml.ScalarNode {
			spKey, existing = agent.Content[j], agent.Content[j+1]
		}
		if lifted != "" && existing != nil && existing.Value != "" && existing.Value != lifted {
			return changes, fmt.Errorf("agents[%s]: both system_prompt and %s in args are set to different values; resolve it by hand", id, legacySystemPromptFlag)
		}
		// Rewrite args, or drop the key when nothing is left. The items that
		// stay keep their own nodes, so their comments and quoting survive;
		// gone holds the nodes whose comments need a new home.
		var gone []*yaml.Node
		if len(keptIdx) == 0 {
			gone = []*yaml.Node{argsKey, argsNode}
			agent.Content = append(agent.Content[:argsAt], agent.Content[argsAt+2:]...)
		} else {
			for _, j := range liftedIdx {
				gone = append(gone, argsNode.Content[j])
			}
			kept := make([]*yaml.Node, 0, len(keptIdx))
			for _, j := range keptIdx {
				kept = append(kept, argsNode.Content[j])
			}
			argsNode.Content = kept
		}
		switch {
		case lifted != "":
			// The comments follow the text to system_prompt, which is where
			// the operator will look next.
			style := yaml.DoubleQuotedStyle
			if strings.ContainsAny(lifted, "\n\"") {
				style = yaml.LiteralStyle
			}
			if existing != nil {
				existing.Value = lifted
				existing.Style = style
				takeComments(spKey, existing, gone...)
			} else {
				key := &yaml.Node{Kind: yaml.ScalarNode, Value: "system_prompt"}
				val := &yaml.Node{Kind: yaml.ScalarNode, Value: lifted, Style: style}
				takeComments(key, val, gone...)
				agent.Content = append(agent.Content, key, val)
			}
		default:
			// A bare flag lifts nothing: its comments stay where it stood, on
			// args when that survives, else beside where args was.
			var texts []string
			for _, n := range gone {
				collectComments(n, &texts)
			}
			if len(keptIdx) > 0 {
				argsKey.HeadComment = joinComments(append([]string{argsKey.HeadComment}, texts...))
			} else {
				rehomeComments(agent, idKey, argsAt, texts)
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

// yamlChildIndex returns the index of key's key node in mapping m, or -1.
func yamlChildIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
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
