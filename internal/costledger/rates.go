package costledger

import (
	"math"
	"strings"
	"sync"
)

// fallbackWeights are input / output / cache-read / cache-write prices
// relative to input, used until a model has enough observations to fit its
// own. Claude list prices keep output at 5x and 5-minute cache writes at
// 1.25x; the cache-read ratio differs by model, which is why a fit wins.
var fallbackWeights = [rateDims]float64{1, 5, 0.1, 1.25}

const (
	rateDims = 4
	// minFitObservations is how many priced rows a model needs before its
	// per-field prices are fitted instead of scaled from fallbackWeights.
	minFitObservations = 8
	// fitTotalTolerance is how far the fitted prices may miss the observed
	// total before the fit is distrusted.
	fitTotalTolerance = 0.05
	// maxRateModels bounds the book; models past it are not learned.
	maxRateModels = 64
)

// RateBook learns per-model token prices from the CLI's own priced rows
// (ModelDelta.CostUSD over its tokens), so turns that never reached a result
// frame can be priced the way the CLI would have priced them. Nil-safe:
// Observe is a no-op and Estimate reports no rate.
type RateBook struct {
	mu    sync.Mutex
	rates map[string]*modelRate
}

// modelRate holds a model's least-squares sums (cost against the four token
// columns) and the worst basis among the rows observed.
type modelRate struct {
	n      int
	ata    [rateDims][rateDims]float64
	aty    [rateDims]float64
	colSum [rateDims]float64
	usd    float64
	basis  Basis
}

// NewRateBook returns an empty book.
func NewRateBook() *RateBook { return &RateBook{rates: make(map[string]*modelRate)} }

// RateKey maps a model id onto the key prices are learned under: the
// provider prefixes (global./us./anthropic. ...), a [1m] context suffix, a
// Bedrock -vN:M revision, a Vertex @date or a -YYYYMMDD date suffix all name
// the same price.
func RateKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i > 0 && strings.HasSuffix(m, "]") {
		m = m[:i]
	}
	for _, p := range []string{"global.", "us.", "eu.", "apac.", "jp.", "au.", "anthropic."} {
		m = strings.TrimPrefix(m, p)
	}
	if i := strings.IndexByte(m, '@'); i > 0 {
		m = m[:i]
	}
	if i := strings.LastIndex(m, "-v"); i > 0 && isRevision(m[i+2:]) {
		m = m[:i]
	}
	if i := strings.LastIndexByte(m, '-'); i > 0 && len(m)-i-1 == 8 && allDigits(m[i+1:]) {
		m = m[:i]
	}
	return m
}

// isRevision matches the N or N:M after a Bedrock "-v".
func isRevision(s string) bool {
	major, minor, found := strings.Cut(s, ":")
	return allDigits(major) && (!found || allDigits(minor))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func rateColumns(t Tokens) [rateDims]float64 {
	return [rateDims]float64{float64(t.Input), float64(t.Output), float64(t.CacheRead), float64(t.CacheWrite)}
}

// Observe learns from one CLI-priced model row. Rows without a positive cost
// or without tokens teach nothing and are skipped.
func (b *RateBook) Observe(d ModelDelta) {
	if b == nil || !(d.CostUSD > 0) || math.IsInf(d.CostUSD, 0) {
		return
	}
	model := d.Model
	if model == "" {
		model = d.RawModel
	}
	key := RateKey(model)
	if key == "" || key == invalidIdent {
		return
	}
	a := rateColumns(d.Tokens)
	if a == ([rateDims]float64{}) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.rates[key]
	if r == nil {
		if len(b.rates) >= maxRateModels {
			return
		}
		r = &modelRate{}
		b.rates[key] = r
	}
	r.n++
	for i := range a {
		for j := range a {
			r.ata[i][j] += a[i] * a[j]
		}
		r.aty[i] += a[i] * d.CostUSD
		r.colSum[i] += a[i]
	}
	r.usd += d.CostUSD
	r.basis = WorseBasis(r.basis, normalizeBasis(d.Basis))
}

// Estimate prices t at model's learned rates. ok is false when the model has
// never been observed; basis is the worst basis among the rows learned from.
func (b *RateBook) Estimate(model string, t Tokens) (usd float64, basis Basis, ok bool) {
	if b == nil {
		return 0, BasisNone, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.rates[RateKey(model)]
	if r == nil {
		return 0, BasisNone, false
	}
	p := r.prices()
	a := rateColumns(t)
	for i := range a {
		usd += p[i] * a[i]
	}
	return usd, r.basis, true
}

// prices returns the fitted per-token prices when the fit is trustworthy:
// enough rows, every price non-negative, and the observed total reproduced.
// Otherwise it scales fallbackWeights to the observed total.
func (r *modelRate) prices() [rateDims]float64 {
	if r.n >= minFitObservations {
		if p, ok := solveScaled(r.ata, r.aty); ok {
			fitted := 0.0
			for i := range p {
				fitted += p[i] * r.colSum[i]
			}
			if math.Abs(fitted-r.usd) <= fitTotalTolerance*r.usd {
				return p
			}
		}
	}
	w := 0.0
	for i := range fallbackWeights {
		w += fallbackWeights[i] * r.colSum[i]
	}
	var p [rateDims]float64
	if w > 0 {
		for i := range p {
			p[i] = fallbackWeights[i] * r.usd / w
		}
	}
	return p
}

// solveScaled solves the normal equations ata·x = aty by Gaussian elimination
// with partial pivoting, after scaling each column by its norm: token columns
// differ by orders of magnitude. It fails on a singular system or any
// negative or non-finite price.
func solveScaled(ata [rateDims][rateDims]float64, aty [rateDims]float64) ([rateDims]float64, bool) {
	var x, s [rateDims]float64
	for i := range s {
		if !(ata[i][i] > 0) {
			return x, false
		}
		s[i] = math.Sqrt(ata[i][i])
	}
	var m [rateDims][rateDims + 1]float64
	for i := range m {
		for j := 0; j < rateDims; j++ {
			m[i][j] = ata[i][j] / (s[i] * s[j])
		}
		m[i][rateDims] = aty[i] / s[i]
	}
	for c := 0; c < rateDims; c++ {
		p := c
		for r := c + 1; r < rateDims; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[p][c]) {
				p = r
			}
		}
		if math.Abs(m[p][c]) < 1e-12 {
			return x, false
		}
		m[c], m[p] = m[p], m[c]
		for r := 0; r < rateDims; r++ {
			if r == c {
				continue
			}
			f := m[r][c] / m[c][c]
			for k := c; k <= rateDims; k++ {
				m[r][k] -= f * m[c][k]
			}
		}
	}
	for i := range x {
		x[i] = m[i][rateDims] / m[i][i] / s[i]
		if x[i] < 0 || math.IsNaN(x[i]) || math.IsInf(x[i], 0) {
			return x, false
		}
	}
	return x, true
}
