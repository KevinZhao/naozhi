package ccmodels

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Alias is one selectable row: the identifier naozhi and cc pass as `--model`,
// and the Bedrock inference profile cc's modelOverrides maps it to.
type Alias struct {
	Name    string // "claude-opus-5", "claude-opus-5[1m]"
	Profile string // "global.anthropic.claude-opus-5[1m]"
}

// OneM reports whether Name carries cc's 1M-context suffix. cc decides the
// context window from the PRE-override string, so the suffix must be on the
// alias; putting it only on the profile yields a 200k window.
func (a Alias) OneM() bool { return strings.HasSuffix(a.Name, oneMSuffix) }

const oneMSuffix = "[1m]"

// poolPrefixRe matches the cross-region inference pool prefix on a profile id
// ("global.anthropic.", "us.anthropic.").
var poolPrefixRe = regexp.MustCompile(`^[a-z0-9-]+\.anthropic\.`)

// profileVersionTailRe matches the trailing Bedrock model version on a profile
// id ("-v1:0", "-v1"), which is not part of any cc alias.
var profileVersionTailRe = regexp.MustCompile(`-v\d+(:\d+)?$`)

// RepairCandidate derives the alias implied by a.Profile, for the failure mode
// where cc expands a bare family alias to its dated form before consulting
// modelOverrides so the override never matches. Returns ok=false when the
// derived name equals a.Name or is not a plausible alias, i.e. when the profile
// offers no spelling the bare alias was not already using.
func RepairCandidate(a Alias) (Alias, bool) {
	base := strings.TrimSuffix(a.Profile, oneMSuffix)
	base = poolPrefixRe.ReplaceAllString(base, "")
	base = profileVersionTailRe.ReplaceAllString(base, "")
	if !strings.HasPrefix(base, "claude-") || base == "claude-" {
		return Alias{}, false
	}
	name := base
	if a.OneM() {
		name += oneMSuffix
	}
	if name == a.Name {
		return Alias{}, false
	}
	return Alias{Name: name, Profile: a.Profile}, true
}

// familyOrder orders the picker by family. A family absent here sorts last,
// then alphabetically, so an unfamiliar model still lands somewhere deterministic.
var familyOrder = []string{"opus", "sonnet", "fable", "haiku"}

// unrankedFamily is greater than every index in familyOrder.
const unrankedFamily = math.MaxInt

// splitAlias decomposes "claude-opus-4-8[1m]" into family "opus" and the
// numeric version components {4, 8}. Non-numeric tail segments are dropped, so
// an unparsable name degrades to an empty version rather than an error.
func splitAlias(name string) (family string, version []int) {
	base := strings.TrimSuffix(name, oneMSuffix)
	base = strings.TrimPrefix(base, "claude-")
	parts := strings.Split(base, "-")
	if len(parts) == 0 {
		return "", nil
	}
	family = parts[0]
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		version = append(version, n)
	}
	return family, version
}

// compareVersionDesc orders version components newest-first; a longer prefix-
// equal version (a dated variant) is treated as newer.
func compareVersionDesc(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return b[i] - a[i]
		}
	}
	return len(b) - len(a)
}

// sortAliases orders in place: family rank, then version descending, then the
// 1M variant ahead of the bare one (larger window first), then name. Total and
// deterministic so a re-sync of an unchanged snapshot produces no diff.
func sortAliases(as []Alias) {
	sort.SliceStable(as, func(i, j int) bool {
		fi, vi := splitAlias(as[i].Name)
		fj, vj := splitAlias(as[j].Name)
		ri, rj := rankOf(fi), rankOf(fj)
		if ri != rj {
			return ri < rj
		}
		if ri == unrankedFamily && fi != fj {
			return fi < fj
		}
		if c := compareVersionDesc(vi, vj); c != 0 {
			return c < 0
		}
		if as[i].OneM() != as[j].OneM() {
			return as[i].OneM()
		}
		return as[i].Name < as[j].Name
	})
}

func rankOf(family string) int {
	for i, f := range familyOrder {
		if f == family {
			return i
		}
	}
	return unrankedFamily
}
