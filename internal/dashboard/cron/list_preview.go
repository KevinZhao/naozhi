package cron

import (
	"fmt"
	"net/http"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// GET /api/cron — list all cron jobs (unscoped, admin view). `?compact=1` clips
// Prompt to compactPromptPrefixBytes and stamps prompt_truncated; the default
// keeps the full prompt for out-of-tree consumers (#494).
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	if h.deps.Scheduler == nil {
		// Explicit empty slice (not nil) so json.Marshal emits `{"jobs":[]}`.
		httputil.WriteJSON(w, cronListResp{Jobs: []cronJobView{}})
		return
	}

	compact := r.URL.Query().Get("compact") == "1"

	jobs := h.deps.Scheduler.ListAllJobsWithNextRun()
	// Capture once; each job would otherwise pay time.Now() + an atomic load.
	now := time.Now()
	startedAt := h.deps.Scheduler.StartedAt()

	// Pre-fetch RecentRuns with bounded parallelism so the 1 Hz poll does not
	// serialise on the per-job recentCacheEntry.mu chain (#525).
	recentByIdx := h.batchRecentRuns(jobs, recentRunsPerJob)

	// One backing array for every job's RecentRuns view; per-job sub-slices
	// encode as independent JSON arrays, so N allocations become one (#1119).
	totalRecent := 0
	for _, r := range recentByIdx {
		totalRecent += len(r)
	}
	var recentBacking []cronRunSummaryView
	if totalRecent > 0 {
		recentBacking = make([]cronRunSummaryView, totalRecent)
	}
	recentBackingNext := 0

	views := make([]cronJobView, 0, len(jobs))
	for idx, entry := range jobs {
		j := entry.Job
		// compact mode clips Prompt and flags prompt_truncated so the dashboard
		// refetches the full body before opening the editor (#494).
		prompt := j.Prompt
		truncated := false
		if compact {
			prompt, truncated = truncatePromptUTF8(j.Prompt, compactPromptPrefixBytes)
		}
		v := cronJobView{
			ID:              j.ID,
			Schedule:        j.Schedule,
			Prompt:          prompt,
			PromptTruncated: truncated,
			Title:           j.Title,
			Platform:        j.Platform,
			ChatID:          maskNotifyChatID(j.ChatID),
			CreatedBy:       j.CreatedBy,
			CreatedAt:       j.CreatedAt.UnixMilli(),
			Paused:          j.Paused,
			WorkDir:         j.WorkDir,
			NotifyPlatform:  j.NotifyPlatform,
			NotifyChatID:    maskNotifyChatID(j.NotifyChatID),
			LastResult:      j.LastResult,
			LastError:       j.LastError,
			LastErrorClass:  string(j.LastErrorClass),
			Notify:          j.Notify,
			FreshContext:    j.FreshContext,
			Backend:         j.Backend,
		}
		v.PausedReason, v.ConsecutiveFailures = j.PausedReason, j.AutoPauseCount()
		if j.PausedReason == cronpkg.PausedReasonAutoTransient && !j.LastRunAt.IsZero() && !j.TransientFailingSince.IsZero() {
			v.TransientOutageMS = j.LastRunAt.Sub(j.TransientFailingSince).Milliseconds()
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		// missed-schedule 只对非 paused 的 job 判定（暂停的错过是预期行为）；
		// 走 missedScheduleVerdict 让 Parse 结果在轮询间被缓存 (#857)。
		if !j.Paused {
			if missed, prevAt := h.missedScheduleVerdict(&j, now, startedAt); missed {
				v.Missed = true
				v.MissedSince = prevAt.UnixMilli()
			}
		}
		// CurrentRun 只在 job 正在执行时返回；空 stats 也省略以减少线上 noise。
		if cur, ok := h.deps.Scheduler.CurrentRun(j.ID); ok {
			v.CurrentRun = &cronCurrentRunView{
				RunID:     cur.RunID,
				StartedAt: cur.StartedAt.UnixMilli(),
				Phase:     cur.Phase,
				Trigger:   string(cur.Trigger),
				SessionID: cur.SessionID,
			}
		}
		if c := j.RunCounters; c.Total > 0 {
			v.Stats = &cronRunCountersView{
				Total:     c.Total,
				Succeeded: c.Succeeded,
				Failed:    c.Failed,
				Skipped:   c.Skipped,
				TimedOut:  c.TimedOut,
				Canceled:  c.Canceled,
			}
		}
		// recent_runs: 卡片 tooltip 用，上限 recentRunsPerJob；详情页用
		// GET /api/cron/runs。Read from the pre-fetched slice (#525).
		if recent := recentByIdx[idx]; len(recent) > 0 {
			// Sub-slice of recentBacking; the only writer into [start:end] (#1119).
			start := recentBackingNext
			end := start + len(recent)
			rv := recentBacking[start:end:end]
			recentBackingNext = end
			for i, r := range recent {
				rv[i] = cronSummaryToView(r)
			}
			v.RecentRuns = rv
		}
		views = append(views, v)
	}

	loc := h.deps.Scheduler.Location()
	// Reuse `now` so the tz label and missed-schedule check share one instant.
	name, offset := now.In(loc).Zone()
	locName := loc.String()
	tzLabel := h.cachedTZLabel(locName, offset)

	resp := cronListResp{
		Jobs:          views,
		Timezone:      locName,
		TimezoneLabel: tzLabel,
		RecentRunsCap: recentRunsPerJob,
		TimezoneAbbr:  name,
	}
	if def := h.deps.Scheduler.NotifyDefault(); def.IsSet() {
		// The chat_id is masked: in a multi-operator deployment it is a private
		// notification target and must not leak verbatim to every user (#789).
		resp.NotifyDefault = &cronNotifyDefaultView{
			Platform: def.Platform,
			ChatID:   maskNotifyChatID(def.ChatID),
		}
	}
	httputil.WriteJSON(w, resp)
}

// cachedTZLabel memoises formatTZOffset(locName, offset) for the 1 Hz poll.
// Keyed on offset too because a fixed location's offset flips across DST.
func (h *Handlers) cachedTZLabel(locName string, offset int) string {
	// Fast path: read-lock for the ≈100% cache-hit case (DST flips are rare).
	h.tzLabelMu.RLock()
	if h.tzLabelHasVal && h.tzLabelLoc == locName && h.tzLabelOffset == offset {
		cached := h.tzLabelCached
		h.tzLabelMu.RUnlock()
		return cached
	}
	h.tzLabelMu.RUnlock()

	// Slow path: write-lock with double-check so concurrent misses don't both recompute.
	h.tzLabelMu.Lock()
	defer h.tzLabelMu.Unlock()
	if h.tzLabelHasVal && h.tzLabelLoc == locName && h.tzLabelOffset == offset {
		return h.tzLabelCached
	}
	label := formatTZOffset(locName, offset)
	h.tzLabelLoc = locName
	h.tzLabelOffset = offset
	h.tzLabelCached = label
	h.tzLabelHasVal = true
	return label
}

// formatTZOffset renders a label like "Asia/Shanghai (UTC+08:00)". ianaName is
// the IANA zone identifier, NOT the abbr (sent separately as timezone_abbr).
// The minute component is abs()'d so "UTC-05:30" does not render as "UTC-05:-30".
func formatTZOffset(ianaName string, offsetSeconds int) string {
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	if minutes < 0 {
		minutes = -minutes
	}
	return fmt.Sprintf("%s (UTC%+03d:%02d)", ianaName, hours, minutes)
}
