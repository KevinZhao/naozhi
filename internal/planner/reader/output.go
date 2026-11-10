package reader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Limits on a reader's output (§15.12 ⑨): one past any fails the run, the
// gateway does not cut a reply to fit.
const (
	AnswerRunes = 600
	ClaimRunes  = 200
	QuoteRunes  = 200
	MaxFacts    = 8
	urlBytes    = 2048
	// maxOutputBytes bounds the reply decoded at all.
	maxOutputBytes = 64 << 10
)

// Confidence levels.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Result is a reader session's answer: a short conclusion and the facts it
// rests on, each with its page and a quote from it.
type Result struct {
	Answer     string `json:"answer"`
	Facts      []Fact `json:"facts"`
	Confidence string `json:"confidence"`
}

// Fact is one claim, its source page and the words on it.
type Fact struct {
	Claim string `json:"claim"`
	URL   string `json:"url"`
	Quote string `json:"quote"`
}

// ParseResult reads a reader session's reply strictly: one JSON object
// with exactly the contract's fields, no text before or after it and no
// code fence; answer 1–600 runes, at most 8 facts, each a claim of at most
// 200 runes, an http(s) URL and a quote of at most 200 runes, confidence
// high, medium or low. Anything else fails the run.
func ParseResult(reply string) (Result, error) {
	text := strings.TrimSpace(reply)
	if !strings.HasPrefix(text, "{") {
		return Result{}, errors.New("回复不是一个 JSON 对象")
	}
	if len(text) > maxOutputBytes {
		return Result{}, fmt.Errorf("回复 %d 字节，超过 %d", len(text), maxOutputBytes)
	}
	var res Result
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return Result{}, fmt.Errorf("JSON 不合格式：%w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("JSON 对象后面还有内容")
	}
	res.Answer = strings.TrimSpace(res.Answer)
	switch n := utf8.RuneCountInString(res.Answer); {
	case n == 0:
		return Result{}, errors.New("answer 是空的")
	case n > AnswerRunes:
		return Result{}, fmt.Errorf("answer %d 字，超过 %d", n, AnswerRunes)
	}
	switch res.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return Result{}, fmt.Errorf("confidence 是 %q，只能是 high、medium 或 low", res.Confidence)
	}
	if len(res.Facts) > MaxFacts {
		return Result{}, fmt.Errorf("facts 有 %d 条，超过 %d", len(res.Facts), MaxFacts)
	}
	if res.Facts == nil {
		res.Facts = []Fact{}
	}
	for i := range res.Facts {
		f := &res.Facts[i]
		f.Claim, f.URL, f.Quote = strings.TrimSpace(f.Claim), strings.TrimSpace(f.URL), strings.TrimSpace(f.Quote)
		switch {
		case f.Claim == "":
			return Result{}, fmt.Errorf("第 %d 条 fact 没有 claim", i+1)
		case utf8.RuneCountInString(f.Claim) > ClaimRunes:
			return Result{}, fmt.Errorf("第 %d 条 fact 的 claim 超过 %d 字", i+1, ClaimRunes)
		case utf8.RuneCountInString(f.Quote) > QuoteRunes:
			return Result{}, fmt.Errorf("第 %d 条 fact 的 quote 超过 %d 字", i+1, QuoteRunes)
		case !webURL(f.URL):
			return Result{}, fmt.Errorf("第 %d 条 fact 的 url 不是 http(s) 地址", i+1)
		}
	}
	return res, nil
}

// webURL reports whether s is an absolute http(s) URL with a host.
func webURL(s string) bool {
	if s == "" || len(s) > urlBytes {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
