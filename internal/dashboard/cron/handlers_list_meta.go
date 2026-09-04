package cron

// cronListResp is the wire shape returned by GET /api/cron — the dashboard
// list view.
type cronListResp struct {
	Jobs          []cronJobView `json:"jobs"`
	Timezone      string        `json:"timezone"`
	TimezoneLabel string        `json:"timezone_label"`
	TimezoneAbbr  string        `json:"timezone_abbr"`
	// RecentRunsCap echoes recentRunsPerJob so the dashboard can tell
	// "fewer than cap → history fully in hand" from "exactly cap → there
	// may be more, confirm via GET /api/cron/runs" without hard-coding
	// the same constant a second time in cron_view.js.
	RecentRunsCap int                    `json:"recent_runs_cap"`
	NotifyDefault *cronNotifyDefaultView `json:"notify_default,omitempty"`
}
