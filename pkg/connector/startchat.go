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
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

// On OLX a chat is always about an ad, and it only comes into being with its
// first message. So "starting a chat" means: find the chat about an ad, and if
// there is none, send the first message (the message-ad command).

var _ bridgev2.IdentifierResolvingNetworkAPI = (*OLXClient)(nil)

var (
	numericAdID = regexp.MustCompile(`^\d{5,}$`)
	// Ad addresses end in "-ID<short id>.html", the short ID being the ad's
	// number in base 62.
	shortAdID = regexp.MustCompile(`ID([0-9A-Za-z]{3,12})\.html`)
)

const base62Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func decodeBase62(s string) (int64, bool) {
	var n int64
	for _, r := range s {
		idx := strings.IndexRune(base62Alphabet, r)
		if idx < 0 || n > (1<<62)/62 {
			return 0, false
		}
		n = n*62 + int64(idx)
	}
	return n, n > 0
}

func encodeBase62(n int64) string {
	if n <= 0 {
		return ""
	}
	var out []byte
	for ; n > 0; n /= 62 {
		out = append([]byte{base62Alphabet[n%62]}, out...)
	}
	return string(out)
}

// AdURL is the address of an ad on a site. OLX redirects it to the full
// address, which has the ad's title in it.
func AdURL(site olxapi.Site, adID string) string {
	n, err := strconv.ParseInt(adID, 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	return site.Origin() + "/d/oferta/x-ID" + encodeBase62(n) + ".html"
}

// ParseAdID reads an ad's number from the number itself or from the ad's address.
func ParseAdID(identifier string) (int64, error) {
	identifier = strings.TrimSpace(identifier)
	if numericAdID.MatchString(identifier) {
		return strconv.ParseInt(identifier, 10, 64)
	}
	if match := shortAdID.FindStringSubmatch(identifier); match != nil {
		if id, ok := decodeBase62(match[1]); ok {
			return id, nil
		}
	}
	return 0, errors.New("that is neither an OLX ad ID (the number shown as \"ID\" on the ad) nor an ad's address")
}

// findConversation returns the user's chat about an ad, if there is one.
func (c *OLXClient) findConversation(ctx context.Context, adID int64) (*olxapi.Conversation, error) {
	for _, archived := range []bool{false, true} {
		convs, _, err := c.API.ListConversations(ctx, olxapi.ListParams{
			AdID:     strconv.FormatInt(adID, 10),
			Archived: boolPtr(archived),
			Limit:    olxapi.MaxPageSize,
		})
		if err != nil {
			return nil, c.checkErr(err)
		}
		for _, conv := range convs {
			if conv != nil && conv.ID != "" {
				return conv, nil
			}
		}
	}
	return nil, nil
}

func (c *OLXClient) resolveConversation(ctx context.Context, conv *olxapi.Conversation) *bridgev2.ResolveIdentifierResponse {
	c.learnOwnUUID(ctx, conv.UserUUID)
	c.trackConversation(conv)
	return &bridgev2.ResolveIdentifierResponse{
		UserID:   MakeUserID(conv.Respondent.UUID),
		UserInfo: c.respondentInfo(&conv.Respondent),
		Context:  adTitle(conv),
		Chat: &bridgev2.CreateChatResponse{
			PortalKey:  c.portalKey(conv.ID),
			PortalInfo: c.wrapChatInfo(conv),
		},
	}
}

// ResolveIdentifier finds the chat about an ad, given the ad's ID or address.
func (c *OLXClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	adID, err := ParseAdID(identifier)
	if err != nil {
		return nil, err
	}
	conv, err := c.findConversation(ctx, adID)
	if err != nil {
		return nil, err
	} else if conv == nil {
		return nil, fmt.Errorf("you have no chat about ad %d yet. OLX chats start with a message: use `message-ad %d <your message>`", adID, adID)
	}
	return c.resolveConversation(ctx, conv), nil
}

var cmdMessageAd = &commands.FullHandler{
	Func: fnMessageAd,
	Name: "message-ad",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Write to the seller of an OLX ad, which starts a chat about it (or continues the one you have).",
		Args:        "<_ad ID or address_> <_message_>",
	},
	RequiresLogin: true,
}

func fnMessageAd(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `$cmdprefix message-ad <ad ID or address> <message>`")
		return
	}
	adID, err := ParseAdID(ce.Args[0])
	if err != nil {
		ce.Reply("Can't tell which ad you mean: %v", err)
		return
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ce.RawArgs), ce.Args[0]))
	if text == "" {
		ce.Reply("The message is empty.")
		return
	}
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("You're not logged in to OLX.")
		return
	}
	client, ok := login.Client.(*OLXClient)
	if !ok || !client.IsLoggedIn() {
		ce.Reply("You're not logged in to OLX.")
		return
	}
	conv, err := client.findConversation(ce.Ctx, adID)
	if err != nil {
		ce.Reply("Failed to look for an existing chat: %v", err)
		return
	}
	msg := &olxapi.OutgoingMessage{Text: text}
	if conv != nil {
		_, err = client.API.SendMessage(ce.Ctx, conv.ID, msg)
	} else {
		_, err = client.API.StartConversation(ce.Ctx, adID, msg)
	}
	if err != nil {
		ce.Reply("OLX did not accept the message: %v", client.checkErr(err))
		return
	}
	if conv == nil {
		conv, err = client.findConversation(ce.Ctx, adID)
		if err != nil || conv == nil {
			ce.Reply("The message was sent. The chat will show up with the next sync.")
			return
		}
	}
	// The own message arrives over the socket (or with the backfill of a new room).
	client.queueResync(ce.Ctx, conv)
	ce.Reply("Sent to %s about “%s”. The chat's room is being set up.", conv.Respondent.Name, adTitle(conv))
}
