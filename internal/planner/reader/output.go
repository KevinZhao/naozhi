package reader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/textutil"
)

// Limits on a reader's output (§15.12 ⑨). Past AnswerRunes, MaxFacts,
// ClaimRunes or QuoteRunes the gateway cuts the reply to fit and says so
// (Result.Truncated): the search is already paid for and counted against
// the shift, and a long answer is still an answer. Past maxOutputBytes, or
// a reply that is no contract JSON at all, fails the run.
const (
	AnswerRunes = 1000
	ClaimRunes  = 200
	QuoteRunes  = 200
	MaxFacts    = 8
	urlBytes    = 2048
	// maxOutputBytes bounds the reply decoded at all.
	maxOutputBytes = 64 << 10
	// TruncatedMark ends an answer cut to AnswerRunes.
	TruncatedMark = "（已截断）"
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
	// Truncated names what the gateway cut to the limits, in reply order:
	// "answer", "facts" (past MaxFacts dropped), "facts[i].claim",
	// "facts[i].quote" (i from 1). Set only by ParseResult, never read
	// from the reader's reply.
	Truncated []string `json:"truncated,omitempty"`
}

// Fact is one claim, its source page and the words on it.
type Fact struct {
	Claim string `json:"claim"`
	URL   string `json:"url"`
	Quote string `json:"quote"`
}

// reply is the contract's shape as the reader writes it: Result without
// Truncated, so a reply carrying "truncated" is an unknown field.
type reply struct {
	Answer     string `json:"answer"`
	Facts      []Fact `json:"facts"`
	Confidence string `json:"confidence"`
}

// ParseResult reads a reader session's reply strictly: one JSON object
// with exactly the contract's fields, no text before or after it and no
// code fence, at most 64KB; a non-empty answer, each fact a non-empty
// claim and an http(s) URL, confidence high, medium or low. Anything else
// fails the run. What is only too long is cut, and named in Truncated: an
// answer past AnswerRunes keeps its first AnswerRunes-len(TruncatedMark)
// runes (white space at the cut trimmed) and ends in TruncatedMark, so a
// cut answer, mark included, is at most AnswerRunes runes; facts past
// MaxFacts are dropped unchecked; a claim or quote past its limit keeps
// its first ClaimRunes / QuoteRunes runes, no mark (a quote is the page's
// words).
func ParseResult(text string) (Result, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		return Result{}, errors.New("回复不是一个 JSON 对象")
	}
	if len(text) > maxOutputBytes {
		return Result{}, fmt.Errorf("回复 %d 字节，超过 %d", len(text), maxOutputBytes)
	}
	var in reply
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Result{}, fmt.Errorf("JSON 不合格式：%w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("JSON 对象后面还有内容")
	}
	res := Result{Answer: strings.TrimSpace(in.Answer), Facts: in.Facts, Confidence: in.Confidence}
	switch n := utf8.RuneCountInString(res.Answer); {
	case n == 0:
		return Result{}, errors.New("answer 是空的")
	case n > AnswerRunes:
		keep := textutil.TruncateRunesNoEllipsis(res.Answer, AnswerRunes-utf8.RuneCountInString(TruncatedMark))
		res.Answer = strings.TrimRightFunc(keep, unicode.IsSpace) + TruncatedMark
		res.Truncated = append(res.Truncated, "answer")
	}
	switch res.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return Result{}, fmt.Errorf("confidence 是 %q，只能是 high、medium 或 low", res.Confidence)
	}
	if len(res.Facts) > MaxFacts {
		res.Facts = res.Facts[:MaxFacts:MaxFacts]
		res.Truncated = append(res.Truncated, "facts")
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
		case !webURL(f.URL):
			return Result{}, fmt.Errorf("第 %d 条 fact 的 url 不是 http(s) 地址", i+1)
		}
		if utf8.RuneCountInString(f.Claim) > ClaimRunes {
			f.Claim = textutil.TruncateRunesNoEllipsis(f.Claim, ClaimRunes)
			res.Truncated = append(res.Truncated, fmt.Sprintf("facts[%d].claim", i+1))
		}
		if utf8.RuneCountInString(f.Quote) > QuoteRunes {
			f.Quote = textutil.TruncateRunesNoEllipsis(f.Quote, QuoteRunes)
			res.Truncated = append(res.Truncated, fmt.Sprintf("facts[%d].quote", i+1))
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
