package cli

// turn_watermark.go — where a shim's output stream stood when a Send was
// issued, so a later reconnect can tell that Send's result from an earlier one
// replayed alongside it (docs/rfc/cron-run-adoption.md §3.2, #3104).

import (
	"strconv"
	"strings"
)

// TurnWatermark is a position in one shim's output stream. Seqs are numbered
// per shim, so Seq only compares between watermarks of the same ShimPID.
type TurnWatermark struct {
	ShimPID int
	Seq     int64
}

// String encodes w as "<shimPID>:<seq>", the form ParseTurnWatermark reads.
func (w TurnWatermark) String() string {
	return strconv.Itoa(w.ShimPID) + ":" + strconv.FormatInt(w.Seq, 10)
}

// ParseTurnWatermark decodes String's form. ok=false for anything else,
// including "" (no watermark was recorded).
func ParseTurnWatermark(s string) (w TurnWatermark, ok bool) {
	pid, seq, found := strings.Cut(s, ":")
	if !found {
		return TurnWatermark{}, false
	}
	p, err := strconv.Atoi(pid)
	if err != nil || p <= 0 {
		return TurnWatermark{}, false
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 0 {
		return TurnWatermark{}, false
	}
	return TurnWatermark{ShimPID: p, Seq: n}, true
}

// TurnWatermark is where this process's shim stream stands now: every frame up
// to Seq has been received. ok=false when the shim never reported its PID, as
// the seq would then name no particular stream.
func (p *Process) TurnWatermark() (TurnWatermark, bool) {
	if p.link.shimPID <= 0 {
		return TurnWatermark{}, false
	}
	return TurnWatermark{ShimPID: p.link.shimPID, Seq: p.link.lastSeq.Load()}, true
}
