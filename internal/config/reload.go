package config

import (
	"reflect"
	"slices"
	"strings"
	"time"
)

// Hot reload (docs/rfc/config-hot-reload.md §3.1): the sections a running
// process applies without a restart, and the diff that tells an operator what
// a restart would still be needed for.

// hotSections are the yaml sections ApplyHotConfig can take at runtime.
// log.level is the only sub-field: the rest of `log` (format, stdio cap) is
// fixed at startup.
var hotSections = []string{"im_access", "im_rate_limit", "log.level"}

// HotSections lists the sections a reload applies without a restart.
func HotSections() []string { return slices.Clone(hotSections) }

// ReloadResult is what a reload reports: the file now in force and which of
// its sections took effect now versus need a restart.
type ReloadResult struct {
	SHA256   string    `json:"sha256"`
	LoadedAt time.Time `json:"loaded_at"`
	// Applied are the hot sections whose value changed in this reload.
	Applied []string `json:"applied"`
	// RestartRequired are the sections that differ from the configuration
	// the process started with and cannot be applied live.
	RestartRequired []string `json:"restart_required"`
	// OpenedPlatforms are the running platforms the previous configuration
	// restricted and this one serves to every sender.
	OpenedPlatforms []string `json:"opened_platforms,omitempty"`
	// OpenPlatforms are the running platforms this configuration serves to
	// every sender, whether or not this reload opened them.
	OpenPlatforms []string `json:"open_platforms,omitempty"`
}

// HotChanged lists the hot sections whose value differs between c and next.
func (c *Config) HotChanged(next *Config) []string {
	var out []string
	if !reflect.DeepEqual(c.IMAccess, next.IMAccess) {
		out = append(out, "im_access")
	}
	if !reflect.DeepEqual(c.IMRateLimit, next.IMRateLimit) {
		out = append(out, "im_rate_limit")
	}
	if c.Log.Level != next.Log.Level {
		out = append(out, "log.level")
	}
	return out
}

// IMAccessOpened lists the platforms c configures that prev restricts and
// next opens to every sender: what reloading a file whose im_access block was
// dropped or misspelt (`im_acess:` decodes as absent) does.
func (c *Config) IMAccessOpened(prev, next *Config) []string {
	var out []string
	for _, p := range c.IMAccessPostures() {
		if !prev.imAccessOpen(p.Platform) && next.imAccessOpen(p.Platform) {
			out = append(out, p.Platform)
		}
	}
	return out
}

// IMAccessOpen lists the platforms c configures that next serves to every
// sender.
func (c *Config) IMAccessOpen(next *Config) []string {
	var out []string
	for _, p := range c.IMAccessPostures() {
		if next.imAccessOpen(p.Platform) {
			out = append(out, p.Platform)
		}
	}
	return out
}

// imAccessOpen reports whether im_access lets every sender in on platform.
func (c *Config) imAccessOpen(platform string) bool {
	_, ok := c.IMAccess.Platforms[platform]
	return !ok && !c.IMAccess.DefaultDeny
}

// RestartRequired lists the top-level yaml sections, hot ones excluded, whose
// value differs between c and next. Fields without a yaml name (derived
// caches, Nodes, Fingerprint) are not compared: they follow the named ones.
func (c *Config) RestartRequired(next *Config) []string {
	var out []string
	cv, nv := reflect.ValueOf(*c), reflect.ValueOf(*next)
	t := cv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := yamlName(f)
		if name == "" || slices.Contains(hotSections, name) {
			continue
		}
		a, b := cv.Field(i).Interface(), nv.Field(i).Interface()
		if name == "log" {
			la, lb := a.(LogConfig), b.(LogConfig)
			la.Level, lb.Level = "", ""
			a, b = la, lb
		}
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	return out
}

// yamlName is f's yaml key, or "" for unexported / untagged / `yaml:"-"`.
func yamlName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag, ok := f.Tag.Lookup("yaml")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	return name
}
