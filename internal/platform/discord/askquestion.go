package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/textutil"

	"github.com/bwmarrin/discordgo"
)

// askCustomIDPrefix marks naozhi's AskUserQuestion buttons. The full id is
// "naq:<option index>:<agent id>"; the index only keeps ids unique.
const askCustomIDPrefix = "naq:"

// Discord message limits a question card must respect; past them the send is
// rejected, so SendQuestionCard errors and dispatch falls back to text.
const (
	discordButtonLabelMaxRunes = 80
	discordCustomIDMaxBytes    = 100
	discordButtonsPerRow       = 5
	discordMaxButtons          = 25
	discordEmbedDescMaxRunes   = 4096
)

// discordInteractionTimeout bounds the interaction response; Discord expires
// the interaction token's initial response after 3 seconds anyway.
const discordInteractionTimeout = 3 * time.Second

// SendQuestionCard posts an AskUserQuestion prompt as an embed. A single
// question gets one button per option (one click is the full answer); several
// questions get a read-only embed asking for one text reply.
func (d *Discord) SendQuestionCard(ctx context.Context, chatID string, card platform.QuestionCard) (string, error) {
	ms, err := buildQuestionMessage(card)
	if err != nil {
		return "", fmt.Errorf("discord question card: %w", err)
	}
	m, err := d.session.ChannelMessageSendComplex(chatID, ms, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("discord send question card: %w", err)
	}
	return platform.EncodeMessageRef(chatID, m.ID), nil
}

// buildQuestionMessage renders card. The click handler reads the answer back
// from this message, so the embed title must be the raw header and each
// button label the untruncated option label: a label Discord cannot show in
// full is an error rather than a silently shortened answer.
func buildQuestionMessage(card platform.QuestionCard) (*discordgo.MessageSend, error) {
	if len(card.Items) == 0 {
		return nil, errors.New("no items")
	}
	ms := &discordgo.MessageSend{
		Content:         "**Claude 想请你确认**",
		AllowedMentions: noMentions(),
	}
	if len(card.Items) > 1 {
		desc := escapeMarkdown(platform.RenderAskQuestionPlain(card.Items))
		if utf8.RuneCountInString(desc) > discordEmbedDescMaxRunes {
			return nil, errors.New("questions exceed the embed description limit")
		}
		ms.Embeds = []*discordgo.MessageEmbed{{
			Title:       "请在一条消息里一次回复全部问题",
			Description: desc,
		}}
		return ms, nil
	}

	item := card.Items[0]
	if len(item.Options) > discordMaxButtons {
		return nil, fmt.Errorf("%d options exceed the %d-button limit", len(item.Options), discordMaxButtons)
	}
	question := escapeMarkdown(item.Question)
	if utf8.RuneCountInString(question) > discordEmbedDescMaxRunes {
		return nil, errors.New("question exceeds the embed description limit")
	}
	agentID := textutil.TruncateRunesNoEllipsis(card.AgentID, platform.AskIDMaxRunes)
	var desc strings.Builder
	desc.WriteString(question)
	var rows []discordgo.MessageComponent
	var row discordgo.ActionsRow
	for i, opt := range item.Options {
		if opt.Label == "" || utf8.RuneCountInString(opt.Label) > discordButtonLabelMaxRunes {
			return nil, fmt.Errorf("option %d label does not fit a %d-rune button", i, discordButtonLabelMaxRunes)
		}
		id := askCustomIDPrefix + strconv.Itoa(i) + ":" + agentID
		if len(id) > discordCustomIDMaxBytes {
			return nil, fmt.Errorf("option %d custom_id is %d bytes, over the %d-byte limit", i, len(id), discordCustomIDMaxBytes)
		}
		desc.WriteString("\n**" + escapeMarkdown(opt.Label) + "**")
		if opt.Description != "" {
			desc.WriteString(" — " + escapeMarkdown(opt.Description))
		}
		row.Components = append(row.Components, discordgo.Button{
			Label: opt.Label, Style: discordgo.PrimaryButton, CustomID: id,
		})
		if len(row.Components) == discordButtonsPerRow {
			rows = append(rows, row)
			row = discordgo.ActionsRow{}
		}
	}
	if len(row.Components) > 0 {
		rows = append(rows, row)
	}
	// The hint keeps the card usable as a prompt even when a click fails.
	hint := "也可以直接回复文字作答。"
	if item.MultiSelect {
		hint = "可多选：每个按钮只提交一项，选多项请直接回复文字。"
	}
	ms.Embeds = []*discordgo.MessageEmbed{{
		Title: textutil.TruncateRunesNoEllipsis(item.Header, platform.AskHeaderMaxRunes),
		// The question fits; only the option list can push past the limit.
		Description: textutil.TruncateRunes(desc.String(), discordEmbedDescMaxRunes-3),
		Footer:      &discordgo.MessageEmbedFooter{Text: hint},
	}}
	ms.Components = rows
	return ms, nil
}

// markdownEscaper backslash-escapes the characters Discord markdown treats as
// formatting, links or mentions; Discord renders "\x" as "x" for punctuation.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`,
	`>`, `\>`, `<`, `\<`, `#`, `\#`, `-`, `\-`, `[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`, `@`, `\@`,
)

func escapeMarkdown(s string) string { return markdownEscaper.Replace(s) }

// onInteractionCreate answers an AskUserQuestion button click. Everything it
// trusts comes from the bot's own card as Discord delivers it: the label from
// the clicked button, the header from the embed title, the agent id from the
// button's custom_id. The card is replaced by the answer in the interaction
// response, which also stops a second click.
func (d *Discord) onInteractionCreate(_ *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic == nil || ic.Interaction == nil || ic.Type != discordgo.InteractionMessageComponent {
		return
	}
	i := ic.Interaction
	data, ok := i.Data.(discordgo.MessageComponentInteractionData)
	if !ok || !strings.HasPrefix(data.CustomID, askCustomIDPrefix) {
		return
	}
	if i.Message == nil || i.Message.Author == nil || i.ChannelID == "" {
		return
	}
	botID := d.getBotID()
	if botID == "" {
		// Unknown identity: the card cannot be told apart from another bot's,
		// so the click fails visibly and the user can answer in text.
		d.maybeHealBotID()
		return
	}
	if i.Message.Author.ID != botID {
		return
	}
	_, agentID, _ := strings.Cut(strings.TrimPrefix(data.CustomID, askCustomIDPrefix), ":")
	header := ""
	if len(i.Message.Embeds) > 0 && i.Message.Embeds[0] != nil {
		header = i.Message.Embeds[0].Title
	}
	text := platform.ComposeAskAnswerText(platform.AskAnswerPayload{
		Header: header, Label: buttonLabel(i.Message.Components, data.CustomID),
	})
	userID := interactionUserID(i)
	if text == "" || userID == "" {
		return
	}
	chatType := "direct"
	if i.GuildID != "" {
		chatType = "group"
	}
	msg := platform.IncomingMessage{
		Platform: "discord",
		// Gateway replays and a second click by the same user collapse.
		EventID:   "component:" + i.ChannelID + ":" + i.Message.ID + ":" + userID,
		MessageID: platform.EncodeMessageRef(i.ChannelID, i.Message.ID),
		UserID:    userID,
		ChatID:    i.ChannelID,
		ChatType:  chatType,
		Text:      text,
		// The dispatcher whitelist-validates AgentID before routing (#2148).
		AgentID: osutil.SanitizeForLog(agentID, platform.AskIDMaxRunes),
		// An explicit click on the bot's card bypasses mention_only gating.
		MentionMe: true,
	}
	// A dropped answer gets no response: Discord shows the click as failed
	// and the buttons stay for another try.
	if !d.dispatch.TryGo("discord", func() { d.handler(d.stopCtx, msg) },
		"channel", msg.ChatID, "user", msg.UserID) {
		return
	}
	d.markQuestionAnswered(i, text)
}

// markQuestionAnswered replaces the card with the chosen answer and removes
// its buttons. Best effort; the answer is already on its way.
func (d *Discord) markQuestionAnswered(i *discordgo.Interaction, answer string) {
	parent := d.stopCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, discordInteractionTimeout)
	defer cancel()
	err := d.session.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:         "✅ 已回答：" + escapeMarkdown(osutil.SanitizeForLog(answer, 1024)),
			Components:      []discordgo.MessageComponent{},
			Embeds:          []*discordgo.MessageEmbed{},
			AllowedMentions: noMentions(),
		},
	}, discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false))
	if err != nil {
		slog.Debug("discord interaction: edit question card failed",
			"channel", i.ChannelID, "err", probeReason(err))
	}
}

// noMentions allows no pings: an explicit empty parse list, not null.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

// buttonLabel finds the label of the button with customID in the message's
// action rows, or "".
func buttonLabel(components []discordgo.MessageComponent, customID string) string {
	for _, c := range components {
		var row *discordgo.ActionsRow
		switch r := c.(type) {
		case *discordgo.ActionsRow:
			row = r
		case discordgo.ActionsRow:
			row = &r
		default:
			continue
		}
		for _, e := range row.Components {
			switch b := e.(type) {
			case *discordgo.Button:
				if b.CustomID == customID {
					return b.Label
				}
			case discordgo.Button:
				if b.CustomID == customID {
					return b.Label
				}
			}
		}
	}
	return ""
}

// interactionUserID is the clicking user: Member.User in a guild, User in a DM.
func interactionUserID(i *discordgo.Interaction) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
