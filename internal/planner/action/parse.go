// Package action is the Planner's way to ask the gateway for something
// (docs/rfc/planner.md §6.2): the ```naozhi-action blocks at the end of a
// planner:main reply. v0.2.0 has three: dispatch (a confirmation card; once
// approved, a task: session), promise (登记答应你的) and ask (a question
// in "等你" whose answer goes back to the Planner). A block that does not
// parse, or names an unknown type or a bad field, is not run: the Planner
// conversation shows it as it was written, with the reason.
package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// Fence is the info string that opens an action block.
const Fence = "naozhi-action"

// Limits on what a block may carry.
const (
	// maxBlocks is how many blocks one reply may run; the rest are refused.
	maxBlocks = 5
	// maxBlockBytes caps one block's body.
	maxBlockBytes = 64 << 10
	// titleRunes is the CLI's --name cap.
	titleRunes        = 64
	messageRunes      = 20000
	textRunes         = 500
	maxOptions        = 6
	optionRunes       = 40
	workspaceRefRunes = 128
)

// Type is a block's "type".
type Type string

const (
	TypeDispatch Type = "dispatch"
	TypePromise  Type = "promise"
	TypeAsk      Type = "ask"
)

// Dispatch asks for a session in a workspace, once the user approves.
type Dispatch struct {
	// Workspace is a workspace ID or name (workspaces.json).
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
	// Model is "" for the default.
	Model   string `json:"model"`
	Message string `json:"message"`
}

// Promise registers something the Planner promised the user.
type Promise struct {
	Text      string    `json:"text"`
	Due       time.Time `json:"-"`
	CloseWhen string    `json:"close_when"`
}

// Ask puts a question to the user; Options are the answers offered (none:
// the user replies in the conversation).
type Ask struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

// Block is one action block of a reply: exactly one of Dispatch, Promise
// and Ask is set, or Err says why it is not run.
type Block struct {
	// Raw is the block's body as written.
	Raw      string
	Type     Type
	Dispatch *Dispatch
	Promise  *Promise
	Ask      *Ask
	Err      error
}

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
			b.Err = fmt.Errorf("一次回复最多 %d 个动作块", maxBlocks)
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
	case "":
		err = errors.New("缺少 type")
	default:
		err = fmt.Errorf("未知类型 %q（只有 dispatch、promise、ask）", head.Type)
	}
	if err != nil {
		b.Dispatch, b.Promise, b.Ask, b.Err = nil, nil, nil, err
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
	return nil
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
		switch {
		case o == "":
			return errors.New("options 里有空的选项")
		case utf8.RuneCountInString(o) > optionRunes:
			return fmt.Errorf("选项超过 %d 字", optionRunes)
		case slices.Contains(a.Options[:i], o):
			return fmt.Errorf("选项 %q 重复", o)
		}
		a.Options[i] = o
	}
	return nil
}
