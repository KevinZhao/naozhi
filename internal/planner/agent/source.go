package agent

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
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
	// SourceLookup is a web lookup's answer (planner/reader, §15.12 ⑨):
	// what the reader read off web pages, so its turn may only notify,
	// promise and ask, as a scout turn.
	SourceLookup Source = "lookup"
	// SourceMorning is the morning tidy (§15.18, D133): before the brief
	// the gateway lists the sessions that moved since the last one, for the
	// Planner to bring the ledger of the user's lines of work up to date.
	// The list is the gateway's and what it names is the sessions' output,
	// as a receipt's is: not outside content, and with no words of the
	// user's.
	SourceMorning Source = "morning"
	// SourceAnswer is the user's answer to a question the Planner asked
	// (TellAnswer): a receipt, but one the user started, its words the
	// answer (D98).
	SourceAnswer Source = "answer"
	// SourceWatch is a session the user opened (观察中) ending a turn
	// normally (TellWatch, §15.22 ①, D138): the gateway's line of it, its
	// last reply's first sentence the session's output. Its turn may only
	// write a side note, notify and update the ledger of lines of work.
	SourceWatch Source = "watch"
	// SourceWatchOutside is SourceWatch for a session that read outside
	// content (its digest headed 含外部内容): what it wrote may carry a line
	// of a web page or a mail, so its turn is held to an outside turn's
	// limits as well and may not write the ledger. A source of its own, so
	// the limit is in turns.json with the request and holds across a restart.
	SourceWatchOutside Source = "watch_outside"
	// SourceWatchRefusal is the receipt of the action blocks a watch turn
	// did not run (TellWatchRefused). Its turn is a watch turn and an
	// outside one both, so it runs only note and notify: a block the watch
	// turn was refused is not run on a second asking, which a receipt from
	// SourceUnrun would let it be. It gets no receipt of its own.
	SourceWatchRefusal Source = "watch_refusal"
	// SourcePatrol is the new findings of a workspace's weekly deep check
	// (Wake, §15.23 ③, D142). They are the user's own repository's content,
	// so the turn is not held to an outside turn's limits and may dispatch;
	// it has none of the user's words, and runs only dispatch (naming its
	// finding), front, notify, ask and note.
	SourcePatrol Source = "patrol"
	// SourcePatrolRefusal is the receipt of the action blocks a patrol turn
	// did not run (TellPatrolRefused). Its turn is held to a patrol turn's
	// limits and caps, or they would hold for one turn only, and gets no
	// receipt of its own.
	SourcePatrolRefusal Source = "patrol_refusal"
	// SourcePatrolReceipt is the gateway's receipt about a session
	// dispatched to fix a patrol finding (TellPatrol): dispatched, its
	// report, its turn's end. The session came of a patrol turn, or of one
	// like it, so the turn its receipt starts is held to a patrol turn's
	// limits and caps as well — or they would hold for the one turn that
	// dispatched and no longer for what follows from it — and may besides
	// mark the task and send it on through custody, as any receipt's turn.
	// A receipt carrying other receipts with it is from here too: the
	// stricter of the two.
	SourcePatrolReceipt Source = "patrol_receipt"
)

// Words is what the user said in a Planner turn, as the gateway recorded it
// (D98): what they typed, or the answer they picked to Question. A turn
// carrying words is one the user started; only such a turn may pass the
// user's decision on to a session.
type Words struct {
	Text     string    `json:"text"`
	Question string    `json:"question,omitempty"`
	At       time.Time `json:"at"`
}

// Entry is the source a turn from s writes on its transcript entry
// (clievent.EventEntry.Source), by which the Planner page lays out its
// timeline without reading the text (§15.14 ②, D113): "" for your message
// and a session you handed over, the user's own as on any session; the
// source's name for the rest, your answer among them. A turn merging
// requests of different sources carries "" (turn.startTurn), which the page
// reads part by part. Both watch sources write "watch", and the receipt of
// a watch turn's blocks "refusal", as does that of a patrol turn's, and a
// receipt about a patrol's task "receipt": the page lays each pair out
// alike.
func (s Source) Entry() string {
	switch s {
	case SourceUser, SourceHandover:
		return ""
	case SourceWatchOutside:
		return string(SourceWatch)
	case SourceWatchRefusal, SourcePatrolRefusal:
		return string(SourceRefusal)
	case SourcePatrolReceipt:
		return string(SourceReceipt)
	}
	return string(s)
}

// DynamicSources are the sources whose turns the Planner page folds into
// 动态 rather than showing as the conversation (§15.14 ②, D113): what the
// gateway, a session or outside content brought in, not you. SourceShift
// is neither: it is the timeline's separator. Sorted, as contract.js lists
// it.
func DynamicSources() []string {
	out := []string{string(SourceBriefing), string(SourceLookup), string(SourceMorning), string(SourcePatrol), string(SourceReceipt), string(SourceRefusal), string(SourceReminder), string(SourceScout), string(SourceUnrun), string(SourceWatch)}
	slices.Sort(out)
	return out
}

// outsideLabels names the sources whose turns carry outside content (mail,
// Slack messages, tasks other people wrote, a web lookup's answer), or
// answer a turn that did: a line in one could pose as a request, so their
// turns may only notify, promise and ask, and write the user a side note
// (note), which reaches no session. A watched session that read outside
// content is among them, and the receipt of any watch turn's blocks, their
// turns held to a watch turn's limits besides.
var outsideLabels = map[Source]string{SourceScout: "巡检", SourceBriefing: "会前简报", SourceRefusal: "被拒动作的回执", SourceLookup: "网页查询结果",
	SourceWatchOutside: "读过外部内容的会话", SourceWatchRefusal: "被拒动作的回执"}

// Outside reports whether a turn from s carries outside content (or answers
// a turn that did).
func (s Source) Outside() bool { return outsideLabels[s] != "" }

// Alone reports whether the gateway runs a request from s as a turn of its
// own, never merged with the user's message: an outside turn, whose limits
// would refuse what the user asked for in the same turn, and a watch turn
// or a patrol turn (and the receipt of its blocks, and a receipt about a
// task of the patrol's), which merged would either refuse the user's blocks
// or run its own by the user's leave — a patrol's dispatch with the user's
// words.
func (s Source) Alone() bool {
	return s.Outside() || s == SourceWatch || s == SourcePatrol || s == SourcePatrolRefusal || s == SourcePatrolReceipt
}

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

// Tidying reports whether the morning tidy's turn is in flight: the brief
// waits for it (brief.Config.Tidying).
func (a *Agent) Tidying() bool { return slices.Contains(a.TurnSources(), SourceMorning) }

// TurnWords is the user's words of the Planner requests in flight, oldest
// first (Turns.PlannerTurnWords): none for a turn the user did not start.
// The action runner reads it as a turn's result arrives, like OutsideTurn.
func (a *Agent) TurnWords() []Words {
	a.mu.Lock()
	turns := a.turns
	a.mu.Unlock()
	if turns == nil {
		return nil
	}
	return turns.PlannerTurnWords(Key)
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
// blocks not run (TellUnrun, TellRefused, TellWatchRefused,
// TellPatrolRefused). The action
// runner reads it as a turn's result arrives: such a turn's own blocks not
// run get no receipt, so the two do not go round, while any other turn's do,
// however the turns queued up between.
func (a *Agent) UnrunTurn() bool {
	for _, src := range a.TurnSources() {
		if src == SourceUnrun || src == SourceRefusal || src == SourceWatchRefusal || src == SourcePatrolRefusal {
			return true
		}
	}
	return false
}

// WatchTurn reports whether the turn in flight was started by a watched
// session's turn end (TellWatch), or answers the receipt of such a turn's
// blocks (TellWatchRefused). The action runner reads it as a turn's result
// arrives: such a turn runs only its note, notify and front blocks.
func (a *Agent) WatchTurn() bool {
	for _, src := range a.TurnSources() {
		if src == SourceWatch || src == SourceWatchOutside || src == SourceWatchRefusal {
			return true
		}
	}
	return false
}

// PatrolTurn reports whether the turn in flight was started by a deep
// check's new findings (Wake from SourcePatrol), or answers the receipt of
// such a turn's blocks (TellPatrolRefused). The action runner reads it as a
// turn's result arrives: such a turn runs only its dispatch (naming a
// finding), front, notify, ask and note blocks.
func (a *Agent) PatrolTurn() bool {
	for _, src := range a.TurnSources() {
		if src == SourcePatrol || src == SourcePatrolRefusal {
			return true
		}
	}
	return false
}

// PatrolTaskTurn reports whether the turn in flight answers a receipt about
// a session dispatched to fix a patrol finding (TellPatrol). The action
// runner reads it as a turn's result arrives: such a turn runs what a
// patrol turn runs, held to the same caps, and status and custody blocks.
func (a *Agent) PatrolTaskTurn() bool {
	return slices.Contains(a.TurnSources(), SourcePatrolReceipt)
}

// Wake starts a Planner turn from src with each block, the gateway's data
// for it (a scout run's findings, in batches a reply's action blocks can
// answer), followed by the snapshot. A block goes once the previous one's
// turn ended, so queued blocks never merge into one turn. It gets no
// receipt. ErrOff, ErrStopped or ErrNotReady when the Planner cannot take
// the first: the caller keeps what it found. sent (nil for none) is told
// how each later block went, nil once submitted: one the Planner cannot
// take gets the error, and so does every block after it, for the caller to
// keep; it is said in the conversation as well.
func (a *Agent) Wake(src Source, sent func(block int, err error), blocks ...string) error {
	return a.WakeEnded(src, sent, nil, blocks...)
}

// WakeEnded is Wake for a caller that follows each block to its turn's
// end: ended (nil for none) is told how block's turn went once it is over,
// the error of one that failed or was dropped from the queue. Submitted is
// not handled: a turn may fail, or be lost to a restart while it waits, in
// which case ended is never told.
func (a *Agent) WakeEnded(src Source, sent, ended func(block int, err error), blocks ...string) error {
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
	if sent == nil {
		sent = func(int, error) {}
	}
	return a.wakeChain(src, sent, ended, blocks, 0)
}

// wakeChain submits blocks[i] and chains the rest on its turn's end.
func (a *Agent) wakeChain(src Source, sent, ended func(int, error), blocks []string, i int) error {
	a.mu.Lock()
	turns, stopped, opts := a.turns, a.state.Stopped, a.opts
	a.mu.Unlock()
	if stopped {
		return ErrStopped
	}
	var done func(string, error)
	if next := i + 1; next < len(blocks) {
		var once sync.Once
		done = func(string, error) {
			once.Do(func() {
				err := a.wakeChain(src, sent, ended, blocks, next)
				if err == nil {
					return
				}
				rest := len(blocks) - next
				slog.Warn("planner: later wake blocks not sent", "source", src, "blocks", rest, "err", err)
				a.Note(fmt.Sprintf("%s还有 %d 批内容没有交给 Planner：%v", cmp.Or(outsideLabels[src], "网关"), rest, err))
				for j := next; j < len(blocks); j++ {
					sent(j, err)
				}
			})
		}
	}
	// The caller hears of this block's turn before the next one goes.
	if ended != nil {
		next := done
		done = func(reply string, err error) {
			ended(i, err)
			if next != nil {
				next(reply, err)
			}
		}
	}
	// The block leads: the logged bubble keeps only the message's head, and
	// the page marks the turn by it.
	snap, ran := a.nextSnapshot()
	if ran != nil {
		if next := done; next != nil {
			done = func(reply string, err error) {
				ran(reply, err)
				next(reply, err)
			}
		} else {
			done = ran
		}
	}
	_, err := turns.SubmitPlanner(Key, src, nil, blocks[i]+"\n\n"+snap, nil, opts, done)
	if err == nil && i > 0 {
		sent(i, nil)
	}
	return err
}
