package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/osutil"
)

// accessProfileIDRe bounds a new profile id to the same identifier charset the
// project layer accepts (alphanumeric + _-., 1-64, no leading dash). Keeps the
// id safe as a YAML key, a session-record value, and a log attr.
var accessProfileIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateAccessProfileID reports whether id is a well-formed access-profile
// name. Exported so the create-endpoint can reject bad ids before any file I/O.
func ValidateAccessProfileID(id string) error {
	if !accessProfileIDRe.MatchString(id) {
		return fmt.Errorf("access profile id %q invalid (allowed: 1-64 chars A-Za-z0-9._-, no leading dash)", id)
	}
	return nil
}

// WriteSecretFile writes secret content to path with 0600 perms, creating the
// parent dir (0700) if needed, using an atomic write so a concurrent reader
// never sees a truncated token. path must be absolute + traversal-free (the
// caller derives it under a trusted secrets dir, never from client input). The
// trailing newline is intentionally NOT added — resolveEnvOverlay trims
// trailing newlines on read, so the round-trip is byte-stable either way.
// RFC project-access-profile P1-d (token file selector auto-chmod 0600).
func WriteSecretFile(path, content string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("secret path must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	if err := osutil.WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write secret file: %w", err)
	}
	return nil
}

// AppendAccessProfile inserts a new access profile into the config.yaml at
// configPath, preserving the rest of the document (comments, ordering, and
// unrelated keys) via yaml.Node surgery. It:
//
//   - rejects an id that already exists (no silent overwrite of an operator's
//     hand-written profile);
//   - validates every env entry through envpolicy.ValidateOverlayEntry (the
//     SAME leaf the load-time validator uses), so the create path can never
//     persist a profile the load path would reject;
//   - writes atomically with 0600 (config.yaml holds no plaintext secrets — the
//     env uses *_FILE references — but it is still operator-private).
//
// It does NOT touch the live Router registry; the caller sequences
// (validate → WriteSecretFile → AppendAccessProfile → Router.AddAccessProfile)
// so disk is durable before memory changes. RFC project-access-profile P1-d.
func AppendAccessProfile(configPath, id string, ap AccessProfile) error {
	if err := ValidateAccessProfileID(id); err != nil {
		return err
	}
	for k, v := range ap.Env {
		if err := envpolicy.ValidateOverlayEntry(k, v); err != nil {
			return fmt.Errorf("env: %w", err)
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("config is not a valid YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}

	profiles := yamlChildMap(root, "access_profiles")
	if profiles == nil {
		profiles = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "access_profiles"},
			profiles)
	}
	// Refuse to clobber an existing entry.
	for i := 0; i+1 < len(profiles.Content); i += 2 {
		if profiles.Content[i].Value == id {
			return fmt.Errorf("access profile %q already exists in config", id)
		}
	}

	profiles.Content = append(profiles.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: id},
		accessProfileToYAML(ap))

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close encoder: %w", err)
	}

	// Re-parse the produced bytes and re-run the access-profile validator on the
	// result — a belt-and-braces guard that the node surgery produced a document
	// the load path accepts. yaml.Unmarshal catches structural corruption;
	// validateAccessProfiles re-checks every profile's env keys/values, default
	// model, and backend references (the same leaf the load path runs), so a
	// surgery bug that emitted, e.g., a malformed env value cannot reach disk.
	// Fail BEFORE writing.
	var check Config
	if err := yaml.Unmarshal(buf.Bytes(), &check); err != nil {
		return fmt.Errorf("re-parse produced config: %w", err)
	}
	if err := validateAccessProfiles(&check); err != nil {
		return fmt.Errorf("produced config failed access-profile validation: %w", err)
	}

	if err := osutil.WriteFileAtomic(configPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// accessProfileToYAML renders an AccessProfile as a YAML mapping node with a
// stable key order (display_name, chip_color, default_model, default_backend,
// env). Only non-empty fields are emitted so the written block stays tidy.
func accessProfileToYAML(ap AccessProfile) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	put := func(k, v string) {
		if v == "" {
			return
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle})
	}
	put("display_name", ap.DisplayName)
	put("chip_color", ap.ChipColor)
	put("default_model", ap.DefaultModel)
	put("default_backend", ap.DefaultBackend)
	if len(ap.Env) > 0 {
		envNode := &yaml.Node{Kind: yaml.MappingNode}
		// Deterministic key order for a stable on-disk diff.
		keys := make([]string, 0, len(ap.Env))
		for k := range ap.Env {
			keys = append(keys, k)
		}
		sortStringsLocal(keys)
		for _, k := range keys {
			envNode.Content = append(envNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Value: ap.Env[k], Style: yaml.DoubleQuotedStyle})
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, envNode)
	}
	return m
}

// yamlChildMap returns the mapping-node value for key under a mapping parent,
// or nil if absent / not a mapping.
func yamlChildMap(parent *yaml.Node, key string) *yaml.Node {
	if parent.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			if parent.Content[i+1].Kind == yaml.MappingNode {
				return parent.Content[i+1]
			}
			return nil
		}
	}
	return nil
}

// sortStringsLocal is a tiny insertion sort to avoid importing sort for a
// single ≤2-element key slice (env maps carry at most a handful of keys).
func sortStringsLocal(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
