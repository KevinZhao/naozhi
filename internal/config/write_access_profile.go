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

// accessProfileIDRe bounds a profile id to the project layer's identifier
// charset so it is safe as a YAML key, session-record value and log attr.
var accessProfileIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateAccessProfileID reports whether id is a well-formed access-profile name.
func ValidateAccessProfileID(id string) error {
	if !accessProfileIDRe.MatchString(id) {
		return fmt.Errorf("access profile id %q invalid (allowed: 1-64 chars A-Za-z0-9._-, no leading dash)", id)
	}
	return nil
}

// WriteSecretFile atomically writes secret content to path with 0600 perms
// (parent dir 0700). path must be absolute and derived by the caller under a
// trusted secrets dir, never from client input. No trailing newline is added.
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

// AppendAccessProfile inserts a new access profile into config.yaml via
// yaml.Node surgery (preserving comments/ordering). It rejects an existing id,
// validates env through the same envpolicy leaf as load, and writes atomically
// 0600. It does NOT touch the live Router; the caller sequences disk before
// memory (validate → WriteSecretFile → AppendAccessProfile → Router.AddAccessProfile).
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

	// Re-parse and re-validate the produced bytes BEFORE writing so a surgery
	// bug cannot put a document on disk that the load path would reject.
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
// stable key order; only non-empty fields are emitted.
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

// sortStringsLocal is a tiny insertion sort for the handful of env keys.
func sortStringsLocal(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
