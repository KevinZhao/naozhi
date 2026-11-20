// Package action is the Planner's way to ask the gateway for something
// (docs/rfc/planner.md §6.2): the ```naozhi-action blocks at the end of a
// planner:main reply. There are twelve: dispatch (a task: session, started at
// once, D88), promise (a promise among the 任务), ask (a
// question in "通知" whose answer goes back to the Planner, or straight to a
// session, D98), status (the Planner's judgment of a task's state, §15.4),
// notify (something to tell the user, pushed now or held for the morning
// brief, §15.6), custody (a session's custody switched, or a message sent to
// a session in custody, planner/custody), relay (the user's decision
// passed on to a session with the user's own words, D98), remember (a
// memory the user asked for, written into a workspace's auto-memory,
// §15.12 ⑧), lookup (a web lookup by a one-shot reader, §15.12 ⑨), park
// (an item the user put down, moved into the 搁置区, §15.14 ⑤), halt (a
// session the user asked to stop: its custody stopped, its process closed,
// D119) and cancel (a task the user said is not wanted, stopped as from 任务,
// D120). A block
// that does not parse, or names an unknown type, a bad field or a task that
// does not exist, is not run: the Planner conversation shows it as it was
// written, with the reason, and the Planner gets a receipt naming it.
package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// Fence is the info string that opens an action block.
const Fence = "naozhi-action"

// Limits on what a block may carry.
const (
	// maxBlocks is how many blocks one reply may run; the rest are refused,
	// and the receipt asks for them in another reply. A scout turn may carry
	// several reminders.
	maxBlocks = 10
	// maxBlockBytes caps one block's body.
	maxBlockBytes = 64 << 10
	// titleRunes is the CLI's --name cap.
	titleRunes        = 64
	messageRunes      = 20000
	textRunes         = 500
	maxOptions        = 6
	optionRunes       = 80
	workspaceRefRunes = 128
	// goalRunes caps a dispatch's goal (D103).
	goalRunes = 60
	// notifyTitleRunes is what the inbox shows of a title.
	notifyTitleRunes = 120
	// notifyIDRunes matches the scout's finding IDs.
	notifyIDRunes = 512
	linkBytes     = 2048
	// keyRunes bounds a session key (sessionkey's cap).
	keyRunes = 512
	// custodyTextRunes caps what the Planner tells a session in custody.
	custodyTextRunes = 2000
	// relayTextRunes caps the Planner's words that follow the user's in a
	// relay.
	relayTextRunes = 4000
)

// Type is a block's "type".
type Type string

const (
	TypeDispatch Type = "dispatch"
	TypePromise  Type = "promise"
	TypeAsk      Type = "ask"
	TypeStatus   Type = "status"
	TypeNotify   Type = "notify"
	TypeCustody  Type = "custody"
	TypeRelay    Type = "relay"
	TypeRemember Type = "remember"
	TypeLookup   Type = "lookup"
	TypePark     Type = "park"
	TypeHalt     Type = "halt"
	TypeCancel   Type = "cancel"
	TypeRoutine  Type = "routine"
	TypeFront    Type = "front"
)

// allTypes lists the block types as the refusal of an unknown one names
// them.
const allTypes = "dispatch、promise、ask、status、notify、custody、relay、remember、lookup、park、halt、cancel、routine、front"

// Dispatch asks for a session in a workspace.
type Dispatch struct {
	// Workspace is a workspace ID or name (workspaces.json).
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
	// Model is "" for the default.
	Model   string `json:"model"`
	Message string `json:"message"`
	// Goal is the task in one line, for its card and its digest (D103);
	// "" takes the message's first sentence. One past goalRunes is cut,
	// not refused: it only names the task.
	Goal string `json:"goal,omitempty"`
	// WithUserWords leads the first message with the user's words of the
	// turn, as a relay does (D98). Only a turn the user started may set it.
	WithUserWords bool `json:"with_user_words,omitempty"`
	// MCPTools are the write tools this session alone may use, by server
	// (D98): server name → its tool names (scout.WriteTools). The gateway
	// turns that server's write switch on for the session and denies its
	// other write tools. Only a turn the user started may carry it.
	MCPTools map[string][]string `json:"mcp_tools,omitempty"`
	// Ultracode starts the session in ultracode, standing workflow
	// orchestration for every turn (the CLI's settings key). The keyword in
	// Message does nothing: the CLI takes it only from a human's input. Only
	// a turn the user started may set it, as the user's choice.
	Ultracode bool `json:"ultracode,omitempty"`
}

// Promise registers something the Planner promised the user.
type Promise struct {
	Text      string    `json:"text"`
	Due       time.Time `json:"-"`
	CloseWhen string    `json:"close_when"`
}

// Ask puts a question to the user; Options are the answers offered (none:
// the user replies in the conversation). Task, when set, is the session the
// answer goes to straight from the gateway, with the user's words (D98); it
// needs options.
type Ask struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Task     string   `json:"task,omitempty"`
	// Trimmed is how many options were cut to optionRunes, "…" included: a
	// question is worth asking even if an option ran long, so the Planner is
	// told in the receipt instead of the block refused.
	Trimmed int `json:"-"`
}

// Status sets a task's state: Task is a task: key or a promise ID, State
// done, doing or failed, Why the evidence.
type Status struct {
	Task  string `json:"task"`
	State string `json:"state"`
	Why   string `json:"why"`
}

// Levels of a notify block.
const (
	// LevelTimely is pushed now, within the daily cap and quiet hours.
	LevelTimely = "timely"
	// LevelBrief waits for the next morning brief.
	LevelBrief = "brief"
)

// Notify tells the user something: Level timely pushes it, brief holds it
// for the next morning brief. ID is the scout finding it is about, "" when
// none; with Title and Link it names the thing, so it is pushed once a day.
type Notify struct {
	Title string `json:"title"`
	Why   string `json:"why"`
	Level string `json:"level"`
	Link  string `json:"link"`
	ID    string `json:"id"`
}

// Custody switches session Key's custody (On), sends it Text through
// custody as the Planner's go-ahead, or turns observing it off (Watch,
// §15.12 ⑥: the user said to leave it alone); exactly one of the three.
// Watch takes only false: observing is on by default, and only the user
// turns it back on.
type Custody struct {
	Key   string `json:"key"`
	On    *bool  `json:"on"`
	Text  string `json:"text"`
	Watch *bool  `json:"watch"`
}

// Relay passes the user's decision on to session Key (D98): the gateway
// leads Text, the Planner's words, with all the user's own words it recorded
// for the turn, never a part the Planner picks (a cut quote can turn "先不要
// 合并" into "合并"). Only a turn the user started may relay.
type Relay struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// Remember asks the gateway to keep Text as memory Name of Workspace (a
// workspace ID or name, or "global" for the Planner's own; §15.12 ⑧, D106).
// Only a turn the user started may remember: that is what keeps outside
// content out of the memory (D41).
type Remember struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Text      string `json:"text"`
}

// rememberTextRunes caps a memory's text.
const rememberTextRunes = 2000

// Lookup asks the gateway to look Q up on the web (§15.12 ⑨, D107): a
// one-shot reader session searches and reads the pages, and its short
// answer with sources comes back as a <naozhi-lookup> block. Why is what the
// Planner wants it for, given to the reader.
type Lookup struct {
	Q   string `json:"q"`
	Why string `json:"why"`
}

// Park moves an item the user put down into the 搁置区, planner/parked.md
// (§15.14 ⑤, D116): Text is the item, Source where it came from (a task
// key, a promise ID, an #issue, the time the user said it). Only a turn the
// user started may park: content brought in must not set aside what the
// user is to decide.
type Park struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

// Park limits: the item is one line, its source short.
const (
	parkTextRunes   = 300
	parkSourceRunes = 120
)

func (p *Park) validate() error {
	p.Text, p.Source = strings.Join(strings.Fields(p.Text), " "), strings.Join(strings.Fields(p.Source), " ")
	switch {
	case p.Text == "":
		return errors.New("park 缺少 text（要搁置的事）")
	case utf8.RuneCountInString(p.Text) > parkTextRunes:
		return fmt.Errorf("text 超过 %d 字", parkTextRunes)
	case p.Source == "":
		return errors.New("park 缺少 source（出处：任务 key、承诺 ID、#issue，或用户那句话的时间）")
	case utf8.RuneCountInString(p.Source) > parkSourceRunes:
		return fmt.Errorf("source 超过 %d 字", parkSourceRunes)
	}
	return nil
}

// Halt stops session Task for the user (§15.15 ②, D119): its custody's
// lineage is stopped and its process closed, the session and its context
// kept; the user's own message in it takes it on again. Only a turn the
// user started may halt: the Planner wakes only once a turn ended, so a
// halt it judged by itself would come too late to matter (D118).
type Halt struct {
	Task string `json:"task"`
	Why  string `json:"why"`
}

// Cancel stops dispatched session Task's task as the user's stop in 任务
// does (§15.15 ③, D120): marked done by the user, 已停止, the promises citing
// it closed. Why is the Planner's and shows only on its block's card. Only
// a turn the user started may cancel, so it is the user's word.
type Cancel struct {
	Task string `json:"task"`
	Why  string `json:"why"`
}

// stopFields checks the task and why of a halt or cancel block of type t.
func stopFields(t Type, task, why *string) error {
	*task, *why = strings.TrimSpace(*task), strings.TrimSpace(*why)
	if err := validKey("task", *task); err != nil {
		return errors.New(string(t) + " " + err.Error())
	}
	switch {
	case *why == "":
		return fmt.Errorf("%s 缺少 why（用户怎么说的）", t)
	case utf8.RuneCountInString(*why) > textRunes:
		return fmt.Errorf("why 超过 %d 字", textRunes)
	}
	return nil
}

func (h *Halt) validate() error { return stopFields(TypeHalt, &h.Task, &h.Why) }

func (c *Cancel) validate() error {
	if err := stopFields(TypeCancel, &c.Task, &c.Why); err != nil {
		return err
	}
	if !sessionkey.IsTaskKey(c.Task) {
		return errors.New("cancel 的 task 要是 task: key（你派出的任务）；用户自己开的会话由用户结束，做完了用 status 标 done")
	}
	return nil
}

// Lookup limits.
const (
	lookupQRunes   = 200
	lookupWhyRunes = 300
)

func (l *Lookup) validate() error {
	l.Q, l.Why = strings.TrimSpace(l.Q), strings.TrimSpace(l.Why)
	switch {
	case l.Q == "":
		return errors.New("lookup 缺少 q（要查的问题）")
	case utf8.RuneCountInString(l.Q) > lookupQRunes:
		return fmt.Errorf("q 超过 %d 字", lookupQRunes)
	case utf8.RuneCountInString(l.Why) > lookupWhyRunes:
		return fmt.Errorf("why 超过 %d 字", lookupWhyRunes)
	}
	return nil
}

// Routine asks for a standing task (例行关注, §15.17, D131): Prompt run in
// Workspace on Schedule (a five-field cron expression, or @daily / @every
// 6h), each run a fresh session, its result in the morning brief when it
// changed. With ID it switches an existing one off or back on instead. Only
// a turn the user started may make or switch one: a mail asking for a
// standing task is not the user asking.
type Routine struct {
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Schedule  string `json:"schedule,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	// ID names an existing routine (the snapshot lists them by it), Off
	// whether to pause it (true) or run it again (false).
	ID  string `json:"id,omitempty"`
	Off *bool  `json:"off,omitempty"`
}

const (
	routineScheduleBytes = 100
	routinePromptRunes   = 8000
	routineIDRunes       = 64
)

func (t *Routine) validate() error {
	t.Title, t.Workspace, t.Schedule = strings.TrimSpace(t.Title), strings.TrimSpace(t.Workspace), strings.TrimSpace(t.Schedule)
	t.Prompt, t.ID = strings.TrimSpace(t.Prompt), strings.TrimSpace(t.ID)
	if t.ID != "" {
		switch {
		case utf8.RuneCountInString(t.ID) > routineIDRunes:
			return errors.New("routine 的 id 太长")
		case t.Off == nil:
			return errors.New("带 id 的 routine 要写 off（true 暂停，false 恢复）")
		case t.Title != "" || t.Workspace != "" || t.Schedule != "" || t.Prompt != "":
			return errors.New("带 id 的 routine 只能暂停或恢复，改内容请用户在“自动化”里改")
		}
		return nil
	}
	switch {
	case t.Off != nil:
		return errors.New("off 只和 id 一起用")
	case t.Title == "":
		return errors.New("routine 缺少 title")
	case utf8.RuneCountInString(t.Title) > titleRunes:
		return fmt.Errorf("title 超过 %d 字", titleRunes)
	case t.Workspace == "":
		return errors.New("routine 缺少 workspace")
	case utf8.RuneCountInString(t.Workspace) > workspaceRefRunes:
		return errors.New("workspace 太长")
	case t.Schedule == "":
		return errors.New("routine 缺少 schedule（五段 cron 表达式，如 0 9 * * 1-5）")
	case len(t.Schedule) > routineScheduleBytes:
		return errors.New("schedule 太长")
	case t.Prompt == "":
		return errors.New("routine 缺少 prompt")
	case utf8.RuneCountInString(t.Prompt) > routinePromptRunes:
		return fmt.Errorf("prompt 超过 %d 字", routinePromptRunes)
	}
	return nil
}

// Front writes one line of work into the ledger (工作线台账, §15.17, D132):
// the front ID names is made (with Name) or changed field by field. State,
// Next and Waiting left out stay as they are; "" clears one.
type Front struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	State    *string  `json:"state,omitempty"`
	Next     *string  `json:"next,omitempty"`
	Waiting  *string  `json:"waiting,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Decision string   `json:"decision,omitempty"`
	Close    bool     `json:"close,omitempty"`
}

// Update is f as the ledger takes it.
func (f Front) Update() front.Update {
	return front.Update{ID: f.ID, Name: f.Name, State: f.State, Next: f.Next, Waiting: f.Waiting,
		Sessions: f.Sessions, Decision: f.Decision, Close: f.Close}
}

func (f *Front) validate() error {
	u := f.Update()
	if err := u.Validate(); err != nil {
		return err
	}
	f.ID, f.Name, f.Decision = u.ID, u.Name, u.Decision
	return nil
}

// Block is one action block of a reply: exactly one of Dispatch, Promise,
// Ask, Status, Notify, Custody, Relay, Remember, Lookup, Park, Halt, Cancel,
// Routine and Front is set, or Err says why it is not run.
type Block struct {
	// Raw is the block's body as written.
	Raw      string
	Type     Type
	Dispatch *Dispatch
	Promise  *Promise
	Ask      *Ask
	Status   *Status
	Notify   *Notify
	Custody  *Custody
	Relay    *Relay
	Remember *Remember
	Lookup   *Lookup
	Park     *Park
	Halt     *Halt
	Cancel   *Cancel
	Routine  *Routine
	Front    *Front
	Err      error
}

// errTooMany refuses the blocks past maxBlocks.
var errTooMany = fmt.Errorf("一次回复最多 %d 个动作块", maxBlocks)

// errUnclosed is a block whose closing fence never came.
var errUnclosed = errors.New("代码块没有结束（缺少结尾的 ```）")

// Parse returns text's action blocks in order.
func Parse(text string) []Block {
	var out []Block
	for _, f := range fences(strings.Split(text, "\n"), Fence) {
		b := Block{Raw: f.body}
		switch {
		case !f.closed:
			b.Err = errUnclosed
		case len(out) >= maxBlocks:
			b.Err = errTooMany
		default:
			b.decode()
		}
		out = append(out, b)
	}
	return out
}

// fence is one fenced block of a text: its body trimmed, whether its
// closing fence came, and the lines it spans, open being the opening
// fence's and end the line after the closing one (len(lines) when it never
// closed).
type fence struct {
	body      string
	closed    bool
	open, end int
}

// fences returns the blocks of lines opened by info, in order. A block runs
// to the first bare ``` line, so one never closed takes the rest of the
// text.
func fences(lines []string, info string) []fence {
	var out []fence
	for i := 0; i < len(lines); i++ {
		if !isOpen(lines[i], info) {
			continue
		}
		f := fence{open: i}
		var body []string
		for i++; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "```" {
				f.closed = true
				break
			}
			body = append(body, lines[i])
		}
		f.body, f.end = strings.TrimSpace(strings.Join(body, "\n")), min(i+1, len(lines))
		out = append(out, f)
	}
	return out
}

// isOpen reports whether line opens a block of info: the fence and the info
// string alone, indented or not.
func isOpen(line, info string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "```")
	return ok && strings.TrimSpace(rest) == info
}

func (b *Block) decode() {
	if len(b.Raw) > maxBlockBytes {
		b.Err = fmt.Errorf("动作块超过 %d KB", maxBlockBytes>>10)
		return
	}
	var head struct {
		Type Type `json:"type"`
	}
	if err := json.Unmarshal([]byte(b.Raw), &head); err != nil {
		b.Err = fmt.Errorf("不是一个 JSON 对象（%v）", err)
		return
	}
	b.Type = head.Type
	var err error
	switch head.Type {
	case TypeDispatch:
		var d struct {
			Type Type `json:"type"`
			Dispatch
		}
		if err = strict(b.Raw, &d); err == nil {
			err = d.validate()
			b.Dispatch = &d.Dispatch
		}
	case TypePromise:
		var p struct {
			Type      Type   `json:"type"`
			Text      string `json:"text"`
			Due       string `json:"due"`
			CloseWhen string `json:"close_when"`
		}
		if err = strict(b.Raw, &p); err == nil {
			pr := &Promise{Text: strings.TrimSpace(p.Text), CloseWhen: strings.TrimSpace(p.CloseWhen)}
			if pr.Due, err = promise.ParseDue(p.Due); err != nil {
				err = errors.New(`due 要是 RFC3339 时间（如 2026-11-05T09:00:00+08:00）或空`)
			} else {
				err = pr.validate()
			}
			b.Promise = pr
		}
	case TypeAsk:
		var a struct {
			Type Type `json:"type"`
			Ask
		}
		if err = strict(b.Raw, &a); err == nil {
			err = a.validate()
			b.Ask = &a.Ask
		}
	case TypeStatus:
		var st struct {
			Type Type `json:"type"`
			Status
		}
		if err = strict(b.Raw, &st); err == nil {
			err = st.validate()
			b.Status = &st.Status
		}
	case TypeNotify:
		var n struct {
			Type Type `json:"type"`
			Notify
		}
		if err = strict(b.Raw, &n); err == nil {
			err = n.validate()
			b.Notify = &n.Notify
		}
	case TypeCustody:
		var c struct {
			Type Type `json:"type"`
			Custody
		}
		if err = strict(b.Raw, &c); err == nil {
			err = c.validate()
			b.Custody = &c.Custody
		}
	case TypeRelay:
		var r struct {
			Type Type `json:"type"`
			Relay
		}
		if err = strict(b.Raw, &r); err == nil {
			err = r.validate()
			b.Relay = &r.Relay
		}
	case TypeRemember:
		var m struct {
			Type Type `json:"type"`
			Remember
		}
		if err = strict(b.Raw, &m); err == nil {
			err = m.validate()
			b.Remember = &m.Remember
		}
	case TypeLookup:
		var l struct {
			Type Type `json:"type"`
			Lookup
		}
		if err = strict(b.Raw, &l); err == nil {
			err = l.validate()
			b.Lookup = &l.Lookup
		}
	case TypePark:
		var pk struct {
			Type Type `json:"type"`
			Park
		}
		if err = strict(b.Raw, &pk); err == nil {
			err = pk.validate()
			b.Park = &pk.Park
		}
	case TypeHalt:
		var h struct {
			Type Type `json:"type"`
			Halt
		}
		if err = strict(b.Raw, &h); err == nil {
			err = h.validate()
			b.Halt = &h.Halt
		}
	case TypeCancel:
		var c struct {
			Type Type `json:"type"`
			Cancel
		}
		if err = strict(b.Raw, &c); err == nil {
			err = c.validate()
			b.Cancel = &c.Cancel
		}
	case TypeRoutine:
		var t struct {
			Type Type `json:"type"`
			Routine
		}
		if err = strict(b.Raw, &t); err == nil {
			err = t.validate()
			b.Routine = &t.Routine
		}
	case TypeFront:
		var f struct {
			Type Type `json:"type"`
			Front
		}
		if err = strict(b.Raw, &f); err == nil {
			err = f.validate()
			b.Front = &f.Front
		}
	case "":
		err = errors.New("缺少 type")
	default:
		err = fmt.Errorf("未知类型 %q（只有 %s）", head.Type, allTypes)
	}
	if err != nil {
		*b = Block{Raw: b.Raw, Type: b.Type, Err: err}
	}
}

// strict decodes raw, already one JSON object, into v, refusing a field v
// does not name.
func strict(raw string, v any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("字段不对（%v）", err)
	}
	return nil
}

func (d *Dispatch) validate() error {
	d.Workspace, d.Title = strings.TrimSpace(d.Workspace), strings.TrimSpace(d.Title)
	d.Model, d.Message = strings.TrimSpace(d.Model), strings.TrimSpace(d.Message)
	d.Goal = cut(strings.Join(strings.Fields(d.Goal), " "), goalRunes)
	switch {
	case d.Workspace == "":
		return errors.New("dispatch 缺少 workspace")
	case utf8.RuneCountInString(d.Workspace) > workspaceRefRunes:
		return errors.New("workspace 太长")
	case d.Title == "":
		return errors.New("dispatch 缺少 title")
	case utf8.RuneCountInString(d.Title) > titleRunes:
		return fmt.Errorf("title 超过 %d 字", titleRunes)
	case strings.IndexFunc(d.Title, unicode.IsControl) >= 0 || strings.HasPrefix(d.Title, "-"):
		return errors.New("title 不能含换行等控制字符，也不能以 - 开头")
	case d.Message == "":
		return errors.New("dispatch 缺少 message")
	case utf8.RuneCountInString(d.Message) > messageRunes:
		return fmt.Errorf("message 超过 %d 字", messageRunes)
	}
	if err := tuningspec.ValidateModel("model", d.Model); err != nil {
		return errors.New("model 不是合法的模型名")
	}
	if err := noEnvelope("message", d.Message); err != nil {
		return err
	}
	return validMCPTools(d.MCPTools)
}

// validMCPTools checks a dispatch's mcp_tools: only the write tools of a
// server with a write switch (scout.WriteSwitch), never the one that turns
// them all on. The other servers list their write tools without a switch,
// so naming one would give the session nothing it lacks.
func validMCPTools(tools map[string][]string) error {
	for _, server := range slices.Sorted(maps.Keys(tools)) {
		if _, ok := scout.WriteSwitch[server]; !ok {
			return fmt.Errorf("mcp_tools 只能放行 %s 的写工具，%q 不行", strings.Join(slices.Sorted(maps.Keys(scout.WriteSwitch)), "、"), server)
		}
		known := scout.WriteTools(server)
		known = slices.DeleteFunc(known, func(t string) bool { return t == scout.EnableWrites })
		if len(tools[server]) == 0 {
			return fmt.Errorf("mcp_tools 里 %s 没有工具", server)
		}
		for _, t := range tools[server] {
			if !slices.Contains(known, t) {
				return fmt.Errorf("mcp_tools 里 %s 没有写工具 %q（可放行：%s）", server, t, strings.Join(known, "、"))
			}
		}
	}
	return nil
}

// envelopeTag opens and closes the gateway's relay envelope (relayEnvelope).
const envelopeTag = "naozhi-relay"

// envelopeLike is the envelope's tag however it is spelled: its two words
// joined by one dash, connector or space ("naozhi_relay", the gateway's own
// escape of it; U+2010 or a space for the hyphen), matched on
// foldLookalike's text. Only one: the repo's name and the word relay apart
// ("naozhi/relay", "naozhi\nRelay") are ordinary text, not a tag.
var envelopeLike = regexp.MustCompile(`(?i)naozhi[\p{Pd}\p{Pc}\p{Zs}]relay`)

// lookalikeLetters maps the Cyrillic and Greek letters drawn like the
// tag's own (а for a, е for e) to them: a tag spelled with them reads as the
// tag.
var lookalikeLetters = map[rune]rune{
	'а': 'a', 'А': 'a', 'α': 'a', 'Α': 'a',
	'е': 'e', 'Е': 'e', 'Ε': 'e',
	'һ': 'h', 'Һ': 'h', 'Н': 'h', 'Η': 'h',
	'і': 'i', 'І': 'i', 'ι': 'i', 'Ι': 'i', 'ı': 'i',
	'ӏ': 'l', 'Ӏ': 'l',
	'Ν': 'n',
	'о': 'o', 'О': 'o', 'ο': 'o', 'Ο': 'o',
	'г': 'r',
	'у': 'y', 'У': 'y', 'ү': 'y', 'Υ': 'y',
	'Ζ': 'z',
}

// foldLookalike drops the invisible format characters (zero-width space,
// joiners, soft hyphen), maps the full-width ASCII forms to ASCII and the
// look-alike letters to the tag's, so a tag spelled with them reads as the
// tag.
func foldLookalike(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		}
		if l, ok := lookalikeLetters[r]; ok {
			return l
		}
		return r
	}, s)
}

// noEnvelope refuses Planner text that names the relay envelope, in a
// spelling a reader takes for it: only the gateway writes one, so a session
// can take it for the user's own words. In 11-07 runs a copy with the
// brackets swapped for ‹› still passed for the real one 9 times in 13, so a
// look-alike tag is the forgery as much as the tag itself. The error quotes
// what matched, so the Planner knows what to take out.
func noEnvelope(field, text string) error {
	folded := foldLookalike(text)
	if loc := envelopeLike.FindStringIndex(folded); loc != nil {
		return fmt.Errorf("%s 里不能出现 %q（像 %s，只有网关写它）", field, folded[loc[0]:loc[1]], envelopeTag)
	}
	return nil
}

// validKey checks a block's session key field.
func validKey(field, key string) error {
	switch {
	case key == "":
		return fmt.Errorf("缺少 %s", field)
	case utf8.RuneCountInString(key) > keyRunes || strings.IndexFunc(key, unicode.IsControl) >= 0:
		return fmt.Errorf("%s 不是合法的会话 key", field)
	}
	return nil
}

var memoryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (m *Remember) validate() error {
	m.Workspace, m.Name, m.Text = strings.TrimSpace(m.Workspace), strings.TrimSpace(m.Name), strings.TrimSpace(m.Text)
	switch {
	case m.Workspace == "":
		return errors.New("remember 缺少 workspace（工作区 ID、名字，或 global）")
	case utf8.RuneCountInString(m.Workspace) > workspaceRefRunes:
		return errors.New("workspace 太长")
	case !memoryNameRE.MatchString(m.Name):
		return errors.New("remember 的 name 只能用小写字母、数字、- 和 _，以字母或数字开头，不超过 64 个")
	case m.Text == "":
		return errors.New("remember 缺少 text")
	case utf8.RuneCountInString(m.Text) > rememberTextRunes:
		return fmt.Errorf("text 超过 %d 字", rememberTextRunes)
	}
	return nil
}

func (r *Relay) validate() error {
	r.Key, r.Text = strings.TrimSpace(r.Key), strings.TrimSpace(r.Text)
	if err := validKey("key", r.Key); err != nil {
		return errors.New("relay " + err.Error())
	}
	switch {
	case r.Text == "":
		return errors.New("relay 缺少 text（你要它做什么）")
	case utf8.RuneCountInString(r.Text) > relayTextRunes:
		return fmt.Errorf("text 超过 %d 字", relayTextRunes)
	}
	return noEnvelope("text", r.Text)
}

func (p *Promise) validate() error {
	switch {
	case p.Text == "":
		return errors.New("promise 缺少 text")
	case utf8.RuneCountInString(p.Text) > textRunes:
		return fmt.Errorf("text 超过 %d 字", textRunes)
	case utf8.RuneCountInString(p.CloseWhen) > textRunes:
		return fmt.Errorf("close_when 超过 %d 字", textRunes)
	}
	return nil
}

func (a *Ask) validate() error {
	a.Question = strings.TrimSpace(a.Question)
	switch {
	case a.Question == "":
		return errors.New("ask 缺少 question")
	case utf8.RuneCountInString(a.Question) > textRunes:
		return fmt.Errorf("question 超过 %d 字", textRunes)
	case len(a.Options) > maxOptions:
		return fmt.Errorf("options 最多 %d 个", maxOptions)
	}
	// The question and the option picked go into the relay envelope with
	// the answer (relayEnvelope), there or from the answer's turn.
	if err := noEnvelope("question", a.Question); err != nil {
		return err
	}
	for _, o := range a.Options {
		if err := noEnvelope("options", o); err != nil {
			return err
		}
	}
	// Duplicates are judged on the options as written; two that differ only
	// past the cut would read the same on the card, which only a shorter
	// option fixes.
	raw := make([]string, len(a.Options))
	for i, o := range a.Options {
		o = strings.TrimSpace(o)
		raw[i] = o
		cut := utf8.RuneCountInString(o) > optionRunes
		if cut {
			o = textutil.TruncateRunesNoEllipsis(o, optionRunes-1) + "…"
			a.Trimmed++
		}
		switch {
		case o == "":
			return errors.New("options 里有空的选项")
		case slices.Contains(raw[:i], raw[i]):
			return fmt.Errorf("选项 %q 重复", raw[i])
		}
		if j := slices.Index(a.Options[:i], o); j >= 0 {
			long, other := i, j
			if !cut {
				long, other = j, i
			}
			return fmt.Errorf("第 %d 个选项超过 %d 字，截断后与第 %d 个相同，请缩短", long+1, optionRunes, other+1)
		}
		a.Options[i] = o
	}
	a.Task = strings.TrimSpace(a.Task)
	if a.Task == "" {
		return nil
	}
	if err := validKey("task", a.Task); err != nil {
		return errors.New("ask 的 " + err.Error())
	}
	if len(a.Options) == 0 {
		return errors.New("带 task 的 ask 要给 options（用户点选的回答直接送到那个会话）")
	}
	return nil
}

func (s *Status) validate() error {
	s.Task, s.State, s.Why = strings.TrimSpace(s.Task), strings.TrimSpace(s.State), strings.TrimSpace(s.Why)
	switch {
	case s.Task == "":
		return errors.New("status 缺少 task")
	case utf8.RuneCountInString(s.Task) > keyRunes:
		return errors.New("task 太长")
	case s.State != "done" && s.State != "doing" && s.State != "failed":
		return fmt.Errorf("state 只能是 done、doing 或 failed，不是 %q", s.State)
	case s.Why == "":
		return errors.New("status 缺少 why（判断的依据）")
	case utf8.RuneCountInString(s.Why) > textRunes:
		return fmt.Errorf("why 超过 %d 字", textRunes)
	}
	return nil
}

func (n *Notify) validate() error {
	n.Title = strings.Join(strings.Fields(n.Title), " ")
	n.Why, n.Level, n.Link, n.ID = strings.TrimSpace(n.Why), strings.TrimSpace(n.Level), strings.TrimSpace(n.Link), strings.TrimSpace(n.ID)
	switch {
	case n.Title == "":
		return errors.New("notify 缺少 title")
	case utf8.RuneCountInString(n.Title) > notifyTitleRunes:
		return fmt.Errorf("title 超过 %d 字", notifyTitleRunes)
	case utf8.RuneCountInString(n.Why) > textRunes:
		return fmt.Errorf("why 超过 %d 字", textRunes)
	case n.Level != LevelTimely && n.Level != LevelBrief:
		return fmt.Errorf("level 只能是 timely 或 brief，不是 %q", n.Level)
	case utf8.RuneCountInString(n.ID) > notifyIDRunes:
		return errors.New("id 太长")
	case n.Link != "" && !webLink(n.Link):
		return errors.New("link 要是 http(s) 链接或空")
	}
	return nil
}

func (c *Custody) validate() error {
	c.Key, c.Text = strings.TrimSpace(c.Key), strings.TrimSpace(c.Text)
	switch {
	case c.Key == "":
		return errors.New("custody 缺少 key")
	case utf8.RuneCountInString(c.Key) > keyRunes || strings.IndexFunc(c.Key, unicode.IsControl) >= 0:
		return errors.New("key 不是合法的会话 key")
	case btoi(c.On != nil)+btoi(c.Text != "")+btoi(c.Watch != nil) != 1:
		return errors.New("custody 带 on（开关托管）、text（让它继续）、watch（关掉观察）三者之一，只能带一个")
	case c.Watch != nil && *c.Watch:
		return errors.New("watch 只能是 false（关掉观察）；观察要用户在会话头部重新打开")
	case utf8.RuneCountInString(c.Text) > custodyTextRunes:
		return fmt.Errorf("text 超过 %d 字", custodyTextRunes)
	}
	return noEnvelope("text", c.Text)
}

// btoi is 1 for true.
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cut is s cut to n runes, "…" included, when it runs longer.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return textutil.TruncateRunesNoEllipsis(s, n-1) + "…"
}

// webLink reports whether s is an absolute http(s) URL the dashboard may
// open.
func webLink(s string) bool {
	if len(s) > linkBytes || strings.IndexFunc(s, unicode.IsSpace) >= 0 || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
