package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

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
// with the file's mode after a backup (editAccessProfiles). It does NOT touch the live Router; the caller sequences disk before
// memory (validate → WriteSecretFile → AppendAccessProfile → BackendRegistry.AddAccessProfile).
func AppendAccessProfile(configPath, id string, ap AccessProfile) error {
	if err := ValidateAccessProfileID(id); err != nil {
		return err
	}
	for k, v := range ap.Env {
		if err := envpolicy.ValidateOverlayEntry(k, v); err != nil {
			return fmt.Errorf("env: %w", err)
		}
	}
	if err := envpolicy.ValidateAWSProfileName(ap.AWSProfile); err != nil {
		return fmt.Errorf("aws_profile: %w", err)
	}

	return editAccessProfiles(configPath, func(profiles *yaml.Node) error {
		// Refuse to clobber an existing entry.
		if accessProfileIndex(profiles, id) >= 0 {
			return fmt.Errorf("access profile %q already exists in config", id)
		}
		profiles.Content = append(profiles.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: id},
			accessProfileToYAML(ap))
		return nil
	})
}

// AccessProfileFields are the fields the dashboard edits on an existing
// profile; env and any key it does not know stay as they are in the file.
type AccessProfileFields struct {
	DisplayName    string
	ChipColor      string
	DefaultModel   string
	DefaultBackend string
	AWSProfile     string
}

// UpdateAccessProfile sets the dashboard's fields of profile id in
// config.yaml ("" removes the key), keeping its env, any other key and the
// file's comments. Backup, validation and write as AppendAccessProfile.
func UpdateAccessProfile(configPath, id string, f AccessProfileFields) error {
	if err := envpolicy.ValidateAWSProfileName(f.AWSProfile); err != nil {
		return fmt.Errorf("aws_profile: %w", err)
	}
	return editAccessProfiles(configPath, func(profiles *yaml.Node) error {
		i := accessProfileIndex(profiles, id)
		if i < 0 {
			return fmt.Errorf("access profile %q is not in config", id)
		}
		m := profiles.Content[i+1]
		if m.Kind != yaml.MappingNode {
			m = &yaml.Node{Kind: yaml.MappingNode}
			profiles.Content[i+1] = m
		}
		for _, kv := range [][2]string{
			{"display_name", f.DisplayName},
			{"chip_color", f.ChipColor},
			{"default_model", f.DefaultModel},
			{"default_backend", f.DefaultBackend},
			{"aws_profile", f.AWSProfile},
		} {
			setYAMLScalar(m, kv[0], kv[1])
		}
		return nil
	})
}

// RemoveAccessProfile deletes profile id from config.yaml. The produced file
// must still load, so a profile default_access_profile names is refused.
func RemoveAccessProfile(configPath, id string) error {
	return editAccessProfiles(configPath, func(profiles *yaml.Node) error {
		i := accessProfileIndex(profiles, id)
		if i < 0 {
			return fmt.Errorf("access profile %q is not in config", id)
		}
		profiles.Content = append(profiles.Content[:i], profiles.Content[i+2:]...)
		return nil
	})
}

// editAccessProfiles runs edit on the access_profiles mapping of config.yaml
// (created when absent) through yaml.Node surgery, so comments, ordering and
// keys this package does not know survive. The produced bytes must load the
// access-profile section cleanly before anything is written; the old file is
// then copied to <path>.bak-<time> and the new one written atomically with
// the file's own mode.
func editAccessProfiles(configPath string, edit func(profiles *yaml.Node) error) error {
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
	if err := edit(profiles); err != nil {
		return err
	}

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
	return WriteFileWithBackup(configPath, data, buf.Bytes())
}

// WriteFileWithBackup copies old to <path>.bak-<YYYYMMDD-HHMMSS> and then
// writes data to path atomically, both with path's current mode (0600 when
// it has none), so an edit never widens a file that may hold credentials.
func WriteFileWithBackup(path string, old, data []byte) error {
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	if len(old) > 0 {
		backup := path + ".bak-" + time.Now().Format("20060102-150405")
		if err := osutil.WriteFileAtomic(backup, old, perm); err != nil {
			return fmt.Errorf("back up %s: %w", filepath.Base(path), err)
		}
	}
	if err := osutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// accessProfileIndex returns the index of id's key node in the
// access_profiles mapping, or -1.
func accessProfileIndex(profiles *yaml.Node, id string) int {
	for i := 0; i+1 < len(profiles.Content); i += 2 {
		if profiles.Content[i].Value == id {
			return i
		}
	}
	return -1
}

// setYAMLScalar sets key to a double-quoted v in mapping m, or removes key
// when v is "".
func setYAMLScalar(m *yaml.Node, key, v string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		if v == "" {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
		m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}
		return
	}
	if v != "" {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle})
	}
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
	put("aws_profile", ap.AWSProfile)
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
