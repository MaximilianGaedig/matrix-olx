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
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

// Price proposals. On OLX they are widgets in the chat with an Accept button
// and a field for another price. On Matrix a widget becomes a notice with the
// same buttons, in the format clients already render for the Telegram bridge:
// a button is a command that the client sends into the room when it is
// pressed. The commands can be typed just as well.

// buttonsField is the content field clients read a message's buttons from.
const buttonsField = "fi.mau.telegram.buttons"

const (
	offerCommand  = "offer"
	acceptCommand = "accept-offer"
)

type button struct {
	Text     string `json:"text"`
	Type     string `json:"type"`
	Command  string `json:"command,omitempty"`
	URL      string `json:"url,omitempty"`
	CopyText string `json:"copy_text,omitempty"`
}

type keyboard struct {
	MessageID int        `json:"message_id"`
	Keyboard  string     `json:"keyboard"`
	Rows      [][]button `json:"rows"`
}

func (oc *OLXConnector) commandPrefix() string {
	if oc.Bridge != nil && oc.Bridge.Config != nil && oc.Bridge.Config.CommandPrefix != "" {
		return oc.Bridge.Config.CommandPrefix
	}
	return "!olx"
}

var proposalStates = map[string]string{
	olxapi.ProposalPending:  "waiting for an answer",
	olxapi.ProposalAccepted: "accepted",
	olxapi.ProposalReplaced: "replaced by a newer proposal",
}

// negotiationText says what a price negotiation widget shows.
func negotiationText(msgType string, extras *olxapi.NegotiationExtras, entry *olxapi.NegotiationMessage, prefix string) string {
	price := extras.Proposal.Price.String()
	var text string
	switch msgType {
	case olxapi.MessageTypeBuyerProposed:
		text = "Price proposal from the buyer: " + price
	case olxapi.MessageTypeSellerProposed:
		text = "Counter-offer from the seller: " + price
	default:
		text = "The seller accepted the price: " + price
	}
	if entry == nil {
		return text
	}
	if state, ok := proposalStates[entry.Proposal.State]; ok && msgType != olxapi.MessageTypeSellerAccepted {
		text += " (" + state + ")"
	}
	if entry.Can(olxapi.NegotiationActionCounter) {
		text += "\nTo answer with another price: `" + prefix + " " + offerCommand + " <price>`"
	}
	return text
}

// negotiationButtons are the buttons the widget has for this user right now.
func negotiationButtons(site olxapi.Site, extras *olxapi.NegotiationExtras, entry *olxapi.NegotiationMessage, prefix string) *keyboard {
	if entry == nil {
		return nil
	}
	price := extras.Proposal.Price
	var row []button
	if entry.Can(olxapi.NegotiationActionAccept) {
		row = append(row, button{
			Text:    "Accept " + price.String(),
			Type:    "callback",
			Command: fmt.Sprintf("%s %s %s %d %s", prefix, acceptCommand, extras.NegotiationID, price.Cents, price.Currency),
		})
	}
	if entry.Can(olxapi.NegotiationActionCounter) {
		row = append(row, button{
			Text:     "Propose another price",
			Type:     "copy",
			CopyText: prefix + " " + offerCommand + " ",
		})
	}
	if entry.Can(olxapi.NegotiationActionDelivery) {
		if link := AdURL(site, string(extras.Ad.AdID)); link != "" {
			row = append(row, button{Text: "Buy with delivery", Type: "url", URL: link})
		}
	}
	if len(row) == 0 {
		return nil
	}
	return &keyboard{Keyboard: "inline", Rows: [][]button{row}}
}

func (c *OLXClient) convertNegotiation(ctx context.Context, msg *olxapi.Message, extras *olxapi.NegotiationExtras) *bridgev2.ConvertedMessage {
	prefix := c.Main.commandPrefix()
	var entry *olxapi.NegotiationMessage
	neg, err := c.API.GetNegotiation(ctx, extras.NegotiationID)
	if err != nil {
		// The proposal is still worth showing without its state and buttons.
		zerolog.Ctx(ctx).Warn().Err(c.checkErr(err)).Msg("Failed to get price negotiation state")
	} else {
		entry = neg.Message(msg.ID)
	}
	part := &bridgev2.ConvertedMessagePart{
		Type: event.EventMessage,
		Content: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    negotiationText(msg.Type, extras, entry, prefix),
		},
	}
	if buttons := negotiationButtons(c.Site, extras, entry, prefix); buttons != nil {
		part.Extra = map[string]any{buttonsField: buttons}
	}
	return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{part}}
}

// A price as people type it: digits, maybe grouped with spaces, maybe a
// fraction, maybe the currency after it. Nothing else, so that "-5" or "1e9"
// is not quietly read as some other number.
var priceInput = regexp.MustCompile(`^(\d[\d \x{00a0}]*?)(?:[.,](\d{1,2}))?\s*\p{L}*\.?$`)

// parsePrice reads a price ("1500", "1 500,50", "1500 zł") into the currency's
// minor unit.
func parsePrice(input string) (int64, error) {
	match := priceInput.FindStringSubmatch(strings.TrimSpace(input))
	if match == nil {
		return 0, fmt.Errorf("%q is not a price", strings.TrimSpace(input))
	}
	match[1] = strings.NewReplacer(" ", "", "\u00a0", "").Replace(match[1])
	whole, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || whole > 1e12 {
		return 0, fmt.Errorf("%q is not a price", strings.TrimSpace(input))
	}
	minor := match[2]
	for len(minor) < 2 {
		minor += "0"
	}
	fraction, _ := strconv.ParseInt(minor, 10, 64)
	cents := whole*100 + fraction
	if cents <= 0 {
		return 0, errors.New("the price has to be more than zero")
	}
	return cents, nil
}

// portalClient is the OLX login a command in a chat's room acts as: the one
// the chat belongs to, and only for its own user.
func portalClient(ce *commands.Event) *OLXClient {
	if ce.Portal == nil {
		ce.Reply("This command belongs in the room of an OLX chat.")
		return nil
	}
	login := ce.Bridge.GetCachedUserLoginByID(ce.Portal.Receiver)
	if login == nil || login.UserMXID != ce.User.MXID {
		ce.Reply("This is not your OLX chat.")
		return nil
	}
	client, ok := login.Client.(*OLXClient)
	if !ok || !client.IsLoggedIn() {
		ce.Reply("You're not logged in to OLX.")
		return nil
	}
	return client
}

// latestProposal finds the newest price negotiation widget of a chat and the
// negotiation service's view of it.
func (c *OLXClient) latestProposal(ctx context.Context, conversationID string) (*olxapi.NegotiationExtras, *olxapi.NegotiationMessage, error) {
	conv, err := c.API.GetConversation(ctx, conversationID)
	if err != nil {
		return nil, nil, c.checkErr(err)
	} else if conv == nil {
		return nil, nil, errors.New("the chat no longer exists on OLX")
	}
	msgs := append([]*olxapi.Message(nil), conv.Messages...)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.After(msgs[j].CreatedAt.Time) })
	for _, msg := range msgs {
		extras, ok := olxapi.ParseNegotiationExtras(msg)
		if !ok {
			continue
		}
		neg, err := c.API.GetNegotiation(ctx, extras.NegotiationID)
		if err != nil {
			return nil, nil, c.checkErr(err)
		}
		return extras, neg.Message(msg.ID), nil
	}
	return nil, nil, nil
}

// negotiationReply words a refusal by the negotiation service.
func negotiationReply(err error) string {
	var negErr *olxapi.NegotiationError
	if errors.As(err, &negErr) {
		return "OLX refused: " + negErr.Description
	}
	return fmt.Sprintf("OLX refused: %v", err)
}

var cmdOffer = &commands.FullHandler{
	Func: fnOffer,
	Name: offerCommand,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Propose a price for the ad of this chat, or answer the other side's proposal with another price.",
		Args:        "<_price_>",
	},
	RequiresLogin:  true,
	RequiresPortal: true,
}

func fnOffer(ce *commands.Event) {
	client := portalClient(ce)
	if client == nil {
		return
	}
	if len(ce.Args) == 0 {
		ce.Reply("Usage: `$cmdprefix %s <price>`", offerCommand)
		return
	}
	cents, err := parsePrice(ce.RawArgs)
	if err != nil {
		ce.Reply("Can't read the price: %v", err)
		return
	}
	adID := ce.Portal.Metadata.(*PortalMetadata).AdID
	if adID == "" {
		ce.Reply("This chat is not about an ad.")
		return
	}
	cfg, err := client.API.GetNegotiationConfig(ce.Ctx, adID)
	if err != nil {
		ce.Reply("%s", negotiationReply(client.checkErr(err)))
		return
	}
	lowest, highest := cfg.Constraints.Price.MinPrice, cfg.Constraints.Price.MaxPrice
	var currency string
	switch {
	case lowest != nil:
		currency = lowest.Currency
	case highest != nil:
		currency = highest.Currency
	default:
		ce.Reply("OLX takes no price proposals for this ad.")
		return
	}
	price := olxapi.Money{Cents: cents, Currency: currency}
	if (lowest != nil && cents < lowest.Cents) || (highest != nil && cents > highest.Cents) {
		switch {
		case lowest != nil && highest != nil:
			ce.Reply("OLX takes proposals between %s and %s for this ad.", lowest, highest)
		case lowest != nil:
			ce.Reply("OLX takes proposals from %s up for this ad.", lowest)
		default:
			ce.Reply("OLX takes proposals up to %s for this ad.", highest)
		}
		return
	}
	extras, entry, err := client.latestProposal(ce.Ctx, string(ce.Portal.ID))
	if err != nil {
		ce.Reply("Failed to look at the chat's proposals: %v", err)
		return
	}
	switch {
	case entry.Can(olxapi.NegotiationActionCounter):
		err = client.API.CounterOffer(ce.Ctx, extras.NegotiationID, price)
	case entry != nil && entry.Proposal.State == olxapi.ProposalPending:
		ce.Reply("The proposal of %s is still waiting for an answer, and OLX lets only the other side answer it.", extras.Proposal.Price)
		return
	case !cfg.Open():
		ce.Reply("OLX takes no price proposals for this ad.")
		return
	default:
		err = client.API.ProposePrice(ce.Ctx, adID, price)
	}
	if err != nil {
		ce.Reply("%s", negotiationReply(client.checkErr(err)))
		return
	}
	ce.Reply("Proposed %s. The proposal appears in the chat.", price)
}

var cmdAcceptOffer = &commands.FullHandler{
	Func: fnAcceptOffer,
	Name: acceptCommand,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Accept the price proposal that is waiting in this chat.",
	},
	RequiresLogin:  true,
	RequiresPortal: true,
}

func fnAcceptOffer(ce *commands.Event) {
	client := portalClient(ce)
	if client == nil {
		return
	}
	var negotiationID string
	var price olxapi.Money
	switch len(ce.Args) {
	case 0:
		extras, entry, err := client.latestProposal(ce.Ctx, string(ce.Portal.ID))
		if err != nil {
			ce.Reply("Failed to look at the chat's proposals: %v", err)
			return
		} else if !entry.Can(olxapi.NegotiationActionAccept) {
			ce.Reply("There is no price proposal for you to accept in this chat.")
			return
		}
		negotiationID, price = extras.NegotiationID, extras.Proposal.Price
	case 3:
		// What an Accept button sends: the proposal it belongs to, exactly.
		cents, err := strconv.ParseInt(ce.Args[1], 10, 64)
		if err != nil || cents <= 0 {
			ce.Reply("Usage: `$cmdprefix %s` (accepts the proposal waiting in this chat)", acceptCommand)
			return
		}
		negotiationID, price = ce.Args[0], olxapi.Money{Cents: cents, Currency: strings.ToUpper(ce.Args[2])}
	default:
		ce.Reply("Usage: `$cmdprefix %s` (accepts the proposal waiting in this chat)", acceptCommand)
		return
	}
	if err := client.API.AcceptProposal(ce.Ctx, negotiationID, price); err != nil {
		ce.Reply("%s", negotiationReply(client.checkErr(err)))
		return
	}
	ce.Reply("Accepted %s.", price)
}
