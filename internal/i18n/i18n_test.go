package i18n

import (
	"sync"
	"testing"
)

// testBundle builds a small two-locale bundle for the rendering/resolution
// tests. It mirrors the design defaults: default "zh-CN", supported
// {zh-CN, en-US}, heuristic {true, 0.3, 4}.
func testBundle() *Bundle {
	return NewForTest(map[string]map[string]string{
		"zh-CN": {
			"greet":     "你好 {name}",
			"plain":     "纯文本",
			"two":       "{a} 和 {b}",
			"lang.curr": "当前语言: {locale}",
		},
		"en-US": {
			"greet": "hello {name}",
			"plain": "plain text",
		},
	})
}

func TestPickDefault(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"en-US", "zh-CN"}, "zh-CN"}, // zh-CN preferred even if not first
		{[]string{"en-US", "fr-FR"}, "en-US"}, // en-US next preference
		{[]string{"fr-FR", "de-DE"}, "fr-FR"}, // neither: first wins
		{nil, "zh-CN"},                        // empty: design default
	}
	for _, c := range cases {
		if got := pickDefault(c.in); got != c.want {
			t.Errorf("pickDefault(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCompile_MalformedBraces(t *testing.T) {
	// Unterminated and empty/malformed braces are preserved as literal text,
	// and the template still renders any valid placeholders around them.
	b := NewForTest(map[string]map[string]string{
		"x": {
			"unterminated": "a {oops",
			"empty":        "a {} b",
			"trailing":     "{name} {",
			"nested":       "{a{b}",
		},
	})
	p := b.For("x")
	if got := p.T("unterminated"); got != "a {oops" {
		t.Errorf("unterminated = %q, want %q", got, "a {oops")
	}
	if got := p.T("empty"); got != "a {} b" {
		t.Errorf("empty = %q, want %q", got, "a {} b")
	}
	if got := p.T("trailing", map[string]any{"name": "N"}); got != "N {" {
		t.Errorf("trailing = %q, want %q", got, "N {")
	}
	// "{a{b}" → "{a" is literal (inner "{" makes the first token malformed),
	// then "{b}" is a placeholder.
	if got := p.T("nested", map[string]any{"b": "B"}); got != "{aB" {
		t.Errorf("nested = %q, want %q", got, "{aB")
	}
}

func TestNormalizeLocale(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// zh family → zh-CN
		{"zh", "zh-CN"},
		{"zh-CN", "zh-CN"},
		{"zh_cn", "zh-CN"},
		{"zh-Hans", "zh-CN"},
		{"zh-Hans-CN", "zh-CN"},
		{"zh-SG", "zh-CN"},
		{"Zh-cN", "zh-CN"},
		{"  zh-CN  ", "zh-CN"},
		// zh-Hant family currently merges to zh-CN (§3.3 downgrade)
		{"zh-TW", "zh-CN"},
		{"zh-HK", "zh-CN"},
		{"zh-Hant", "zh-CN"},
		{"zh-Hant-TW", "zh-CN"},
		// POSIX encoding suffix stripping
		{"zh_CN.UTF-8", "zh-CN"},
		{"zh_cn.gbk", "zh-CN"},
		{"en_US.UTF-8", "en-US"},
		{"zh_CN.UTF-8@modifier", "zh-CN"},
		// en family → en-US
		{"en", "en-US"},
		{"en-US", "en-US"},
		{"en-GB", "en-US"},
		{"en_us", "en-US"},
		// non-whitelisted → ""
		{"ja", ""},
		{"ja-JP", ""},
		{"ko-KR", ""},
		{"fr-FR", ""},
		{"de", ""},
		{"*", ""},
		{"*.*", ""},
		{"", ""},
		{"   ", ""},
		{"zhx", ""},
		{"enx", ""},
	}
	for _, c := range cases {
		if got := NormalizeLocale(c.in); got != c.want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestT_MissingKey(t *testing.T) {
	p := testBundle().For("zh-CN")
	if got := p.T("does.not.exist"); got != "[does.not.exist]" {
		t.Errorf("missing key = %q, want %q", got, "[does.not.exist]")
	}
	// Unknown locale also yields the [key] form.
	pu := testBundle().For("ja-JP")
	if got := pu.T("greet"); got != "[greet]" {
		t.Errorf("unknown locale = %q, want %q", got, "[greet]")
	}
}

func TestT_NamedPlaceholders(t *testing.T) {
	p := testBundle().For("zh-CN")
	if got := p.T("greet", map[string]any{"name": "世界"}); got != "你好 世界" {
		t.Errorf("greet = %q, want %q", got, "你好 世界")
	}
	if got := p.T("two", map[string]any{"a": "甲", "b": "乙"}); got != "甲 和 乙" {
		t.Errorf("two = %q, want %q", got, "甲 和 乙")
	}
	// Non-string arg values are stringified.
	if got := p.T("lang.curr", map[string]any{"locale": 42}); got != "当前语言: 42" {
		t.Errorf("int arg = %q, want %q", got, "当前语言: 42")
	}
	// A template with no placeholders renders verbatim.
	if got := p.T("plain"); got != "纯文本" {
		t.Errorf("plain = %q, want %q", got, "纯文本")
	}
}

func TestT_MissingArg(t *testing.T) {
	p := testBundle().For("zh-CN")
	// No args map at all: placeholder stays literal.
	if got := p.T("greet"); got != "你好 {name}" {
		t.Errorf("no-arg greet = %q, want %q", got, "你好 {name}")
	}
	// Map present but missing the key: that placeholder stays, others fill.
	if got := p.T("two", map[string]any{"a": "甲"}); got != "甲 和 {b}" {
		t.Errorf("partial two = %q, want %q", got, "甲 和 {b}")
	}
}

func TestT_ExtraArg(t *testing.T) {
	p := testBundle().For("zh-CN")
	got := p.T("greet", map[string]any{"name": "世界", "unused": "x", "also": 1})
	if got != "你好 世界" {
		t.Errorf("extra arg = %q, want %q", got, "你好 世界")
	}
}

func TestHeuristic_CJK(t *testing.T) {
	b := testBundle()
	cases := []struct {
		text          string
		wantLocale    string
		wantConfident bool
	}{
		{"你好世界啊", "zh-CN", true},
		{"hello world test", "en-US", true},
		{"你好ab", "zh-CN", true},        // 4 runes, ratio 0.5 ≥ 0.3
		{"helloworld你", "en-US", true}, // 11 runes, ratio ~0.09 < 0.3
	}
	for _, c := range cases {
		gotLoc, gotConf := b.Heuristic(c.text)
		if gotLoc != c.wantLocale || gotConf != c.wantConfident {
			t.Errorf("Heuristic(%q) = (%q,%v), want (%q,%v)",
				c.text, gotLoc, gotConf, c.wantLocale, c.wantConfident)
		}
	}
}

func TestHeuristic_Disabled(t *testing.T) {
	b := testBundle()
	b.heuristicCfg.Enabled = false
	if loc, conf := b.Heuristic("你好世界啊这是中文"); loc != "" || conf != false {
		t.Errorf("disabled Heuristic = (%q,%v), want (\"\",false)", loc, conf)
	}
}

func TestHeuristic_ShortText(t *testing.T) {
	b := testBundle() // MinRunes == 4
	cases := []string{"abc", "你好", "", "一二三"}
	for _, txt := range cases {
		if loc, conf := b.Heuristic(txt); loc != "" || conf != false {
			t.Errorf("short Heuristic(%q) = (%q,%v), want (\"\",false)", txt, loc, conf)
		}
	}
	// Exactly MinRunes runes is enough to be confident.
	if loc, conf := b.Heuristic("一二三四"); !conf || loc != "zh-CN" {
		t.Errorf("min-runes Heuristic = (%q,%v), want (zh-CN,true)", loc, conf)
	}
}

func TestResolveIM_AllBranches(t *testing.T) {
	b := testBundle()
	cases := []struct {
		name       string
		in         IMResolveInput
		wantLocale string
		wantSource string
	}{
		{
			name:       "user lock never overridden",
			in:         IMResolveInput{PrevSource: "user", PrevLocale: "en-US", PlatformHint: "zh-CN", MessageText: "你好世界啊"},
			wantLocale: "en-US", wantSource: "user",
		},
		{
			name:       "new platform value adopted even if equal to prev",
			in:         IMResolveInput{PrevSource: "platform", PrevLocale: "zh-CN", PlatformHint: "zh-Hans"},
			wantLocale: "zh-CN", wantSource: "platform",
		},
		{
			name:       "new platform value adopted differing from prev",
			in:         IMResolveInput{PrevSource: "platform", PrevLocale: "zh-CN", PlatformHint: "en-GB"},
			wantLocale: "en-US", wantSource: "platform",
		},
		{
			name:       "unnormalizable platform hint ignored, falls to prev platform carry",
			in:         IMResolveInput{PrevSource: "platform", PrevLocale: "en-US", PlatformHint: "ja-JP"},
			wantLocale: "en-US", wantSource: "platform",
		},
		{
			name:       "prev platform carry when no hint",
			in:         IMResolveInput{PrevSource: "platform", PrevLocale: "zh-CN"},
			wantLocale: "zh-CN", wantSource: "platform",
		},
		{
			name:       "heuristic when no platform and confident text",
			in:         IMResolveInput{MessageText: "你好世界啊这里都是中文"},
			wantLocale: "zh-CN", wantSource: "heuristic",
		},
		{
			name:       "prev heuristic carry when text not confident",
			in:         IMResolveInput{PrevSource: "heuristic", PrevLocale: "en-US", MessageText: "ab"},
			wantLocale: "en-US", wantSource: "heuristic",
		},
		{
			name:       "default fallback when nothing applies",
			in:         IMResolveInput{MessageText: "ab"},
			wantLocale: "zh-CN", wantSource: "",
		},
		{
			name:       "user source but empty prev locale falls through to heuristic",
			in:         IMResolveInput{PrevSource: "user", PrevLocale: "", MessageText: "hello world here"},
			wantLocale: "en-US", wantSource: "heuristic",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLoc, gotSrc := b.ResolveIM(c.in)
			if gotLoc != c.wantLocale || gotSrc != c.wantSource {
				t.Errorf("ResolveIM = (%q,%q), want (%q,%q)", gotLoc, gotSrc, c.wantLocale, c.wantSource)
			}
		})
	}
}

func TestResolveDashboard(t *testing.T) {
	b := testBundle()
	cases := []struct {
		name           string
		cookie         string
		query          string
		acceptLanguage string
		want           string
	}{
		{"query wins over everything", "en-US", "zh", "en-GB", "zh-CN"},
		{"cookie wins over accept-language", "en-US", "", "zh-CN", "en-US"},
		{"accept-language used when no cookie/query", "", "", "en-GB,en;q=0.9", "en-US"},
		{"accept-language q-value ordering", "", "", "en;q=0.8,zh-CN;q=0.9", "zh-CN"},
		{"accept-language unsupported skips to default", "", "", "fr-FR,de;q=0.5", "zh-CN"},
		{"default when all empty", "", "", "", "zh-CN"},
		{"invalid cookie ignored, falls to default", "ja-JP", "", "", "zh-CN"},
		{"invalid query ignored, falls to cookie", "en-US", "fr", "", "en-US"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.ResolveDashboard(c.cookie, c.query, c.acceptLanguage); got != c.want {
				t.Errorf("ResolveDashboard(%q,%q,%q) = %q, want %q",
					c.cookie, c.query, c.acceptLanguage, got, c.want)
			}
		})
	}
}

// TestConcurrent_T exercises the immutable-Bundle concurrency guarantee. Run
// under -race; a data race here would mean the Bundle is being mutated through
// a Printer, violating NNH1.
func TestConcurrent_T(t *testing.T) {
	b := testBundle()
	var wg sync.WaitGroup
	const goroutines = 32
	const iters = 500
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			locale := "zh-CN"
			if g%2 == 0 {
				locale = "en-US"
			}
			p := b.For(locale)
			for i := 0; i < iters; i++ {
				_ = p.T("greet", map[string]any{"name": "x"})
				_ = p.T("missing")
				_, _ = b.Heuristic("你好世界啊")
				_, _ = b.ResolveIM(IMResolveInput{MessageText: "hello world test"})
				_ = b.ResolveDashboard("", "", "en-GB,zh;q=0.5")
				_ = NormalizeLocale("zh_CN.UTF-8")
			}
		}(g)
	}
	wg.Wait()
}
