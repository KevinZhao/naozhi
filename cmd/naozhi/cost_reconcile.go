// cost_reconcile.go — `naozhi cost reconcile`: settle session ledger entries
// against the transcripts they were charged for (#3210).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/costledger/cliusage"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runlog"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

const (
	// An entry charged a restored cost-state total r when r >= restoreFloor,
	// Amount >= restoreRatio*r, and it exceeds its own turn's transcript
	// usage by restoreExcess of r: nearer r than 0, as that window can also
	// hold spend no entry booked and the entry spend no transcript shows.
	restoreRatio  = 0.98
	restoreFloor  = 0.5
	restoreExcess = 0.5
	// residualFloor and residualShare bound the per-day gap left alone:
	// max(residualFloor, residualShare*transcript).
	residualFloor = 1.0
	residualShare = 0.05

	reconcilePrefix = "reconcile:"
)

// runCostReconcile parses `naozhi cost reconcile` and runs it.
func runCostReconcile(args []string) {
	fs, configPath := newSubFlagSet("cost reconcile", "config.yaml")
	sid := fs.String("session", "", "only this CLI session id")
	until := fs.String("until", "", "settle days before this UTC date (YYYY-MM-DD, default today)")
	claudeDir := fs.String("claude-dir", claudefs.DefaultDir(), "the Claude CLI directory holding projects/")
	write := fs.Bool("write", false, "append the adjustments (default: report only)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cost reconcile: load config: %v\n", err)
		os.Exit(1)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	untilDay := today
	if *until != "" {
		if untilDay, err = time.Parse(time.DateOnly, *until); err != nil || untilDay.After(today) {
			fmt.Fprintf(os.Stderr, "cost reconcile: -until %q must be a date no later than today (UTC)\n", *until)
			os.Exit(2)
		}
	}
	rep, err := reconcileLedger(reconcileOpts{
		SessionStorePath: osutil.ExpandHome(cfg.Session.StorePath),
		ClaudeDir:        *claudeDir,
		CronStorePath:    osutil.ExpandHome(cfg.Cron.StorePath),
		Cost:             cfg.Cost,
		Session:          *sid,
		Until:            untilDay,
		Write:            *write,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cost reconcile: %v\n", err)
		os.Exit(1)
	}
	switch {
	case !*write && len(rep.Planned) > 0:
		fmt.Println("dry-run：未写入。确认上表后加 -write 追加这些 Kind=adjust 条目。")
	case *write && rep.Appended > 0:
		fmt.Println("修正条目落在过去的日期文件里；重启 naozhi 后内存汇总才会包含它们。")
	}
}

// reconcileOpts says what to settle and where the inputs live.
type reconcileOpts struct {
	SessionStorePath string
	ClaudeDir        string
	CronStorePath    string // its run records name the CLI sessions cron ran
	Cost             config.CostConfig
	Session          string    // one CLI session id; "" settles every one
	Until            time.Time // UTC midnight: entries and days from here on are left alone
	Write            bool
}

// reconcileReport is what a reconcile found and (with Write) appended.
type reconcileReport struct {
	Sessions     []sessionSettlement
	Flagged      []flaggedEntry
	Planned      []costledger.Entry
	Unattributed int
	Appended     int
}

// sessionSettlement is one CLI session's figures over its settled days.
type sessionSettlement struct {
	SessionID                     string
	Entries                       int
	Before, After, Transcript     float64
	FlaggedUSD, ResidualUSD       float64
	FlaggedN, ResidualDays        int
	UnpricedDays, AlreadyFlaggedN int
	UndecidedN                    int     // entries whose turn could not be priced or bounded to judge a restore
	HeldDays                      int     // days an unattributed entry shared a key with
	OpenDays                      int     // days whose spend another day's entry books, or none yet
	ForeignDays                   int     // days holding a cron run's turns or lines of unknown origin
	EmptyDays                     int     // days the ledger books spend on and the transcript shows none
	AboveDays                     int     // other days the ledger books more on than the transcript shows
	AboveUSD                      float64 // what the ledger books over the transcript on AboveDays
	TerminalN                     int     // interactive-terminal messages left out
	Skipped                       string  // why nothing was settled; "" when settled
}

// flaggedEntry is a ledger entry that charged a restored cost-state total.
type flaggedEntry struct {
	SessionID string
	Entry     costledger.Entry
	Restored  float64
}

// reconcileLedger settles each CLI session's USD ledger entries against its
// transcripts, in two passes over the days before o.Until. An entry that
// charged the cost-state total its process restored on --resume gets a
// negative Kind=adjust of that total. Then each UTC day whose ledger sum is
// short of the priced transcript usage by more than max($1, 5%) gets the
// difference as one Kind=adjust, unless settleSession holds the day; a day
// booked above its transcript is reported, not lowered. Adjust run ids make a
// second run append nothing. Without o.Write nothing is opened for writing.
func reconcileLedger(o reconcileOpts, out io.Writer) (reconcileReport, error) {
	var rep reconcileReport
	if o.SessionStorePath == "" {
		return rep, fmt.Errorf("session.store_path is empty; the ledger has no home")
	}
	if !o.Cost.IsEnabled() {
		return rep, fmt.Errorf("cost.enabled is false")
	}
	if o.Until.IsZero() {
		o.Until = time.Now().UTC().Truncate(24 * time.Hour)
	}
	ledgerDir := datadir.ForStore(o.SessionStorePath).CostRoot()
	lopts := costledger.Options{RetentionDays: o.Cost.RetentionDays, RollupDays: o.Cost.RollupDays}
	var store *costledger.Store
	if o.Write {
		store = costledger.NewStore(ledgerDir, lopts)
	} else {
		store = costledger.OpenReadOnly(ledgerDir, lopts)
	}
	if !store.Enabled() {
		return rep, fmt.Errorf("open ledger at %s", ledgerDir)
	}
	defer store.Close()

	retention := o.Cost.RetentionDays
	if retention <= 0 {
		retention = costledger.DefaultRetentionDays
	}
	now := time.Now()
	cutoff := now.Add(-time.Duration(retention) * 24 * time.Hour)
	l, err := loadLedgerSessions(store, cutoff, now)
	if err != nil {
		return rep, err
	}
	at := sessionAttribution(o.SessionStorePath)
	rep.Unattributed = l.attribute(at, o.ClaudeDir, o.SessionStorePath)
	l.runs = at.runs

	if o.Session != "" && len(l.bySID[o.Session]) == 0 {
		return rep, fmt.Errorf("no ledger entries attributed to session %s", o.Session)
	}
	order := l.order()
	settled := make([]*sessionInputs, 0, len(order))
	counted := map[string]bool{}
	for _, sid := range order {
		// Every session is read, in order, so -session settles one exactly as
		// a full run would: an earlier session claims what a fork copied.
		in := readSessionInputs(o.ClaudeDir, sid, counted)
		for _, m := range in.marks {
			for _, d := range costStateRows(m) {
				l.rates.Observe(d)
			}
		}
		if o.Session == "" || sid == o.Session {
			settled = append(settled, in)
		}
	}
	cronRuns := cronSessionRuns(o.CronStorePath, now)
	firstDay := l.first.UTC().Truncate(24 * time.Hour)
	if c := cutoff.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour); c.After(firstDay) {
		firstDay = c // the oldest day file may be half swept
	}
	for _, in := range settled {
		st := settleSession(in, l.bySID[in.sid], l, cronRuns[in.sid], firstDay, o.Until, &rep)
		rep.Sessions = append(rep.Sessions, st)
	}

	printReconcile(out, rep, o.Write)
	if o.Write {
		for _, e := range rep.Planned {
			if store.Append(e) {
				rep.Appended++
			}
		}
		store.Close()
		if rep.Appended < len(rep.Planned) {
			return rep, fmt.Errorf("appended %d of %d adjustments; rerun to add the rest", rep.Appended, len(rep.Planned))
		}
		fmt.Fprintf(out, "已追加 %d 条 Kind=adjust\n", rep.Appended)
	}
	return rep, nil
}

// ledgerSessions is the ledger read once: session entries grouped by the CLI
// session they were charged for, the key-days of those no session could be
// named for, every run id seen, and a rate book learned from every
// CLI-priced turn row.
type ledgerSessions struct {
	entries    []costledger.Entry // session-source claude USD entries, not yet attributed
	bySID      map[string][]costledger.Entry
	unassigned map[keyDay]bool
	runIDs     map[string]bool
	rates      *costledger.RateBook
	first      time.Time           // oldest entry of any source
	runs       map[string]timeSpan // run id -> its session-runs record's start and end, either may be zero
}

// keyDay is a session key on a UTC day (YYYY-MM-DD).
type keyDay struct{ key, day string }

func keyDayOf(e costledger.Entry) keyDay {
	return keyDay{e.SessionKey, e.TS.UTC().Format(time.DateOnly)}
}

func loadLedgerSessions(store *costledger.Store, from, now time.Time) (*ledgerSessions, error) {
	l := &ledgerSessions{bySID: map[string][]costledger.Entry{}, unassigned: map[keyDay]bool{},
		runIDs: map[string]bool{}, rates: costledger.NewRateBook()}
	err := store.Scan(costledger.Query{From: from, To: now.Add(24 * time.Hour), AllowFullRange: true}, func(e costledger.Entry) bool {
		if l.first.IsZero() || e.TS.Before(l.first) {
			l.first = e.TS
		}
		if e.RunID != "" {
			l.runIDs[e.RunID] = true
		}
		if e.Unit != costledger.UnitUSD {
			return true
		}
		if e.Kind == costledger.KindTurn {
			for _, m := range e.Models {
				l.rates.Observe(m)
			}
		}
		if e.Source == costledger.SourceSession && e.Backend == "claude" && !sessionkey.IsCronKey(e.SessionKey) {
			l.entries = append(l.entries, e)
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("scan ledger: %w", err)
	}
	return l, nil
}

// attribute names each entry's CLI session: from its own run id (process-end
// partials, unowned results and reconcile adjustments carry it), from the
// session-runs record sharing its run id, from a key that only ever held one
// session, for a turn under a key that held several from their transcripts
// (see soleActiveIn), or, for an entry whose run record names no session,
// from the one naozhi session whose transcript began in that run (see
// soleBornIn). It returns how many entries none of these placed and records
// their key-days.
func (l *ledgerSessions) attribute(at attribution, claudeDir, storePath string) (unattributed int) {
	act, booked := sessionActivity{}, bookedTimes(l.entries, at.chains)
	born := newSessionBirths(claudeDir, storePath, at)
	for _, e := range l.entries {
		sid := runIDSession(e.RunID)
		if sid == "" {
			sid = at.byRun[e.RunID]
		}
		if sid == "" {
			sid = at.byKey[e.SessionKey]
		}
		if c := at.chains[e.SessionKey]; sid == "" && len(c) > 0 && e.Kind == costledger.KindTurn {
			act.read(claudeDir, c)
			sid = act.soleActiveIn(c, booked.before(e.SessionKey, e.TS), e.TS)
		}
		if span := at.runs[e.RunID]; sid == "" && at.unnamed[e.RunID] && !span.from.IsZero() && !span.to.IsZero() {
			sid = born.soleBornIn(span.from.Add(-birthSlack), span.to.Add(birthSlack))
		}
		if !claudefs.IsValidSessionID(sid) {
			unattributed++
			l.unassigned[keyDayOf(e)] = true
			continue
		}
		l.bySID[sid] = append(l.bySID[sid], e)
	}
	return unattributed
}

// runIDSession returns the CLI session an "end:<sid>:…", "unowned:<sid>:…" or
// "reconcile:<sid>:…" run id names.
func runIDSession(runID string) string {
	for _, p := range []string{"end:", "unowned:", reconcilePrefix} {
		if rest, ok := strings.CutPrefix(runID, p); ok {
			sid, _, _ := strings.Cut(rest, ":")
			return sid
		}
	}
	return ""
}

// sessionActivity holds what was read of each session's transcript, so a
// session is read at most once whatever the outcome.
type sessionActivity map[string]sessionTimes

// sessionTimes are the times of a session's messages naozhi may have run,
// ascending; ok is false when its transcript could not be read in full.
type sessionTimes struct {
	times []time.Time
	ok    bool
}

// read adds the sessions of sids not tried yet.
func (a sessionActivity) read(claudeDir string, sids []string) {
	for _, sid := range sids {
		if _, tried := a[sid]; !tried {
			a[sid] = readSessionTimes(claudeDir, sid)
		}
	}
}

// readSessionTimes reads sid's transcript on its own, so a fork keeps the
// lines it copied from its parent.
func readSessionTimes(claudeDir, sid string) sessionTimes {
	path := locateTranscript(claudeDir, sid)
	if path == "" {
		return sessionTimes{}
	}
	u, found, err := claudefs.SessionMessageUsage(filepath.Dir(path), sid, map[string]bool{})
	if err != nil || !found || u.Truncated {
		return sessionTimes{}
	}
	var times []time.Time
	for _, m := range u.Messages {
		if m.Entrypoint != "cli" && m.Entrypoint != "claude-vscode" {
			times = append(times, m.At)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return sessionTimes{times: times, ok: true}
}

// soleActiveIn names the one session of sids with a message in (from, to],
// a turn's window as turnSpan cuts it.
// A session's transcript also holds its turns under other keys, and a fork's
// lines copied from its parent, so two sessions with a message there name
// none; so do a session not read and a window no session has a message in.
func (a sessionActivity) soleActiveIn(sids []string, from, to time.Time) string {
	sole := ""
	for _, sid := range sids {
		st := a[sid]
		if !st.ok {
			return ""
		}
		i := sort.Search(len(st.times), func(i int) bool { return st.times[i].After(from) })
		if i == len(st.times) || st.times[i].After(to) {
			continue
		}
		if sole != "" {
			return ""
		}
		sole = sid
	}
	return sole
}

// keyBookings holds, per key, the ascending times of its entries other than
// adjustments.
type keyBookings map[string][]time.Time

// bookedTimes collects the bookings of the keys in chains.
func bookedTimes(entries []costledger.Entry, chains map[string][]string) keyBookings {
	b := keyBookings{}
	for _, e := range entries {
		if e.Kind != costledger.KindAdjust && len(chains[e.SessionKey]) > 0 {
			b[e.SessionKey] = append(b[e.SessionKey], e.TS)
		}
	}
	for _, ts := range b {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	}
	return b
}

// before returns key's latest booking earlier than t, or the zero time.
func (b keyBookings) before(key string, t time.Time) time.Time {
	ts := b[key]
	if i := sort.Search(len(ts), func(i int) bool { return !ts[i].Before(t) }); i > 0 {
		return ts[i-1]
	}
	return time.Time{}
}

// order lists the sessions oldest first, so a fork's parent claims the
// messages the fork copied.
func (l *ledgerSessions) order() []string {
	out := make([]string, 0, len(l.bySID))
	for sid, es := range l.bySID {
		sort.SliceStable(es, func(i, j int) bool { return es[i].TS.Before(es[j].TS) })
		out = append(out, sid)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := l.bySID[out[i]][0].TS, l.bySID[out[j]][0].TS
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i] < out[j]
	})
	return out
}

// attribution is what the session store and session-runs records say about
// which CLI session ran what.
type attribution struct {
	byRun   map[string]string   // run id -> CLI session
	byKey   map[string]string   // key -> the one CLI session it ever held
	chains  map[string][]string // key -> the sessions it held, sorted, when more than one
	runs    map[string]timeSpan // run id -> its record's start and end, either may be zero
	unnamed map[string]bool     // run ids whose record names no session
}

// sessionAttribution maps run id to CLI session id and to its span from the
// session-runs records, and each session key to the one CLI session it ever
// held. A key whose records and sessions.json chain together name more than
// one session goes to chains instead, with those sessions sorted. A record
// naming no session still gives its run's span, and is marked unnamed.
func sessionAttribution(storePath string) attribution {
	byRun, runs, unnamed := map[string]string{}, map[string]timeSpan{}, map[string]bool{}
	held := map[string]map[string]bool{}
	hold := func(key, sid string) {
		if held[key] == nil {
			held[key] = map[string]bool{}
		}
		held[key][sid] = true
	}
	runlog.WalkRecords(datadir.ForStore(storePath).SessionRunsRoot(), func(string, error) {}, func(rec runlog.Record) {
		var r runhistory.SessionRun
		if json.Unmarshal(rec.Raw, &r) != nil {
			return
		}
		if r.RunID != "" {
			runs[r.RunID] = timeSpan{r.StartedAt, r.EndedAt}
			if r.SessionID == "" {
				unnamed[r.RunID] = true
			} else {
				byRun[r.RunID] = r.SessionID
			}
		}
		if r.SessionKey != "" && r.SessionID != "" {
			hold(r.SessionKey, r.SessionID)
		}
	})
	for key, ids := range session.StoredSessionIDs(storePath) {
		for _, id := range ids {
			hold(key, id)
		}
	}
	byKey, chains := map[string]string{}, map[string][]string{}
	for key, ids := range held {
		if len(ids) == 1 {
			for id := range ids {
				byKey[key] = id
			}
			continue
		}
		for id := range ids {
			chains[key] = append(chains[key], id)
		}
		sort.Strings(chains[key])
	}
	return attribution{byRun: byRun, byKey: byKey, chains: chains, runs: runs, unnamed: unnamed}
}

// cronSessionRuns maps each CLI session a cron run used to the spans of
// those runs; a run with no end yet spans to now.
func cronSessionRuns(cronStorePath string, now time.Time) map[string][]timeSpan {
	out := map[string][]timeSpan{}
	if cronStorePath == "" {
		return out
	}
	runlog.WalkRecords(datadir.ForStore(cronStorePath).RunsRoot(), func(string, error) {}, func(rec runlog.Record) {
		var r cron.CronRun
		if json.Unmarshal(rec.Raw, &r) != nil || r.SessionID == "" || r.StartedAt.IsZero() {
			return
		}
		end := r.EndedAt
		if end.IsZero() {
			end = now
		}
		out[r.SessionID] = append(out[r.SessionID], timeSpan{r.StartedAt, end})
	})
	return out
}

// timeSpan is [from, to].
type timeSpan struct{ from, to time.Time }

// sessionInputs is what a session's transcripts say.
type sessionInputs struct {
	sid     string
	marks   []claudefs.CostStateMark
	usage   claudefs.SessionMessages
	skipped string
}

func readSessionInputs(claudeDir, sid string, counted map[string]bool) *sessionInputs {
	in := &sessionInputs{sid: sid}
	path := locateTranscript(claudeDir, sid)
	if path == "" {
		in.skipped = "未找到 transcript"
		return in
	}
	var err error
	if in.marks, err = claudefs.CostStates(path, sid); err != nil {
		in.skipped = "读 transcript 失败: " + err.Error()
		return in
	}
	u, found, err := claudefs.SessionMessageUsage(filepath.Dir(path), sid, counted)
	switch {
	case err != nil:
		in.skipped = "读 transcript 失败: " + err.Error()
	case !found:
		in.skipped = "未找到 transcript"
	case u.Truncated:
		in.skipped = "子 agent transcript 过多，用量不全"
	}
	in.usage = u
	return in
}

// locateTranscript finds <claudeDir>/projects/*/<sid>.jsonl.
func locateTranscript(claudeDir, sid string) string {
	root := claudefs.ProjectsRoot(claudeDir)
	if root == "" || !claudefs.IsValidSessionID(sid) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", sid+".jsonl"))
	sort.Strings(matches)
	for _, p := range matches {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// costStateRows is a cost-state's per-model usage as ledger rows.
func costStateRows(m claudefs.CostStateMark) []costledger.ModelDelta {
	var models map[string]clievent.ModelUsage
	if len(m.ModelUsage) == 0 || json.Unmarshal(m.ModelUsage, &models) != nil {
		return nil
	}
	inc, _ := costledger.Delta(cliusage.Cumulative(m.TotalCostUSD, models), costledger.Cumulative{})
	sort.Slice(inc.Models, func(i, j int) bool { return inc.Models[i].RawModel < inc.Models[j].RawModel })
	return inc.Models
}

// restoredBy returns the cost-state a process starting before ts restored:
// the last one some later line follows by ts. A cost-state the process wrote
// itself, on its way out, is followed only by later lines. Every later entry
// gets it as a candidate, as a killed process writes none and the next one
// restores the same line again; chargesRestore tells which of them charged it.
func restoredBy(marks []claudefs.CostStateMark, ts time.Time) (claudefs.CostStateMark, bool) {
	var best claudefs.CostStateMark
	found := false
	for _, m := range marks {
		if !m.After.IsZero() && !m.After.After(ts) {
			best, found = m, true
		}
	}
	return best, found
}

// restoreRows negates, row by row of e, the share of m's usage that row
// carried, so a flag lands in e's own model buckets and never takes one
// below what e added to it. An entry without rows gets none.
func restoreRows(e costledger.Entry, m claudefs.CostStateMark) []costledger.ModelDelta {
	left := map[string]*costledger.ModelDelta{}
	for _, d := range costStateRows(m) {
		k := costledger.RateKey(d.Model)
		if left[k] == nil {
			left[k] = &costledger.ModelDelta{}
		}
		addRow(left[k], d)
	}
	var out []costledger.ModelDelta
	for _, r := range e.Models {
		l := left[costledger.RateKey(r.Model)]
		if l == nil {
			continue
		}
		take := r
		take.CostUSD = min(r.CostUSD, l.CostUSD)
		take.Input, take.Output = min(r.Input, l.Input), min(r.Output, l.Output)
		take.CacheRead, take.CacheWrite = min(r.CacheRead, l.CacheRead), min(r.CacheWrite, l.CacheWrite)
		take.Thinking, take.WebSearch = min(r.Thinking, l.Thinking), min(r.WebSearch, l.WebSearch)
		addRow(l, negate(take))
		if take.CostUSD > 0 || take.Tokens != (costledger.Tokens{}) {
			out = append(out, negate(take))
		}
	}
	return out
}

// addRow adds d's cost and tokens to r.
func addRow(r *costledger.ModelDelta, d costledger.ModelDelta) {
	r.CostUSD += d.CostUSD
	r.Input += d.Input
	r.Output += d.Output
	r.CacheRead += d.CacheRead
	r.CacheWrite += d.CacheWrite
	r.Thinking += d.Thinking
	r.WebSearch += d.WebSearch
}

// turnUsage is the transcript usage of the turn an entry booked; usd is set
// when priced.
type turnUsage struct {
	tokens int64
	usd    float64
	priced bool
}

// turnWindow sums the messages with from < At <= to, leaving out
// interactive-terminal ones as classifyMessages does, and prices them.
func turnWindow(msgs []claudefs.MessageUsage, from, to time.Time, rates *costledger.RateBook) turnUsage {
	var in []claudefs.MessageUsage
	var u turnUsage
	for _, m := range msgs {
		if !m.At.After(from) || m.At.After(to) || m.Entrypoint == "cli" || m.Entrypoint == "claude-vscode" {
			continue
		}
		in = append(in, m)
		u.tokens += m.Input + m.Output + m.CacheRead + m.CacheWrite
	}
	u.priced = true
	for _, rows := range claudefs.DayTotals(in) {
		p, ok := priceDay(rows, rates)
		u.usd, u.priced = u.usd+p.usd, u.priced && ok
	}
	return u
}

// turnSpan is the window (from, to] of entries[i]'s turn, prev being where
// the turn before it ended, and next where the next one starts. A turn entry
// is booked at its end: naozhi stamps it on the result, after the CLI stamped
// that turn's lines on the same clock, so a later line is the next turn's.
// A backfill is booked at its run's start and spans to the run's recorded
// end, else up to the next entry, which may take in that entry's turn too,
// so the next turn then starts back at the backfill's start; to is zero when
// nothing bounds it.
func turnSpan(entries []costledger.Entry, i int, prev time.Time, runs map[string]timeSpan) (from, to, next time.Time) {
	e := entries[i]
	if e.Kind != costledger.KindBackfill {
		return prev, e.TS, e.TS
	}
	if end := runs[e.RunID].to; end.After(e.TS) {
		return e.TS, end, end
	}
	for _, n := range entries[i+1:] {
		if n.Kind != costledger.KindAdjust {
			return e.TS, n.TS, e.TS
		}
	}
	return e.TS, time.Time{}, e.TS
}

// mayChargeRestore reports whether e is large enough to have charged m's
// total, which must be large enough to tell.
func mayChargeRestore(e costledger.Entry, m claudefs.CostStateMark) bool {
	return m.TotalCostUSD >= restoreFloor && e.Amount >= restoreRatio*m.TotalCostUSD
}

// chargesRestore reports whether e, which mayChargeRestore of m, charged m's
// total on top of turn, the usage of its own turn: a turn differenced from a
// baseline of 0 exceeds it by about r, a correctly baselined one by about
// nothing. Tokens are compared when e and m both have them, priced USD
// otherwise; decided is false when that needs a price turn lacks.
func chargesRestore(e costledger.Entry, m claudefs.CostStateMark, turn turnUsage) (charged, decided bool) {
	if et, rt := tokenSum(e.Models), tokenSum(costStateRows(m)); et > 0 && rt > 0 {
		return float64(et-turn.tokens) >= restoreExcess*float64(rt), true
	}
	if !turn.priced {
		return false, false
	}
	return e.Amount-turn.usd >= restoreExcess*m.TotalCostUSD, true
}

func tokenSum(rows []costledger.ModelDelta) int64 {
	var n int64
	for _, r := range rows {
		n += r.Input + r.Output + r.CacheRead + r.CacheWrite
	}
	return n
}

// settleSession plans one session's adjustments into rep and returns its row.
// Terminal messages are not naozhi's spend and are left out. A day gets no
// residual when any of its spend may be booked elsewhere or not be naozhi's:
// an entry no session could be named for shares a key with this session's
// entries that day; a message has no entry of the session after it that day
// (the entry that books it, if any, is on another day, held too) or predates
// the run of the session's first entry; a cron run of the session touched it;
// or a message's origin is unknown. A residual only raises a day: the CLI also
// bills requests its transcript never logs (cancels, background calls, output
// counted mid-stream). A day the entries net below zero on is raised to zero.
func settleSession(in *sessionInputs, entries []costledger.Entry, l *ledgerSessions, cronRuns []timeSpan, firstDay, until time.Time, rep *reconcileReport) sessionSettlement {
	st := sessionSettlement{SessionID: in.sid, Entries: len(entries), Skipped: in.skipped}
	settles := func(t time.Time) bool { return !t.Before(firstDay) && t.Before(until) }
	ledger := map[string]*dayFigures{}
	held := map[string]bool{}
	day := func(d string) *dayFigures {
		if ledger[d] == nil {
			ledger[d] = &dayFigures{models: map[string]*costledger.ModelDelta{}}
		}
		return ledger[d]
	}
	for _, e := range entries {
		if settles(e.TS) {
			kd := keyDayOf(e)
			day(kd.day).add(e)
			held[kd.day] = held[kd.day] || l.unassigned[kd]
			st.Before += e.Amount
		}
	}
	if in.skipped != "" {
		st.After = st.Before
		return st
	}

	var prev time.Time // where the next entry's turn starts
	for i, e := range entries {
		if e.Kind == costledger.KindAdjust {
			continue
		}
		var from, to time.Time
		from, to, prev = turnSpan(entries, i, prev, l.runs)
		if !settles(e.TS) || e.RunID == "" || (e.Kind != costledger.KindTurn && e.Kind != costledger.KindBackfill) {
			continue
		}
		m, ok := restoredBy(in.marks, e.TS)
		if !ok || !mayChargeRestore(e, m) {
			continue
		}
		if to.IsZero() {
			st.UndecidedN++
			continue
		}
		// The turn starts no earlier than the cost-state: what the process
		// that wrote it ran after the previous entry is in r.
		if m.Before.After(from) {
			from = m.Before
		}
		charged, decided := chargesRestore(e, m, turnWindow(in.usage.Messages, from, to, l.rates))
		if !decided {
			st.UndecidedN++
		}
		if !charged {
			continue
		}
		runID := reconcilePrefix + in.sid + ":run:" + e.RunID
		if l.runIDs[runID] {
			st.AlreadyFlaggedN++
			continue
		}
		adj := adjustOf(e, runID, -m.TotalCostUSD)
		adj.Models = restoreRows(e, m)
		rep.Flagged = append(rep.Flagged, flaggedEntry{SessionID: in.sid, Entry: e, Restored: m.TotalCostUSD})
		rep.Planned = append(rep.Planned, adj)
		day(e.TS.UTC().Format(time.DateOnly)).add(adj)
		st.FlaggedN++
		st.FlaggedUSD += adj.Amount
	}

	usage, open, foreign := classifyMessages(in.usage.Messages, entries, l.runs, cronRuns, &st)
	days := make([]string, 0, len(ledger)+len(usage))
	for d := range ledger {
		days = append(days, d)
	}
	for d := range usage {
		if ledger[d] == nil {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	for _, d := range days {
		start, _ := time.Parse(time.DateOnly, d)
		if !settles(start) {
			continue
		}
		t, ok := priceDay(usage[d], l.rates)
		if !ok {
			st.UnpricedDays++
			continue
		}
		st.Transcript += t.usd
		lf := day(d)
		diff := t.usd - lf.usd
		if math.Abs(diff) <= max(residualFloor, residualShare*t.usd) {
			continue
		}
		switch {
		case held[d]:
			st.HeldDays++
			continue
		case foreign[d]:
			st.ForeignDays++
			continue
		case open[d]:
			st.OpenDays++
			continue
		case len(usage[d]) == 0 && lf.usd > 0:
			st.EmptyDays++
			continue
		case diff < 0:
			st.AboveDays++
			st.AboveUSD -= diff
			continue
		}
		adj := adjustOf(lastBefore(entries, start.Add(24*time.Hour)), reconcilePrefix+in.sid+":day:"+d, diff)
		adj.TS, adj.Basis = start.Add(12*time.Hour), t.basis
		adj.Models = modelResiduals(t.models, lf.models)
		rep.Planned = append(rep.Planned, adj)
		st.ResidualDays++
		st.ResidualUSD += diff
	}
	st.After = st.Before + st.FlaggedUSD + st.ResidualUSD
	return st
}

// classifyMessages sums the messages naozhi may have run per UTC day and
// marks the days settleSession holds as open or foreign (see there).
// entries are time-ordered; a first entry with no run start known holds
// every message before it.
func classifyMessages(msgs []claudefs.MessageUsage, entries []costledger.Entry, runs map[string]timeSpan,
	cronRuns []timeSpan, st *sessionSettlement) (usage map[string][]claudefs.ModelTokens, open, foreign map[string]bool) {
	var booked []time.Time
	var since time.Time
	for _, e := range entries {
		if e.Kind == costledger.KindAdjust {
			continue
		}
		if booked == nil {
			since = e.TS
			if t := runs[e.RunID].from; !t.IsZero() && t.Before(e.TS) {
				since = t
			}
		}
		booked = append(booked, e.TS)
	}
	open, foreign = map[string]bool{}, map[string]bool{}
	kept := make([]claudefs.MessageUsage, 0, len(msgs))
	for _, m := range msgs {
		d := m.At.UTC().Format(time.DateOnly)
		switch m.Entrypoint {
		case "cli", "claude-vscode":
			st.TerminalN++
			continue
		case "sdk-cli":
		default:
			foreign[d] = true
		}
		kept = append(kept, m)
		if m.At.Before(since) {
			open[d] = true
		}
		i := sort.Search(len(booked), func(i int) bool { return !booked[i].Before(m.At) })
		if i < len(booked) && booked[i].Before(m.At.UTC().Truncate(24*time.Hour).Add(24*time.Hour)) {
			continue
		}
		open[d] = true
		if i < len(booked) {
			open[booked[i].UTC().Format(time.DateOnly)] = true
		}
	}
	for _, r := range cronRuns {
		for t := r.from.UTC().Truncate(24 * time.Hour); !t.After(r.to); t = t.Add(24 * time.Hour) {
			foreign[t.Format(time.DateOnly)] = true
		}
	}
	return claudefs.DayTotals(kept), open, foreign
}

// adjustOf is a Kind=adjust entry booked like e (same time, key, workspace).
func adjustOf(e costledger.Entry, runID string, amount float64) costledger.Entry {
	return costledger.Entry{TS: e.TS, Source: costledger.SourceSession, Kind: costledger.KindAdjust,
		SessionKey: e.SessionKey, RunID: runID, Workspace: e.Workspace, Backend: "claude",
		Unit: costledger.UnitUSD, Amount: amount, Basis: e.Basis}
}

// lastBefore returns the last of the (time-ordered) entries before t, or the
// first entry when none is. A day's residual is booked under that entry's
// key alone, even when the day's spend ran under several.
func lastBefore(entries []costledger.Entry, t time.Time) costledger.Entry {
	last := entries[0]
	for _, e := range entries {
		if !e.TS.Before(t) {
			break
		}
		last = e
	}
	return last
}

func negate(d costledger.ModelDelta) costledger.ModelDelta {
	d.CostUSD = -d.CostUSD
	d.Input, d.Output, d.CacheRead, d.CacheWrite = -d.Input, -d.Output, -d.CacheRead, -d.CacheWrite
	d.Thinking, d.WebSearch = -d.Thinking, -d.WebSearch
	return d
}

// dayFigures is one day's ledger sum and per-model rows (keyed by RateKey).
type dayFigures struct {
	usd    float64
	models map[string]*costledger.ModelDelta
}

func (f *dayFigures) add(e costledger.Entry) {
	f.usd += e.Amount
	for _, m := range e.Models {
		k := costledger.RateKey(m.Model)
		r := f.models[k]
		if r == nil {
			r = &costledger.ModelDelta{Model: m.Model}
			f.models[k] = r
		}
		addRow(r, m)
	}
}

// pricedDay is a day of transcript usage at learned rates.
type pricedDay struct {
	usd    float64
	basis  costledger.Basis
	models map[string]*costledger.ModelDelta
}

// priceDay prices a day's transcript rows; ok is false when a model has no
// learned rate, as the day's figure would then be short.
func priceDay(rows []claudefs.ModelTokens, rates *costledger.RateBook) (pricedDay, bool) {
	p := pricedDay{models: map[string]*costledger.ModelDelta{}}
	for _, r := range rows {
		tok := costledger.Tokens{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}
		usd, basis, ok := rates.Estimate(r.Model, tok)
		if !ok {
			return pricedDay{}, false
		}
		p.usd += usd
		p.basis = costledger.WorseBasis(p.basis, basis)
		k := costledger.RateKey(r.Model)
		m := p.models[k]
		if m == nil {
			m = &costledger.ModelDelta{Model: costledger.CanonicalModel("", r.Model), RawModel: r.Model}
			p.models[k] = m
		}
		m.CostUSD += usd
		m.Tokens = costledger.Tokens{Input: m.Input + tok.Input, Output: m.Output + tok.Output,
			CacheRead: m.CacheRead + tok.CacheRead, CacheWrite: m.CacheWrite + tok.CacheWrite}
	}
	return p, true
}

// modelResiduals is transcript minus ledger per model, for the drill-down;
// rows with nothing left are dropped. Thinking and web-search counts are not
// in the transcript and are left as booked.
func modelResiduals(transcript, ledger map[string]*costledger.ModelDelta) []costledger.ModelDelta {
	keys := make([]string, 0, len(transcript)+len(ledger))
	for k := range transcript {
		keys = append(keys, k)
	}
	for k := range ledger {
		if transcript[k] == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []costledger.ModelDelta
	for _, k := range keys {
		t, l := transcript[k], ledger[k]
		var d costledger.ModelDelta
		switch {
		case t != nil:
			d.Model, d.RawModel = t.Model, t.RawModel
		default:
			d.Model = l.Model
		}
		if t == nil {
			t = &costledger.ModelDelta{}
		}
		if l == nil {
			l = &costledger.ModelDelta{}
		}
		d.CostUSD = t.CostUSD - l.CostUSD
		d.Input, d.Output = t.Input-l.Input, t.Output-l.Output
		d.CacheRead, d.CacheWrite = t.CacheRead-l.CacheRead, t.CacheWrite-l.CacheWrite
		if math.Abs(d.CostUSD) >= 0.005 || d.Tokens != (costledger.Tokens{}) {
			out = append(out, d)
		}
	}
	return out
}

func printReconcile(out io.Writer, rep reconcileReport, write bool) {
	fmt.Fprintf(out, "%-8s %5s %11s %11s %11s %11s  %s\n", "session", "条目", "账本(前)", "账本(后)", "transcript", "修正", "说明")
	var before, after, transcript float64
	for _, s := range rep.Sessions {
		note := s.Skipped
		if note == "" {
			note = fmt.Sprintf("标记 %d 条 %+.2f；残差 %d 天 %+.2f", s.FlaggedN, s.FlaggedUSD, s.ResidualDays, s.ResidualUSD)
			if s.AlreadyFlaggedN > 0 {
				note += fmt.Sprintf("；已修正过 %d 条", s.AlreadyFlaggedN)
			}
			if s.HeldDays > 0 {
				note += fmt.Sprintf("；%d 天有同 key 的未归属条目，残差未记", s.HeldDays)
			}
			if s.OpenDays > 0 {
				note += fmt.Sprintf("；%d 天的用量记在别的日子或尚未记账（跨零点/-until 的轮次、首条记账前的历史），残差未记", s.OpenDays)
			}
			if s.ForeignDays > 0 {
				note += fmt.Sprintf("；%d 天含 cron run 或来源不明的用量，残差未记", s.ForeignDays)
			}
			if s.EmptyDays > 0 {
				note += fmt.Sprintf("；%d 天 transcript 无用量而账本有，残差未记", s.EmptyDays)
			}
			if s.AboveDays > 0 {
				note += fmt.Sprintf("；%d 天账本高于 transcript 共 %.2f（transcript 不记 CLI 的全部请求：取消/后台请求、流式中途的 output 计数），未下调", s.AboveDays, s.AboveUSD)
			}
			if s.TerminalN > 0 {
				note += fmt.Sprintf("；%d 条终端交互消息不计入", s.TerminalN)
			}
			if s.UnpricedDays > 0 {
				note += fmt.Sprintf("；%d 天含未学到单价的模型，未比对", s.UnpricedDays)
			}
			if s.UndecidedN > 0 {
				note += fmt.Sprintf("；%d 条无法判定是否计入恢复额（无单价或回填 run 无结束时间），未标记", s.UndecidedN)
			}
		}
		fmt.Fprintf(out, "%-8.8s %5d %11.2f %11.2f %11.2f %+11.2f  %s\n", s.SessionID, s.Entries, s.Before, s.After, s.Transcript, s.After-s.Before, note)
		before, after, transcript = before+s.Before, after+s.After, transcript+s.Transcript
	}
	fmt.Fprintf(out, "%-8s %5s %11.2f %11.2f %11.2f %+11.2f\n", "合计", "", before, after, transcript, after-before)
	if rep.Unattributed > 0 {
		fmt.Fprintf(out, "%d 条会话条目归不到 CLI session（无 run 记录，key 没对应过 session，或对应过多个而按 transcript 时间分不出；run 记录没写 session 的首轮，其间起头的已知 session 不止一个、没有或与别的首轮重叠），未参与对账；同 key 同日的残差不记\n", rep.Unattributed)
	}
	if len(rep.Flagged) > 0 {
		fmt.Fprintln(out, "\n计入了 --resume 恢复总额的条目：")
		for _, f := range rep.Flagged {
			fmt.Fprintf(out, "  %s  %-8.8s  %s  run=%s  记 %.2f，恢复额 %.2f\n",
				f.Entry.TS.UTC().Format(time.RFC3339), f.SessionID, f.Entry.SessionKey, f.Entry.RunID, f.Entry.Amount, f.Restored)
		}
	}
	mode := "将追加（dry-run）"
	if write {
		mode = "追加"
	}
	fmt.Fprintf(out, "\n%s %d 条 Kind=adjust\n", mode, len(rep.Planned))
}
