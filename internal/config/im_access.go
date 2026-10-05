package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/imauth"
)

// IMAccessConfig is the im_access block: which IM senders may use the bot and
// which of them may run admin commands. Absent keeps every platform open.
type IMAccessConfig struct {
	// DefaultDeny refuses every sender on a platform with no entry in Platforms.
	DefaultDeny bool `yaml:"default_deny,omitempty"`
	// DenyReply replaces the direct-chat refusal text.
	DenyReply string `yaml:"deny_reply,omitempty"`
	// Platforms is keyed by platform name; see imAccessPlatforms.
	Platforms map[string]IMAccessRule `yaml:"platforms,omitempty"`
}

// IMAccessRule lists one platform's sender IDs (Feishu open_id, Slack user
// ID, Discord user ID, Weixin from).
type IMAccessRule struct {
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
	AdminUsers   []string `yaml:"admin_users,omitempty"`
}

// imAccessPlatforms are the platform names an im_access entry may use.
var imAccessPlatforms = []string{"feishu", "slack", "discord", "weixin"}

// validateIMAccess rejects every im_access shape that would leave a platform
// open, or locked, other than the way the operator wrote it: an unknown
// platform key (a typo means no rule applies), an entry with no IDs, and an ID
// that is blank, an unexpanded ${VAR} or carries control bytes (none can
// match a real sender).
func validateIMAccess(cfg *Config) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.IMAccess.Platforms)) {
		rule := cfg.IMAccess.Platforms[name]
		if !slices.Contains(imAccessPlatforms, name) {
			return fmt.Errorf("im_access.platforms: unknown platform %q (want one of %s)",
				name, strings.Join(imAccessPlatforms, ", "))
		}
		if len(rule.AllowedUsers) == 0 && len(rule.AdminUsers) == 0 {
			return fmt.Errorf("im_access.platforms.%s lists no allowed_users or admin_users; list at least one ID, or remove the entry", name)
		}
		for _, list := range []struct {
			field string
			ids   []string
		}{{"allowed_users", rule.AllowedUsers}, {"admin_users", rule.AdminUsers}} {
			for i, id := range list.ids {
				where := fmt.Sprintf("im_access.platforms.%s.%s[%d]", name, list.field, i)
				switch {
				case strings.TrimSpace(id) == "":
					return fmt.Errorf("%s is empty", where)
				case containsEnvPlaceholder(id):
					return fmt.Errorf("%s contains unexpanded ${VAR} — check environment variables", where)
				case strings.ContainsFunc(id, isControlRune):
					return fmt.Errorf("%s contains control characters", where)
				}
			}
		}
	}
	return nil
}

func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f }

// IMAccessPosture is one configured platform's im_access state.
type IMAccessPosture struct {
	Platform string
	// Open is true when the platform has no entry and default_deny is off:
	// every sender is served. No entry with Open false means default_deny.
	Open bool
	// Users counts the distinct IDs that may chat (admins included); Admins
	// counts admin_users, where 0 makes every user an admin.
	Users, Admins int
}

// IMAccessPostures reports the im_access state of every configured platform,
// in imAccessPlatforms order.
func (c *Config) IMAccessPostures() []IMAccessPosture {
	var out []IMAccessPosture
	for _, name := range imAccessPlatforms {
		if !c.hasPlatform(name) {
			continue
		}
		rule, ok := c.IMAccess.Platforms[name]
		if !ok {
			out = append(out, IMAccessPosture{Platform: name, Open: !c.IMAccess.DefaultDeny})
			continue
		}
		admins := idSet(rule.AdminUsers)
		users := idSet(rule.AllowedUsers)
		maps.Copy(users, admins)
		out = append(out, IMAccessPosture{Platform: name, Users: len(users), Admins: len(admins)})
	}
	return out
}

// imAccessDiags warns once per open platform (see IMAccessPosture.Open):
// anyone who can message the bot there runs commands on this host.
func (c *Config) imAccessDiags() []ValidationDiag {
	var diags []ValidationDiag
	for _, p := range c.IMAccessPostures() {
		if !p.Open {
			continue
		}
		diags = append(diags, ValidationDiag{
			Level: "warn",
			Field: "platforms." + p.Platform,
			Msg:   "IM 入口无鉴权：任何能私聊 bot 的用户都可在宿主机执行命令 (no sender allowlist)",
			Hint:  "set im_access.platforms." + p.Platform + ".allowed_users or im_access.default_deny: true",
		})
	}
	return diags
}

// IMAccessPolicy converts the im_access block into the policy the dispatcher
// enforces. IDs are trimmed; nothing else is normalised.
func (c *Config) IMAccessPolicy() *imauth.Policy {
	p := &imauth.Policy{
		DefaultDeny: c.IMAccess.DefaultDeny,
		DenyReply:   strings.TrimSpace(c.IMAccess.DenyReply),
		Rules:       make(map[string]imauth.Rule, len(c.IMAccess.Platforms)),
	}
	for name, r := range c.IMAccess.Platforms {
		p.Rules[name] = imauth.Rule{Allowed: idSet(r.AllowedUsers), Admins: idSet(r.AdminUsers)}
	}
	return p
}

func idSet(ids []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[strings.TrimSpace(id)] = struct{}{}
	}
	return m
}
