// Package action is the Planner's way to ask the gateway for something
// (docs/rfc/planner.md §6.2): the ```naozhi-action blocks at the end of a
// planner:main reply. There are seven: dispatch (a task: session, started at
// once, D88), promise (a promise among the 任务), ask (a
// question in "通知" whose answer goes back to the Planner, or straight to a
// session, D98), status (the Planner's judgment of a task's state, §15.4),
// notify (something to tell the user, pushed now or held for the morning
// brief, §15.6), custody (a session's custody switched, or a message sent to
// a session in custody, planner/custody) and relay (the user's decision
// passed on to a session with the user's own words, D98). A block that
// does not parse, or names an unknown type, a bad field or a task that does
// not exist, is not run: the Planner conversation shows it as it was
// written, with the reason, and the Planner gets a receipt naming it.
package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/scout"
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
	taskRefRunes      = 128
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
)

// Dispatch asks for a session in a workspace.
type Dispatch struct {
	// Workspace is a workspace ID or name (workspaces.json).
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
	// Model is "" for the default.
	Model   string `json:"model"`
	Message string `json:"message"`
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

// Custody switches session Key's custody (On), or sends it Text through
// custody as the Planner's go-ahead; exactly one of the two.
type Custody struct {
	Key  string `json:"key"`
	On   *bool  `json:"on"`
	Text string `json:"text"`
}

// Relay passes the user's decision on to session Key (D98): the gateway
// leads Text, the Planner's words, with all the user's own words it recorded
// for the turn, never a part the Planner picks (a cut quote can turn "先不要
// 合并" into "合并"). Only a turn the user started may relay.
type Relay struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// Block is one action block of a reply: exactly one of Dispatch, Promise,
// Ask, Status, Notify, Custody and Relay is set, or Err says why it is not
// run.
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
	Err      error
}

// errTooMany refuses the blocks past maxBlocks.
var errTooMany = fmt.Errorf("一次回复最多 %d 个动作块", maxBlocks)

// Parse returns text's action blocks in order.
func Parse(text string) []Block {
	var out []Block
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !isOpen(lines[i]) {
			continue
		}
		var body []string
		closed := false
		for i++; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "```" {
				closed = true
				break
			}
			body = append(body, lines[i])
		}
		b := Block{Raw: strings.TrimSpace(strings.Join(body, "\n"))}
		switch {
		case !closed:
			b.Err = errors.New("代码块没有结束（缺少结尾的 ```）")
		case len(out) >= maxBlocks:
			b.Err = errTooMany
		default:
			b.decode()
		}
		out = append(out, b)
	}
	return out
}

// isOpen reports whether line opens an action block: the fence and the info
// string alone, indented or not.
func isOpen(line string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "```")
	return ok && strings.TrimSpace(rest) == Fence
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
	case "":
		err = errors.New("缺少 type")
	default:
		err = fmt.Errorf("未知类型 %q（只有 dispatch、promise、ask、status、notify、custody、relay）", head.Type)
	}
	if err != nil {
		b.Dispatch, b.Promise, b.Ask, b.Status, b.Notify, b.Custody, b.Relay, b.Err = nil, nil, nil, nil, nil, nil, nil, err
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

// noEnvelope refuses Planner text that names the relay envelope: only the
// gateway writes one, so a session can take it for the user's own words. In
// 11-07 runs a copy with the brackets swapped for ‹› still passed for the
// real one 9 times in 13.
func noEnvelope(field, text string) error {
	if strings.Contains(strings.ToLower(text), envelopeTag) {
		return fmt.Errorf("%s 里不能出现 %s（只有网关写它）", field, envelopeTag)
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
	for i, o := range a.Options {
		o = strings.TrimSpace(o)
		if utf8.RuneCountInString(o) > optionRunes {
			o = textutil.TruncateRunesNoEllipsis(o, optionRunes-1) + "…"
			a.Trimmed++
		}
		switch {
		case o == "":
			return errors.New("options 里有空的选项")
		case slices.Contains(a.Options[:i], o):
			return fmt.Errorf("选项 %q 重复", o)
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
	case utf8.RuneCountInString(s.Task) > taskRefRunes:
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
	case (c.On == nil) == (c.Text == ""):
		return errors.New("custody 要么带 on（开关托管），要么带 text（让它继续），只能二选一")
	case utf8.RuneCountInString(c.Text) > custodyTextRunes:
		return fmt.Errorf("text 超过 %d 字", custodyTextRunes)
	}
	return noEnvelope("text", c.Text)
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
