// Package workflow tracks the background runs of Claude Code's Workflow tool
// (local_workflow tasks) from the stream-json frames one CLI process emits:
// a header per run, its phases and one row per workflow agent, normalized,
// redacted and bounded so the dashboard can publish them as they are.
//
// A Tracker is the per-process builder; it publishes an immutable Set. The
// session layer's board merges Sets across processes into a Published and
// stamps wire versions on it. This package is pure logic: no I/O, no
// goroutines; it imports only clievent and textutil.
// See docs/rfc/workflow-dashboard.md §4.2 and §5.2-§5.7.
package workflow

// Status is a workflow's normalized status.
type Status string

const (
	StatusRunning     Status = "running"
	StatusPaused      Status = "paused"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusKilled      Status = "killed"
	StatusInterrupted Status = "interrupted"
	StatusUnknown     Status = "unknown"
)

// AllStatuses lists every Status, for the dashboard's generated contract.
func AllStatuses() []string {
	return []string{string(StatusRunning), string(StatusPaused), string(StatusCompleted), string(StatusFailed),
		string(StatusKilled), string(StatusInterrupted), string(StatusUnknown)}
}

// AgentState is a workflow agent row's normalized state.
type AgentState string

const (
	AgentQueued  AgentState = "queued"
	AgentRunning AgentState = "running"
	AgentDone    AgentState = "done"
	AgentFailed  AgentState = "failed"
	AgentSkipped AgentState = "skipped"
	AgentStopped AgentState = "stopped"
	AgentUnknown AgentState = "unknown"
)

// AllAgentStates lists every AgentState, for the dashboard's generated contract.
func AllAgentStates() []string {
	return []string{string(AgentQueued), string(AgentRunning), string(AgentDone), string(AgentFailed),
		string(AgentSkipped), string(AgentStopped), string(AgentUnknown)}
}

// Source names where a published workflow's rows last came from.
type Source string

const (
	SourceStream     Source = "stream"
	SourceReplay     Source = "replay"
	SourceResultFile Source = "result_file"
	SourceRef        Source = "ref"
)

// Degraded values, in precedence order: when several hold, the first wins.
// The board sets DegradedSnapshotStale; the Tracker sets the rest.
const (
	DegradedSnapshotStale   = "snapshot_stale"
	DegradedSnapshotDropped = "snapshot_dropped"
	DegradedTooMany         = "too_many"
	DegradedPhasesCapped    = "phases_capped"
	DegradedDecodeError     = "decode_error"
	DegradedNoSnapshot      = "no_snapshot"
)

// IsRunning reports whether a workflow in status st still runs: running or
// paused. It decides keep-alive pinning and WorkflowBoard.Running.
func IsRunning(st Status) bool { return st == StatusRunning || st == StatusPaused }

// IsUnsettled reports whether st is not final: running, paused or unknown.
// The display set is every unsettled workflow plus the latest terminal ones.
func IsUnsettled(st Status) bool { return IsRunning(st) || st == StatusUnknown }

// IsTerminal reports whether st is final.
func IsTerminal(st Status) bool {
	switch st {
	case StatusCompleted, StatusFailed, StatusKilled, StatusInterrupted:
		return true
	}
	return false
}

// Agent is one workflow agent row, keyed by CC's agent Index. Strings are
// redacted, then truncated. Rows are immutable once published; the board
// stamps Rev on its own copies.
type Agent struct {
	Index      int    `json:"index"`
	PhaseIndex int    `json:"phase_index,omitempty"`
	Label      string `json:"label"`
	// AgentID is the current or last non-empty agentId; empty means the
	// agent never started and cannot be drilled into.
	AgentID string `json:"agent_id,omitempty"`
	// PrevAgentIDs are earlier attempts' agentIds, oldest first, ≤ 8.
	PrevAgentIDs    []string   `json:"prev_agent_ids,omitempty"`
	Model           string     `json:"model,omitempty"`
	State           AgentState `json:"state"`
	RawState        string     `json:"raw_state,omitempty"` // only when State is unknown
	Blocked         bool       `json:"blocked,omitempty"`
	Attempt         int        `json:"attempt,omitempty"`
	Cached          bool       `json:"cached,omitempty"`
	QueuedAt        int64      `json:"queued_at,omitempty"`
	StartedAt       int64      `json:"started_at,omitempty"`
	LastProgressAt  int64      `json:"last_progress_at,omitempty"`
	DurationMS      int64      `json:"duration_ms,omitempty"`
	Tokens          int64      `json:"tokens,omitempty"`
	ToolCalls       int        `json:"tool_calls,omitempty"`
	LastTool        string     `json:"last_tool,omitempty"`
	LastToolSummary string     `json:"last_tool_summary,omitempty"`
	Error           string     `json:"error,omitempty"`
	// Rev is the wire version at which this row's content last changed; the
	// board stamps it, the Tracker leaves it 0.
	Rev uint64 `json:"rev"`
}

// Phase is one workflow phase with the counts of its agents.
type Phase struct {
	Index  int    `json:"index"`
	Title  string `json:"title"`
	Counts `json:"counts"`
}

// Counts tallies agents by normalized state. Unknown rows count in Total only.
type Counts struct {
	Total   int `json:"total"`
	Queued  int `json:"queued"`
	Running int `json:"running"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Stopped int `json:"stopped"`
}

// Workflow is one run, keyed by its task id. Published values are immutable.
// Fields tagged json:"-" stay off the wire (see WireView).
type Workflow struct {
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Current     string `json:"current,omitempty"` // latest "<phase>: <label>"
	Status      Status `json:"status"`
	RawStatus   string `json:"raw_status,omitempty"`
	// StartedAt is 0 when unknown; Src.StartedAt says how it was learned.
	StartedAt int64 `json:"started_at,omitempty"`
	EndedAt   int64 `json:"ended_at,omitempty"`
	// LastObservedAt is the last live frame's time; 0 for a replay seed.
	LastObservedAt int64   `json:"last_observed_at,omitempty"`
	Tokens         int64   `json:"tokens,omitempty"`
	ToolCalls      int     `json:"tool_calls,omitempty"`
	DurationMS     int64   `json:"duration_ms,omitempty"`
	Counts         Counts  `json:"counts"`
	Phases         []Phase `json:"phases"`
	Agents         []Agent `json:"agents"` // ascending Index
	// AgentsCapped: the snapshot had more agents than maxAgents; Counts
	// still cover all of them.
	AgentsCapped  bool   `json:"agents_capped,omitempty"`
	NotifySummary string `json:"notify_summary,omitempty"`
	Source        Source `json:"source"`
	Degraded      string `json:"degraded,omitempty"`
	// Version is the board's wire version once published; 0 in a Set.
	Version uint64 `json:"version"`

	TrackerVersion uint64 `json:"-"` // bumped on every change; the board's ordering guard
	SnapshotSeq    uint64 `json:"-"` // stream / replay snapshots applied
	ResultLoaded   bool   `json:"-"` // a result file with this TaskID was merged
	// LaunchTranscriptDir is the launch receipt's transcriptDir verbatim:
	// never validated here, never used for I/O.
	LaunchTranscriptDir string `json:"-"`
	// SessionID is the CC session the run hangs under, verbatim.
	SessionID string   `json:"-"`
	Src       FieldSrc `json:"-"`
	// RunDir is written only by the board, after resolving it off-lock.
	RunDir string `json:"-"`
}

// FieldSrc grades how Name, StartedAt and SessionID were learned; higher
// is more trustworthy, 0 means no value. The board merges per field by it.
type FieldSrc struct{ Name, StartedAt, SessionID uint8 }

// Source grades of Workflow.Name.
const (
	NameFromSummary uint8 = 1 // summary / description fallback
	NameFromRef     uint8 = 2 // board merge only
	NameFromLaunch  uint8 = 3 // task_started.workflow_name / launch workflowName
)

// Source grades of Workflow.StartedAt.
const (
	StartedFromSnapshot   uint8 = 1 // earliest agent queuedAt / startedAt
	StartedFromLive       uint8 = 2 // time a live task_started was read
	StartedFromRef        uint8 = 3 // board merge only
	StartedFromResultFile uint8 = 4 // result file startTime
)

// Source grades of Workflow.SessionID.
const (
	SessionFromProgress uint8 = 1 // earliest other task frame's session_id
	SessionFromRef      uint8 = 2 // board merge only
	SessionFromLaunch   uint8 = 3 // task_started / launch frame's session_id
	SessionFromRunDir   uint8 = 4 // board: <sid> of the resolved run dir
)

// Set is what a Tracker publishes: immutable, swapped atomically.
type Set struct {
	// Workflows: unsettled first by StartedAt, then terminal by EndedAt, newest first.
	Workflows []*Workflow
	Version   uint64 // Tracker-private, monotonic
	// SeedWrapped: the replay SeedFromReplay saw had lost frames to the
	// shim ring's eviction.
	SeedWrapped bool
}

// Summary is a workflow's minimal form in a session snapshot.
type Summary struct {
	TaskID       string `json:"task_id"`
	Name         string `json:"name,omitempty"`
	Status       Status `json:"status"`
	Counts       Counts `json:"counts"`
	Tokens       int64  `json:"tokens,omitempty"`
	StartedAt    int64  `json:"started_at,omitempty"`
	EndedAt      int64  `json:"ended_at,omitempty"`
	CurrentPhase string `json:"current_phase,omitempty"`
	Epoch        string `json:"epoch"`
	Version      uint64 `json:"version"` // same version space as workflow_state frames
}

// Ref is the header the board persists per workflow in sessions.json, so a
// restart can show runs whose frames the replay no longer holds. A
// non-empty SessionID or RunID must be re-validated when read back.
type Ref struct {
	TaskID         string `json:"task_id"`
	RunID          string `json:"run_id,omitempty"`
	Name           string `json:"name,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Status         Status `json:"status"`
	StartedAt      int64  `json:"started_at,omitempty"`
	EndedAt        int64  `json:"ended_at,omitempty"`
	LastObservedAt int64  `json:"last_observed_at,omitempty"`
	Counts         Counts `json:"counts"`
	Tokens         int64  `json:"tokens,omitempty"`
}
