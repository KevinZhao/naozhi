package agent

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Source is what started a Planner turn. The gateway records it for every
// turn it submits, so a reply's action blocks can be judged by where the
// turn came from (docs/rfc/planner.md §15.6 安全).
type Source string

const (
	// SourceUser is a message you sent.
	SourceUser Source = "user"
	// SourceHandover is a session you handed over (交给 Planner).
	SourceHandover Source = "handover"
	// SourceReceipt is the gateway's receipt (Tell): a dispatch's outcome,
	// your answer to a question.
	SourceReceipt Source = "receipt"
	// SourceReminder is a due reminder.
	SourceReminder Source = "reminder"
	// SourceShift is the shift change: the handoff request and summary.
	SourceShift Source = "shift"
	// SourceScout is a scout run's findings.
	SourceScout Source = "scout"
	// SourceBriefing is a pre-meeting brief's material.
	SourceBriefing Source = "briefing"
	// SourceRefusal is the receipt of action blocks refused in a scout or
	// briefing turn (TellRefused).
	SourceRefusal Source = "refusal"
	// SourceUnrun is the receipt of the action blocks a reply's turn did not
	// run, in a turn no outside content brought in (TellUnrun).
	SourceUnrun Source = "unrun"
)

// outsideLabels names the sources whose turns carry outside content (mail,
// Slack messages, tasks other people wrote), or answer a turn that did: a
// line in one could pose as a request, so their turns may only promise and
// ask.
var outsideLabels = map[Source]string{SourceScout: "巡检", SourceBriefing: "会前简报", SourceRefusal: "被拒动作的回执"}

// ErrNoBlock answers a Wake with nothing to say.
var ErrNoBlock = errors.New("唤醒 Planner 的内容为空")

// TurnSources is the sources of the Planner requests in flight, sorted:
// those the gateway submitted and that have not finished, and those in
// flight when the last run ended (a shim-held turn finishes on the
// reattached CLI), each until a result answers it. Requests running side
// by side (queue.mode passthrough) all count, so a turn is never judged by
// fewer sources than its own. None between turns or before Start; a turn
// the CLI runs on its own (a cross-session message) has none, unless one
// of the gateway's was in flight.
func (a *Agent) TurnSources() []Source {
	a.mu.Lock()
	turns := a.turns
	a.mu.Unlock()
	if turns == nil {
		return nil
	}
	return turns.PlannerTurnSources(Key)
}

// OutsideTurn names what brought outside content into the turn in flight
// ("巡检", "会前简报"), "" when nothing did. The action runner reads it as a
// turn's result arrives, while the turn is still in flight.
func (a *Agent) OutsideTurn() string {
	var labels []string
	for _, src := range a.TurnSources() {
		if l := outsideLabels[src]; l != "" {
			labels = append(labels, l)
		}
	}
	return strings.Join(labels, "、")
}

// UnrunTurn reports whether the turn in flight answers a receipt of action
// blocks not run (TellUnrun, TellRefused). The action runner reads it as a
// turn's result arrives: such a turn's own blocks not run get no receipt, so
// the two do not go round, while any other turn's do, however the turns
// queued up between.
func (a *Agent) UnrunTurn() bool {
	for _, src := range a.TurnSources() {
		if src == SourceUnrun || src == SourceRefusal {
			return true
		}
	}
	return false
}

// Wake starts a Planner turn from src with each block, the gateway's data
// for it (a scout run's findings, in batches a reply's action blocks can
// answer), followed by the snapshot. A block goes once the previous one's
// turn ended, so queued blocks never merge into one turn. It gets no
// receipt. ErrOff, ErrStopped or ErrNotReady when the Planner cannot take
// the first: the caller keeps what it found. A later block it cannot take
// is said in the conversation, so no finding goes unseen.
func (a *Agent) Wake(src Source, blocks ...string) error {
	if !a.cfg.Enabled {
		return ErrOff
	}
	a.mu.Lock()
	turns, stopped := a.turns, a.state.Stopped
	a.mu.Unlock()
	switch {
	case stopped:
		return ErrStopped
	case turns == nil:
		return ErrNotReady
	case len(blocks) == 0:
		return ErrNoBlock
	}
	for i, b := range blocks {
		switch blocks[i] = strings.TrimSpace(b); {
		case blocks[i] == "":
			return ErrNoBlock
		case len(blocks[i]) > maxMessageBytes:
			return ErrTooLong
		}
	}
	return a.wakeChain(src, blocks)
}

// wakeChain submits blocks[0] and chains the rest on its turn's end.
func (a *Agent) wakeChain(src Source, blocks []string) error {
	a.mu.Lock()
	turns, stopped, opts := a.turns, a.state.Stopped, a.opts
	a.mu.Unlock()
	if stopped {
		return ErrStopped
	}
	var done func(string, error)
	if rest := blocks[1:]; len(rest) > 0 {
		var once sync.Once
		done = func(string, error) {
			once.Do(func() {
				if err := a.wakeChain(src, rest); err != nil {
					slog.Warn("planner: later wake blocks not sent", "source", src, "blocks", len(rest), "err", err)
					a.Note(fmt.Sprintf("%s还有 %d 批内容没有交给 Planner：%v", cmp.Or(outsideLabels[src], "网关"), len(rest), err))
				}
			})
		}
	}
	// The block leads: the logged bubble keeps only the message's head, and
	// the page marks the turn by it.
	_, err := turns.SubmitPlanner(Key, src, blocks[0]+"\n\n"+a.Snapshot(), opts, done)
	return err
}
