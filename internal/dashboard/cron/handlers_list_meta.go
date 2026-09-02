package cron

// cronListResp is the wire shape returned by GET /api/cron — the dashboard
// list view. R230B-CR-3 swapped the previous map[string]any literal for
// this named struct so the JSON encoder can cache the type's reflect
// descriptor across the 1-Hz dashboard polls instead of paying the
// per-call map iteration + interface boxing each request.
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
