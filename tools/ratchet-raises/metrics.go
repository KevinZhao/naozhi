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
// the TOTAL already counts).
type metric struct {
	value      int64
	newIsRaise bool
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

// jsRatchet reads scripts/js-ratchet.baseline.json: every per-file metric
// except lines, plus totals across files. Lines are gated only as their sum:
// moving code between files is a refactor, not a raise. A new file's metrics
// are new keys, so the totals are what keep one from absorbing growth: the sum
// of lines and of fnOver100, and the longest function anywhere (MAX.maxFnLines).
// js-ratchet --check still holds each file's own lines.
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
		for name, v := range ms {
			switch name {
			case "lines":
				totals["TOTAL.lines"] += v
				continue
			case "fnOver100":
				totals["TOTAL.fnOver100"] += v
			case "maxFnLines":
				totals["MAX.maxFnLines"] = max(totals["MAX.maxFnLines"], v)
			}
			into["js-ratchet:"+file+"."+name] = metric{value: v}
		}
	}
	for k, v := range totals {
		into["js-ratchet:"+k] = metric{value: v}
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

// raises lists every value that went up between base and head, sorted by gate.
func raises(base, head metrics) []raise {
	var out []raise
	for k, h := range head {
		b, existed := base[k]
		switch {
		case !existed && h.newIsRaise:
			out = append(out, raise{Gate: k, From: 0, To: h.value})
		case existed && h.value > b.value:
			out = append(out, raise{Gate: k, From: b.value, To: h.value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gate < out[j].Gate })
	return out
}
