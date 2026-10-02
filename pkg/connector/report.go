// mautrix-olx - A Matrix-OLX puppeting bridge.
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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

// Reporting the other person of a chat to OLX's moderators, which the website
// does from the flag in the chat's header: a reason from OLX's list and, for
// some reasons, a description.
//
// It is a command and deliberately has no buttons: a report is about a real
// person and goes to real moderators, so it takes typing the reason, not one
// stray click.

const reportCommand = "report"

// maxReportDescription is the longest description OLX's form accepts.
const maxReportDescription = 5000

var cmdReport = &commands.FullHandler{
	Func: fnReport,
	Name: reportCommand,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Report the other person in this chat to OLX. Without a reason, lists the reasons OLX offers.",
		Args:        "[_reason_] [_description_]",
	},
	RequiresLogin:  true,
	RequiresPortal: true,
}

// reportRoles sorts the two people of a chat into buyer and seller, which is
// how a report names them.
func reportRoles(conv *olxapi.Conversation) (buyer, seller json.Number, err error) {
	self, other := json.Number(conv.UserID), json.Number(conv.Respondent.ID)
	if self == "" || other == "" {
		return "", "", errors.New("the chat does not say who is who")
	}
	switch conv.Respondent.Type {
	case "seller":
		return self, other, nil
	case "buyer":
		return other, self, nil
	default:
		return "", "", fmt.Errorf("the chat does not say whether %s is the buyer or the seller", conv.Respondent.Name)
	}
}

func formatReasons(reasons []*olxapi.ReportReason) string {
	var out strings.Builder
	for _, reason := range reasons {
		fmt.Fprintf(&out, "* `%s` – %s", reason.Key, reason.Label)
		if reason.Description != "" {
			out.WriteString(": " + reason.Description)
		}
		if reason.NeedsDescription {
			out.WriteString(" (needs a description)")
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func fnReport(ce *commands.Event) {
	client := portalClient(ce)
	if client == nil {
		return
	}
	reasons, err := client.API.GetReportReasons(ce.Ctx)
	if err != nil {
		ce.Reply("Failed to get OLX's list of reasons: %v", client.checkErr(err))
		return
	}
	if len(ce.Args) == 0 {
		ce.Reply("To report the other person in this chat to OLX, send `$cmdprefix %s <reason> [description]` with one of:\n\n%s", reportCommand, formatReasons(reasons))
		return
	}
	var reason *olxapi.ReportReason
	for _, candidate := range reasons {
		if strings.EqualFold(candidate.Key, ce.Args[0]) {
			reason = candidate
		}
	}
	if reason == nil {
		ce.Reply("`%s` is not one of OLX's reasons:\n\n%s", ce.Args[0], formatReasons(reasons))
		return
	}
	description := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ce.RawArgs), ce.Args[0]))
	if reason.NeedsDescription && description == "" {
		ce.Reply("OLX wants a description for this reason: `$cmdprefix %s %s <what happened>`", reportCommand, reason.Key)
		return
	}
	if len([]rune(description)) > maxReportDescription {
		ce.Reply("The description is too long: OLX takes up to %d characters.", maxReportDescription)
		return
	}
	conv, err := client.API.GetConversation(ce.Ctx, string(ce.Portal.ID))
	if err != nil {
		ce.Reply("Failed to get the chat: %v", client.checkErr(err))
		return
	} else if conv == nil {
		ce.Reply("The chat no longer exists on OLX.")
		return
	}
	buyer, seller, err := reportRoles(conv)
	if err != nil {
		ce.Reply("Can't report: %v.", err)
		return
	}
	ad := adID(conv)
	if ad == "" {
		ce.Reply("Can't report: this chat is not about an ad.")
		return
	}
	err = client.API.ReportChat(ce.Ctx, &olxapi.Report{
		AdID:        json.Number(ad),
		BuyerID:     buyer,
		SellerID:    seller,
		ReasonType:  reason.Key,
		Description: description,
	})
	if err != nil {
		ce.Reply("OLX did not take the report: %v", client.checkErr(err))
		return
	}
	ce.Reply("Reported %s to OLX for “%s”.", conv.Respondent.Name, reason.Label)
}
