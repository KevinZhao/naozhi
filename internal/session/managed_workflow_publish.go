package session

// managed_workflow_publish.go — the board's merge and publication (RFC §5.8
// "合并与发布"): live Set and retained entries merged per field, bounded,
// stamped with wire versions, published with the summaries the session
// snapshot carries; and the Refs sessions.json keeps (R0 reads them back).

import (
	"cmp"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// agentEqual compares rows ignoring Rev; tests swap it to count comparisons.
var agentEqual = workflow.AgentEqualIgnoringRev

// publishLocked merges, bounds, versions and publishes the board's entries.
// notify false publishes without a sessions_update or subscriber wake.
func (b *WorkflowBoard) publishLocked(notify bool) {
	var live []*workflow.Workflow
	if b.set != nil {
		live = b.set.Workflows
	}
	held := make(map[string]bool, len(live))
	for _, w := range live {
		held[w.TaskID] = true
	}
	// Entries the Tracker evicted (its terminal LRU) stay on the board.
	for id, st := range b.last {
		if st.live && !held[id] {
			b.retained[id] = &retainedEntry{wf: staleEntry(st.unresolved()), from: b.proc}
		}
	}
	merged := make([]*workflow.Workflow, 0, len(live)+len(b.retained))
	for _, w := range live {
		merged = append(merged, mergeEntry(w, b.retained[w.TaskID], b.bindAt))
	}
	for id, r := range b.retained {
		if !held[id] {
			merged = append(merged, r.wf)
		}
	}
	merged = b.capLocked(merged, held)

	ver := b.ver + 1
	bumped, structural := false, false
	pubs := make([]*workflow.Workflow, 0, len(merged))
	next := make(map[string]*wireState, len(merged))
	for _, m := range merged {
		st := b.last[m.TaskID]
		b.registerResolveLocked(m)
		p := *m
		p.RunDir = ""
		if rs := b.resolve[m.TaskID]; rs != nil && rs.ok {
			p.RunDir = rs.run.RunDir
			if rs.run.SessionID != "" {
				p.SessionID, p.Src.SessionID = rs.run.SessionID, workflow.SessionFromRunDir
			}
		}
		rows, changed := stampRows(m.Agents, st, workflow.IsTerminal(m.Status), ver)
		p.Agents = rows
		switch {
		case st == nil:
			structural = true
			p.Version = ver
		case changed || !headerEqual(&p, st.pub):
			structural = structural || p.Status != st.pub.Status || p.RunID != st.pub.RunID
			p.Version = ver
		default:
			p.Version = st.pub.Version
		}
		bumped = bumped || p.Version == ver
		pubs = append(pubs, &p)
		next[p.TaskID] = &wireState{pub: &p, src: m.Agents, live: held[p.TaskID], sid: m.SessionID, sidSrc: m.Src.SessionID}
	}
	if bumped {
		b.ver = ver
	}
	b.last = next

	pub := workflow.NewPublished(b.epoch, pubs, b.cur.Load())
	b.cur.Store(pub)
	sums, running, lastObs := summarize(pub)
	b.summaries.Store(&sums)
	b.running.Store(running)
	b.lastObs.Store(lastObs)
	if !notify || !(bumped || structural) {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	if structural {
		b.notify.arm(notifyStructural)
	} else {
		b.notify.arm(notifyCount)
	}
}

// mergeEntry merges the board's own entry for a task into the live one, per
// field (RFC §5.8 step 3): state, rows and totals are the process's; run id,
// end time and launch dir fill in when the process lacks them; name, start
// and session id go to the higher source grade. A replay seed carries no
// observation time, so it is taken as observed when the CLI was bound.
func mergeEntry(live *workflow.Workflow, ret *retainedEntry, bindAt int64) *workflow.Workflow {
	if ret == nil && live.LastObservedAt != 0 {
		return live
	}
	m := *live
	var observed int64
	if ret != nil {
		r := ret.wf
		m.RunID = cmp.Or(m.RunID, r.RunID)
		m.LaunchTranscriptDir = cmp.Or(m.LaunchTranscriptDir, r.LaunchTranscriptDir)
		if m.EndedAt == 0 && workflow.IsTerminal(m.Status) {
			m.EndedAt = r.EndedAt
		}
		if r.Src.Name > m.Src.Name {
			m.Name, m.Src.Name = r.Name, r.Src.Name
		}
		if r.Src.StartedAt > m.Src.StartedAt {
			m.StartedAt, m.Src.StartedAt = r.StartedAt, r.Src.StartedAt
		}
		if r.Src.SessionID > m.Src.SessionID {
			m.SessionID, m.Src.SessionID = r.SessionID, r.Src.SessionID
		}
		observed = r.LastObservedAt
	}
	if m.LastObservedAt == 0 {
		m.LastObservedAt = max(observed, bindAt)
	}
	return &m
}

// capLocked bounds the board: at most workflowBoardMaxUnsettled unsettled
// and workflowBoardMaxTerminal terminal entries. Only retained entries are
// dropped — unknown first, then stale or restored ones, then the rest, the
// longest unobserved (for terminal: the earliest ended) first; dropping one
// is no evidence it ended. Live entries are the Tracker's to bound.
func (b *WorkflowBoard) capLocked(wfs []*workflow.Workflow, held map[string]bool) []*workflow.Workflow {
	var unsettled, terminal int
	var spareU, spareT []*workflow.Workflow
	for _, w := range wfs {
		if workflow.IsUnsettled(w.Status) {
			unsettled++
			if !held[w.TaskID] {
				spareU = append(spareU, w)
			}
		} else {
			terminal++
			if !held[w.TaskID] {
				spareT = append(spareT, w)
			}
		}
	}
	drop := map[string]bool{}
	if n := unsettled - workflowBoardMaxUnsettled; n > 0 {
		slices.SortFunc(spareU, func(x, y *workflow.Workflow) int {
			return cmp.Or(cmp.Compare(dropClass(x), dropClass(y)), cmp.Compare(x.LastObservedAt, y.LastObservedAt), cmp.Compare(x.TaskID, y.TaskID))
		})
		for _, w := range spareU[:min(n, len(spareU))] {
			drop[w.TaskID] = true
		}
	}
	if n := terminal - workflowBoardMaxTerminal; n > 0 {
		slices.SortFunc(spareT, func(x, y *workflow.Workflow) int {
			return cmp.Or(cmp.Compare(x.EndedAt, y.EndedAt), cmp.Compare(x.TaskID, y.TaskID))
		})
		for _, w := range spareT[:min(n, len(spareT))] {
			drop[w.TaskID] = true
		}
	}
	if len(drop) == 0 {
		return wfs
	}
	for id := range drop {
		delete(b.retained, id)
		delete(b.last, id)
		delete(b.resolve, id)
	}
	return slices.DeleteFunc(wfs, func(w *workflow.Workflow) bool { return drop[w.TaskID] })
}

// dropClass orders unsettled retained entries for capLocked.
func dropClass(w *workflow.Workflow) int {
	switch {
	case w.Status == workflow.StatusUnknown:
		return 0
	case w.Degraded == workflow.DegradedSnapshotStale || w.Source == workflow.SourceRef:
		return 1
	}
	return 2
}

// stampRows gives src's rows their wire Rev against the task's last
// publication: an unchanged row keeps its Rev, a changed or new one gets
// ver. Rows the last publication had and src lacks are kept (an index never
// disappears within an epoch); once the workflow is terminal a kept queued
// or running row stops. A src shared with the last publication is not
// compared row by row.
func stampRows(src []workflow.Agent, st *wireState, terminal bool, ver uint64) ([]workflow.Agent, bool) {
	var prev []workflow.Agent
	if st != nil {
		prev = st.pub.Agents
		if (sameRows(src, st.src) || sameRows(src, prev)) && (!terminal || workflow.IsTerminal(st.pub.Status)) {
			return prev, false
		}
	}
	out := make([]workflow.Agent, 0, min(len(src)+len(prev), workflow.MaxAgents))
	changed := false
	i, j := 0, 0
	for (i < len(src) || j < len(prev)) && len(out) < workflow.MaxAgents {
		switch {
		case j == len(prev) || (i < len(src) && src[i].Index < prev[j].Index):
			a := src[i]
			a.Rev, changed = ver, true
			out = append(out, a)
			i++
		case i == len(src) || prev[j].Index < src[i].Index:
			a := prev[j]
			if terminal && (a.State == workflow.AgentQueued || a.State == workflow.AgentRunning) {
				a.State, a.Rev, changed = workflow.AgentStopped, ver, true
			}
			out = append(out, a)
			j++
		default:
			a := src[i]
			if agentEqual(&a, &prev[j]) {
				a.Rev = prev[j].Rev
			} else {
				a.Rev, changed = ver, true
			}
			out = append(out, a)
			i, j = i+1, j+1
		}
	}
	return out, changed || j < len(prev)
}

// sameRows reports whether a and b are one slice: copy-on-write sharing.
func sameRows(a, b []workflow.Agent) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// headerEqual compares the wire header of two entries: every field of
// workflow.WireView but Agents and Version (a test pins the field count).
func headerEqual(a, b *workflow.Workflow) bool {
	return a.TaskID == b.TaskID && a.RunID == b.RunID && a.Name == b.Name &&
		a.Description == b.Description && a.Current == b.Current &&
		a.Status == b.Status && a.RawStatus == b.RawStatus &&
		a.StartedAt == b.StartedAt && a.EndedAt == b.EndedAt && a.LastObservedAt == b.LastObservedAt &&
		a.Tokens == b.Tokens && a.ToolCalls == b.ToolCalls && a.DurationMS == b.DurationMS &&
		a.Counts == b.Counts && slices.Equal(a.Phases, b.Phases) && a.AgentsCapped == b.AgentsCapped &&
		a.NotifySummary == b.NotifySummary && a.Source == b.Source && a.Degraded == b.Degraded
}

// summarize derives the snapshot summaries (every unsettled entry plus the
// latest workflowSummaryTerminal terminal ones), whether any entry runs, and
// the latest observation of the running entries (of all when none runs).
func summarize(p *workflow.Published) ([]workflow.Summary, bool, int64) {
	var sums []workflow.Summary
	var lastRun, lastAny int64
	running, terminal := false, 0
	for _, w := range p.Workflows {
		lastAny = max(lastAny, w.LastObservedAt)
		if workflow.IsRunning(w.Status) {
			running, lastRun = true, max(lastRun, w.LastObservedAt)
		}
		if !workflow.IsUnsettled(w.Status) {
			if terminal == workflowSummaryTerminal {
				continue
			}
			terminal++
		}
		sums = append(sums, workflow.Summary{
			TaskID: w.TaskID, Name: w.Name, Status: w.Status, Counts: w.Counts, Tokens: w.Tokens,
			StartedAt: w.StartedAt, EndedAt: w.EndedAt, CurrentPhase: w.Current,
			Epoch: p.Epoch, Version: w.Version,
		})
	}
	if running {
		return sums, true, lastRun
	}
	return sums, false, lastAny
}

// refs is what sessions.json keeps of the board: the header of each
// published entry, at most workflowBoardMaxUnsettled unsettled ones (the
// latest observed when the live process alone holds more) and the terminal
// ones. An unknown entry unobserved for workflowPinMax is left out, so a
// run nobody claims stops coming back.
func (b *WorkflowBoard) refs(now time.Time) []workflow.Ref {
	p := b.Published()
	if p == nil || len(p.Workflows) == 0 {
		return nil
	}
	cutoff := now.Add(-workflowPinMax).UnixMilli()
	keep := slices.DeleteFunc(slices.Clone(p.Workflows), func(w *workflow.Workflow) bool {
		return w.Status == workflow.StatusUnknown && w.LastObservedAt < cutoff
	})
	var unsettled []*workflow.Workflow
	for _, w := range keep {
		if workflow.IsUnsettled(w.Status) {
			unsettled = append(unsettled, w)
		}
	}
	if len(unsettled) > workflowBoardMaxUnsettled {
		slices.SortStableFunc(unsettled, func(x, y *workflow.Workflow) int { return cmp.Compare(y.LastObservedAt, x.LastObservedAt) })
		out := map[string]bool{}
		for _, w := range unsettled[workflowBoardMaxUnsettled:] {
			out[w.TaskID] = true
		}
		keep = slices.DeleteFunc(keep, func(w *workflow.Workflow) bool { return out[w.TaskID] })
	}
	if len(keep) == 0 {
		return nil
	}
	refs := make([]workflow.Ref, len(keep))
	for i, w := range keep {
		refs[i] = workflow.Ref{
			TaskID: w.TaskID, RunID: w.RunID, Name: w.Name, SessionID: w.SessionID, Status: w.Status,
			StartedAt: w.StartedAt, EndedAt: w.EndedAt, LastObservedAt: w.LastObservedAt,
			Counts: w.Counts, Tokens: w.Tokens,
		}
	}
	return refs
}

// workflowTaskIDRe is the shape of a CC task id.
var workflowTaskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// restore is R0 (RFC §5.9): sessions.json's Refs become retained entries,
// running ones snapshot_stale until a Tracker reports them. The file is
// hand-editable: a Ref with a malformed task id or status, or a non-empty
// session or run id that fails its check, is dropped; an empty one is kept
// (not known yet). Nothing is announced: this is the state on disk.
func (b *WorkflowBoard) restore(key string, refs []workflow.Ref, workspace string, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.workspace, b.procGone = workspace, now.UnixMilli()
	for _, ref := range refs {
		w, ok := restoredWorkflow(ref, now)
		if !ok {
			slog.Warn("dropping invalid persisted workflow", "key", osutil.SanitizeForLog(key, 128))
			continue
		}
		b.retained[w.TaskID] = &retainedEntry{wf: w}
	}
	b.publishLocked(false)
}

func restoredWorkflow(ref workflow.Ref, now time.Time) (*workflow.Workflow, bool) {
	known := workflow.IsUnsettled(ref.Status) || workflow.IsTerminal(ref.Status)
	if !known || !workflowTaskIDRe.MatchString(ref.TaskID) ||
		(ref.SessionID != "" && !claudefs.IsValidSessionID(ref.SessionID)) ||
		(ref.RunID != "" && !claudefs.IsValidWorkflowRunID(ref.RunID)) {
		return nil, false
	}
	w := &workflow.Workflow{
		TaskID: ref.TaskID, RunID: ref.RunID, SessionID: ref.SessionID, Status: ref.Status,
		Name:      textutil.TruncateRunes(textutil.RedactSecrets(ref.Name), workflowNameRunes),
		StartedAt: ref.StartedAt, EndedAt: ref.EndedAt, LastObservedAt: ref.LastObservedAt,
		Counts: ref.Counts, Tokens: ref.Tokens, Source: workflow.SourceRef,
	}
	if w.LastObservedAt == 0 {
		w.LastObservedAt = now.UnixMilli()
	}
	if workflow.IsRunning(w.Status) {
		w.Degraded = workflow.DegradedSnapshotStale
	}
	if w.Name != "" {
		w.Src.Name = workflow.NameFromRef
	}
	if w.StartedAt > 0 {
		w.Src.StartedAt = workflow.StartedFromRef
	}
	if w.SessionID != "" {
		w.Src.SessionID = workflow.SessionFromRef
	}
	return w, true
}

// workflowNameRunes is the Tracker's cap on a workflow name.
const workflowNameRunes = 120
