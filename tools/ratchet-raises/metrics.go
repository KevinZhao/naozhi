package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// metric is one ratchet value. A key that exists only in head is a raise
// from zero when newIsRaise is set (a new coupling edge, a new exemption) and
// a new ratchet otherwise (a new baseline constant, a new JS file whose lines
// the TOTAL already counts). A key that exists only in base — the baseline
// lost the file that carried it, or the whole document — is a raise to -1
// when goneIsRaise is set: deleting the ratchet silently un-does it (#3025).
// anyChangeIsRaise raises on a value change in either direction, for
// metrics whose value is not itself ordered (a sha-derived int for a golden
// pin: a smaller number is not an improvement).
type metric struct {
	value            int64
	newIsRaise       bool
	goneIsRaise      bool
	anyChangeIsRaise bool
}

type metrics map[string]metric

// goBaselineConst matches an integer constant whose name says it is a
// baseline, alone or inside a const block.
var goBaselineConst = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w*[Bb]aseline\w*)\s*=\s*(\d+)\s*$`)

// goConsts reads every baseline constant in files (path → source).
func goConsts(files map[string]string, into metrics) {
	for path, src := range files {
		for _, m := range goBaselineConst.FindAllStringSubmatch(src, -1) {
			v, _ := strconv.ParseInt(m[2], 10, 64)
			into["go:"+path+"#"+m[1]] = metric{value: v}
		}
	}
}

// jsRatchet reads scripts/js-ratchet.baseline.json. lines, configureDeps,
// deadInjections, innerHTMLAssign, htmlInsert and lateBindings are gated only
// as their sum: moving code between files is a refactor, not a raise (S20a,
// #3026 D-S20-4); the other per-file metrics are keys of their own. A new
// file's metrics are new keys, so the totals and MAX.maxFnLines keep one from
// absorbing growth; js-ratchet --check still holds each file's own values.
// The "_global" entry is not a file: its metrics are GLOBAL.<name>.
func jsRatchet(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc map[string]map[string]int64
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-ratchet baseline: %w", err)
	}
	// A total exists only once some file carries its metric: a revision that
	// predates the metric has no such ratchet, rather than one at zero.
	totals := map[string]int64{}
	for file, ms := range doc {
		if file == jsGlobal {
			for name, v := range ms {
				totals["GLOBAL."+name] = v
			}
			continue
		}
		for name, v := range ms {
			switch name {
			case "lines", "configureDeps", "deadInjections", "innerHTMLAssign", "htmlInsert", "lateBindings":
				totals["TOTAL."+name] += v
				continue
			case "fnOver100":
				totals["TOTAL.fnOver100"] += v
			case "maxFnLines":
				totals["MAX.maxFnLines"] = max(totals["MAX.maxFnLines"], v)
			}
			into["js-ratchet:"+file+"."+name] = metric{value: v}
		}
	}
	// Deleting a file's metrics, or the whole document, must not silently
	// erase the sum/max it fed: a total only base holds is a raise to -1.
	for k, v := range totals {
		into["js-ratchet:"+k] = metric{value: v, goneIsRaise: true}
	}
	return nil
}

// jsGlobal is the js-ratchet baseline's cross-file entry (no static/ file
// can be named it: they all end in .js).
const jsGlobal = "_global"

// jsCaps reads scripts/js-ratchet.caps.json: the fail-closed caps that sit
// alongside js-ratchet.baseline.json (#3025 S19-0). maxFnLines.default and
// lines.<file> are ratchet constants (goneIsRaise: deleting the cap must not
// silently remove it). exempt / sideEffectLegacy / cycleLegacy are escape
// hatches (newIsRaise: a new entry loosens the gate; dropping one — the file
// got clean — is free, same as any other ratchet improvement). default is
// decoded as an int64, like every other ratchet value here: encoding/json
// then rejects 1e19 or 120.5 outright, where a float64 converted with
// int64(...) is implementation-defined out of range (amd64 gives MinInt64,
// so "default": 1e19 would read as a cap lowered below 120 and pass).
func jsCaps(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		MaxFnLines struct {
			Default *int64   `json:"default"`
			Exempt  []string `json:"exempt"`
		} `json:"maxFnLines"`
		Lines            map[string]int64 `json:"lines"`
		SideEffectLegacy []string         `json:"sideEffectLegacy"`
		CycleLegacy      []string         `json:"cycleLegacy"`
		Leaves           []string         `json:"leaves"`
		// The lists js-ratchet's analysis reads (S20a): an allowed receiver
		// or a new shell root zeroes or frees counts; a dropped late-binding
		// table stops counting its writes.
		InjectionAllow    []string          `json:"injectionAllow"`
		ShellRoots        []string          `json:"shellRoots"`
		LateBindingTables map[string]string `json:"lateBindingTables"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-ratchet caps: %w", err)
	}
	if doc.MaxFnLines.Default != nil {
		into["js-caps:maxFnLines.default"] = metric{value: *doc.MaxFnLines.Default, goneIsRaise: true}
	}
	for _, f := range doc.MaxFnLines.Exempt {
		into["js-caps:exempt:"+f] = metric{value: 1, newIsRaise: true}
	}
	for f, v := range doc.Lines {
		into["js-caps:lines."+f] = metric{value: v, goneIsRaise: true}
	}
	for _, f := range doc.SideEffectLegacy {
		into["js-caps:sideEffectLegacy:"+f] = metric{value: 1, newIsRaise: true}
	}
	for _, f := range doc.CycleLegacy {
		into["js-caps:cycleLegacy:"+f] = metric{value: 1, newIsRaise: true}
	}
	// A leaf may import only other leaves (S20a); dropping a file from the
	// list frees it to import anything, so a lost entry is the raise.
	for _, f := range doc.Leaves {
		into["js-caps:leaf:"+f] = metric{value: 1, goneIsRaise: true}
	}
	for _, a := range doc.InjectionAllow {
		into[capsSections["injectionAllow"]+a] = metric{value: 1, newIsRaise: true}
	}
	for _, f := range doc.ShellRoots {
		into[capsSections["shellRoots"]+f] = metric{value: 1, newIsRaise: true}
	}
	for name, f := range doc.LateBindingTables {
		into[capsSections["lateBindingTables"]+name+"="+f] = metric{value: 1, goneIsRaise: true}
	}
	return nil
}

// capsSections maps the caps.json lists that arrived after the document
// itself to their metric key prefix. A list base does not have yet is being
// created, so its first entries are recorded, not raised (run()), the same
// rule as for the whole document.
var capsSections = map[string]string{
	"injectionAllow":    "js-caps:injectionAllow:",
	"shellRoots":        "js-caps:shellRoot:",
	"lateBindingTables": "js-caps:lateBindingTable:",
}

// goldenPins reads test/e2e/golden/pins.json: a map from golden fixture file
// to its sha256 (hex). Only the first 12 hex chars are kept, as an int64 —
// enough entropy to make a collision between two genuinely different
// renderings not worth engineering ratchet metrics around, and small enough
// to fit the same int64 every other metric uses. A changed or deleted pin is
// a raise; new pins are not newIsRaise here, so creating the file is free,
// and run() marks them newIsRaise once base already has a pins document.
func goldenPins(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("golden pins: %w", err)
	}
	for file, sha := range doc {
		short := sha
		if len(short) > 12 {
			short = short[:12]
		}
		v, err := strconv.ParseInt(short, 16, 64)
		if err != nil {
			return fmt.Errorf("golden pins: %s: bad sha %q: %w", file, sha, err)
		}
		into["golden:"+file] = metric{value: v, goneIsRaise: true, anyChangeIsRaise: true}
	}
	return nil
}

// jsDeps reads scripts/js-deps-baseline.json. Each cross-file reference in
// matrix and tdz is an edge that must not appear; typeofGuards and bridgeRefs
// are counts that must not grow.
func jsDeps(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		Matrix       map[string]map[string][]string `json:"matrix"`
		TDZ          map[string]map[string][]string `json:"tdz"`
		TypeofGuards map[string]int64               `json:"typeofGuards"`
		BridgeRefs   map[string]int64               `json:"bridgeRefs"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-deps baseline: %w", err)
	}
	edges := func(kind string, m map[string]map[string][]string) {
		for from, tos := range m {
			for to, syms := range tos {
				for _, s := range syms {
					into["js-deps:"+kind+":"+from+">"+to+":"+s] = metric{value: 1, newIsRaise: true}
				}
			}
		}
	}
	edges("matrix", doc.Matrix)
	edges("tdz", doc.TDZ)
	for f, v := range doc.TypeofGuards {
		into["js-deps:typeofGuards:"+f] = metric{value: v, newIsRaise: true}
	}
	for f, v := range doc.BridgeRefs {
		into["js-deps:bridgeRefs:"+f] = metric{value: v, newIsRaise: true}
	}
	return nil
}

// exemptions reads tools/lint-server-handlers/exemptions.yaml. A new
// file_size entry, a higher current or limit, a later until date and a new
// handle_baseline entry each loosen the lint.
func exemptions(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		FileSize []struct {
			Path    string `yaml:"path"`
			Current int64  `yaml:"current"`
			Limit   int64  `yaml:"limit"`
			Until   string `yaml:"until"`
		} `yaml:"file_size"`
		HandleBaseline []string `yaml:"handle_baseline"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("exemptions: %w", err)
	}
	for _, e := range doc.FileSize {
		k := "exemptions:file_size:" + e.Path
		into[k] = metric{value: 1, newIsRaise: true}
		into[k+".current"] = metric{value: e.Current}
		into[k+".limit"] = metric{value: e.Limit}
		// 2027-03-31 → 20270331, so a later date is a larger number.
		d, _ := strconv.ParseInt(strings.ReplaceAll(e.Until, "-", ""), 10, 64)
		into[k+".until"] = metric{value: d}
	}
	for _, h := range doc.HandleBaseline {
		into["exemptions:handle_baseline:"+h] = metric{value: 1, newIsRaise: true}
	}
	return nil
}

// raise is one loosened ratchet value.
type raise struct {
	Gate string `json:"gate"`
	From int64  `json:"from"`
	To   int64  `json:"to"`
}

// raises lists every value that went up between base and head, sorted by
// gate. A key base held but head lost is a raise to -1 when base marked it
// goneIsRaise (#3025).
func raises(base, head metrics) []raise {
	var out []raise
	seen := make(map[string]bool, len(head))
	for k, h := range head {
		seen[k] = true
		b, existed := base[k]
		switch {
		case !existed && h.newIsRaise:
			out = append(out, raise{Gate: k, From: 0, To: h.value})
		case existed && h.value > b.value:
			out = append(out, raise{Gate: k, From: b.value, To: h.value})
		case existed && h.value < b.value && (h.anyChangeIsRaise || b.anyChangeIsRaise):
			out = append(out, raise{Gate: k, From: b.value, To: h.value})
		}
	}
	for k, b := range base {
		if seen[k] || !b.goneIsRaise {
			continue
		}
		out = append(out, raise{Gate: k, From: b.value, To: -1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gate < out[j].Gate })
	return out
}
