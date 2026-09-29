package spawndiag

import "sync"

// recentCap bounds the recent-diag list /health carries.
const recentCap = 32

// Entry is one diag as the operator-facing summary carries it: which gate,
// which configured key, what the gate did. Reason and scope are left out on
// purpose: a reason can quote the refused value and a scope is a session key.
type Entry struct {
	Layer  string `json:"layer"`
	Key    string `json:"key"`
	Action string `json:"action"`
}

// Summary is what the process has observed since start: first occurrences
// counted per "layer|action", and the most recent ones, newest last.
type Summary struct {
	Counts map[string]int64 `json:"counts"`
	Recent []Entry          `json:"recent"`
}

var (
	summaryMu sync.Mutex
	counts    = map[string]int64{}
	recent    []Entry
)

// record adds a first occurrence to the summary.
func record(d Diag) {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	counts[d.Layer+"|"+d.Action]++
	recent = append(recent, Entry{Layer: d.Layer, Key: d.Key, Action: d.Action})
	if len(recent) > recentCap {
		recent = append(recent[:0:0], recent[len(recent)-recentCap:]...)
	}
}

// Snapshot returns a copy of the summary, or false when nothing has been
// observed.
func Snapshot() (Summary, bool) {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	if len(counts) == 0 {
		return Summary{}, false
	}
	s := Summary{Counts: make(map[string]int64, len(counts)), Recent: append([]Entry(nil), recent...)}
	for k, v := range counts {
		s.Counts[k] = v
	}
	return s, true
}

// resetSummaryForTest clears the summary.
func resetSummaryForTest() {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	counts = map[string]int64{}
	recent = nil
}
