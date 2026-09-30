package ccprobe

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/naozhi/naozhi/internal/ccmodels"
)

// ccTurnConcurrency bounds stage 2. Turns are network-bound and each costs a
// real Bedrock call, so this trades a ~4x wall-clock win against the chance of
// throttling ourselves into a wall of StatusUnknown.
const ccTurnConcurrency = 4

// Prober runs the two probe stages against one snapshot's worth of candidates.
//
// A zero HTTP is filled in on first use. Skipping either stage is legitimate:
// SkipCC gives a fast profile-permission check, and a Prober with no credentials
// still runs stage 2.
type Prober struct {
	Region  string
	Creds   aws.Credentials
	CLIPath string
	// BaseSettings is the operator's settings document, which stage 2 derives
	// its isolated per-alias settings from.
	BaseSettings []byte
	HTTP         *http.Client
	SkipCC       bool

	// Observe, when set, is called as each alias is decided, so a CLI can show
	// progress on a probe that takes a minute. Called from several goroutines.
	Observe func(ccmodels.Verdict)
}

// Run probes every candidate and returns verdicts keyed by alias name, ready for
// ccmodels.BuildPlan. It returns no error: a stage that cannot run leaves its
// aliases undecided, which BuildPlan keeps.
func (p *Prober) Run(ctx context.Context, candidates []ccmodels.Alias) map[string]ccmodels.Verdict {
	verdicts := make(map[string]ccmodels.Verdict, len(candidates))
	survivors := p.runConverseStage(ctx, candidates, verdicts)
	if p.SkipCC {
		return verdicts
	}
	p.runCCStage(ctx, survivors, verdicts)
	return verdicts
}

// runConverseStage probes each distinct profile once and fans the result out to
// every alias sharing it. Returns the aliases stage 2 should still test.
func (p *Prober) runConverseStage(ctx context.Context, candidates []ccmodels.Alias, verdicts map[string]ccmodels.Verdict) []ccmodels.Alias {
	if p.Creds.AccessKeyID == "" || p.Region == "" {
		p.recordAll(candidates, verdicts, ccmodels.StatusUnknown, "no credentials or region for the Converse stage")
		return candidates
	}
	hc := p.client()
	byProfile := map[string]ccmodels.Verdict{}
	survivors := make([]ccmodels.Alias, 0, len(candidates))
	for _, a := range candidates {
		v, done := byProfile[a.Profile]
		if !done {
			v = ProbeProfile(ctx, hc, p.Creds, p.Region, a.Profile)
			byProfile[a.Profile] = v
		}
		v.Alias = a.Name
		p.record(v, verdicts)
		if v.Status == ccmodels.StatusDenied {
			continue
		}
		survivors = append(survivors, a)
	}
	return survivors
}

// runCCStage runs one cc turn per alias, bounded by ccTurnConcurrency. A stage-2
// verdict always replaces the stage-1 one: it tested strictly more.
func (p *Prober) runCCStage(ctx context.Context, aliases []ccmodels.Alias, verdicts map[string]ccmodels.Verdict) {
	if p.CLIPath == "" {
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, ccTurnConcurrency)
	for _, a := range aliases {
		wg.Add(1)
		go func(a ccmodels.Alias) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			v := ProbeAlias(ctx, p.CLIPath, p.BaseSettings, a)
			mu.Lock()
			p.record(v, verdicts)
			mu.Unlock()
		}(a)
	}
	wg.Wait()
}

// record stores a verdict and notifies Observe. Callers hold any needed lock.
func (p *Prober) record(v ccmodels.Verdict, verdicts map[string]ccmodels.Verdict) {
	verdicts[v.Alias] = v
	if p.Observe != nil {
		p.Observe(v)
	}
}

func (p *Prober) recordAll(as []ccmodels.Alias, verdicts map[string]ccmodels.Verdict, st ccmodels.Status, detail string) {
	for _, a := range as {
		p.record(ccmodels.Verdict{Alias: a.Name, Status: st, Detail: detail}, verdicts)
	}
}

func (p *Prober) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: converseTimeout}
}

// EstimateDuration is a rough wall-clock estimate for a probe of n candidates,
// so a CLI can warn before spending it.
func EstimateDuration(n int, skipCC bool) time.Duration {
	d := time.Duration(n) * time.Second
	if !skipCC {
		batches := (n + ccTurnConcurrency - 1) / ccTurnConcurrency
		d += time.Duration(batches) * 12 * time.Second
	}
	return d
}

// Describe renders a verdict as one operator-facing line.
func Describe(v ccmodels.Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-34s %s", v.Alias, v.Status)
	if v.Window > 0 {
		fmt.Fprintf(&b, " (%dk window)", v.Window/1000)
	}
	if v.Detail != "" {
		fmt.Fprintf(&b, " — %s", v.Detail)
	}
	return b.String()
}
