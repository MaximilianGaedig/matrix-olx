// matrix-olx - A Matrix-OLX puppeting bridge.
// Copyright (C) 2026 Maximilian Gaedig
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

// Closed questions. OLX asks one of the two people something and offers a
// few answers as buttons. On Matrix the question is a notice with the same
// buttons, each of which sends the answer command; the command can be typed
// just as well.

const answerCommand = "answer"

// questionNote is what OLX's web app says under a question that names no
// note of its own.
const questionNote = "Only visible to you."

func optionTexts(question *olxapi.QuestionExtras) string {
	texts := make([]string, len(question.Options))
	for i, option := range question.Options {
		texts[i] = option.Text
	}
	return strings.Join(texts, ", ")
}

// questionText says what a closed question shows.
func questionText(question *olxapi.QuestionExtras, prefix string) string {
	lines := []string{strings.TrimSpace(question.Text)}
	if detail := strings.TrimSpace(question.DetailedText); detail != "" && detail != lines[0] {
		lines = append(lines, detail)
	}
	note := strings.TrimSpace(question.PrivacyNote)
	if note == "" {
		note = questionNote
	}
	lines = append(lines, note, "Answers: "+optionTexts(question)+"\nTo answer: `"+prefix+" "+answerCommand+" <answer>`")
	return strings.Join(lines, "\n")
}

// questionButtons has a button per answer, laid out as on OLX: two side by
// side, more than two one under another.
func questionButtons(messageID string, question *olxapi.QuestionExtras, prefix string) *keyboard {
	var rows [][]button
	for _, option := range question.Options {
		b := button{
			Text:    option.Text,
			Type:    "callback",
			Command: prefix + " " + answerCommand + " " + messageID + " " + option.Key(),
		}
		if len(question.Options) > 2 || len(rows) == 0 {
			rows = append(rows, []button{b})
		} else {
			rows[0] = append(rows[0], b)
		}
	}
	return &keyboard{Keyboard: "inline", Rows: rows}
}

func (c *OLXClient) convertQuestion(msg *olxapi.Message, question *olxapi.QuestionExtras) *bridgev2.ConvertedMessage {
	prefix := c.Main.commandPrefix()
	return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
		Type: event.EventMessage,
		Content: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    questionText(question, prefix),
		},
		Extra: map[string]any{buttonsField: questionButtons(msg.ID, question, prefix)},
	}}}
}

// pickQuestion finds the question an answer command is about and the answer
// it names. A button names the question by its message; a typed command
// means the newest question of the chat.
func pickQuestion(msgs []*olxapi.Message, args []string) (*olxapi.Message, *olxapi.QuestionExtras, string) {
	msgs = append([]*olxapi.Message(nil), msgs...)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.After(msgs[j].CreatedAt.Time) })
	var newest *olxapi.Message
	var newestQuestion *olxapi.QuestionExtras
	for _, msg := range msgs {
		question, ok := olxapi.ParseQuestionExtras(msg)
		if !ok {
			continue
		}
		if len(args) > 1 && msg.ID == args[0] {
			return msg, question, strings.Join(args[1:], " ")
		}
		if newest == nil {
			newest, newestQuestion = msg, question
		}
	}
	return newest, newestQuestion, strings.Join(args, " ")
}

var cmdAnswer = &commands.FullHandler{
	Func: fnAnswer,
	Name: answerCommand,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Answer the question OLX asked in this chat. The buttons under the question do the same.",
		Args:        "<_answer_>",
	},
	RequiresLogin:  true,
	RequiresPortal: true,
}

func fnAnswer(ce *commands.Event) {
	client := portalClient(ce)
	if client == nil {
		return
	}
	conv, err := client.API.GetConversation(ce.Ctx, string(ce.Portal.ID))
	if err != nil {
		ce.Reply("Failed to look at the chat's questions: %v", client.checkErr(err))
		return
	} else if conv == nil {
		ce.Reply("The chat no longer exists on OLX.")
		return
	}
	msg, question, name := pickQuestion(conv.Messages, ce.Args)
	if question == nil {
		ce.Reply("OLX has asked no question in this chat.")
		return
	}
	option := question.Option(name)
	if option == nil {
		ce.Reply("“%s” takes one of these answers: %s\nUsage: `$cmdprefix %s <answer>`", strings.TrimSpace(question.Text), optionTexts(question), answerCommand)
		return
	}
	if err = client.API.AnswerQuestion(ce.Ctx, conv.ID, msg.ID, question, option); err != nil {
		ce.Reply("OLX did not take the answer: %v", client.checkErr(err))
		return
	}
	ce.Reply("Answered “%s” with “%s”.", strings.TrimSpace(question.Text), option.Text)
}
