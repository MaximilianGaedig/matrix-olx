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

package olxapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Price negotiation ("Zaproponuj cenę"): a buyer proposes a price for an ad,
// the seller accepts it or answers with another. Every step shows up in the
// chat as a message of one of the widget types below; the state of a proposal
// and what can be done about it come from the negotiation service.

const (
	MessageTypeBuyerProposed  = "custom:price_negotiation_widget_buyer_proposed"
	MessageTypeSellerProposed = "custom:price_negotiation_widget_seller_proposed"
	MessageTypeSellerAccepted = "custom:price_negotiation_widget_seller_accepted"
)

// IsNegotiationType reports whether a message type is a price negotiation widget.
func IsNegotiationType(messageType string) bool {
	switch messageType {
	case MessageTypeBuyerProposed, MessageTypeSellerProposed, MessageTypeSellerAccepted:
		return true
	default:
		return false
	}
}

// Money is an amount in the currency's minor unit.
type Money struct {
	Cents    int64  `json:"cents"`
	Currency string `json:"currency"`
}

// String formats the amount the way OLX shows prices.
func (m Money) String() string {
	return (&Price{MinorAmount: &m.Cents, CurrencyCode: m.Currency}).String()
}

// NegotiationExtras is what a price negotiation widget message carries.
type NegotiationExtras struct {
	NegotiationID string `json:"negotiationId"`
	Proposal      struct {
		Price Money `json:"price"`
	} `json:"proposal"`
	Ad struct {
		AdID FlexID `json:"adId"`
	} `json:"ad"`
}

// ParseNegotiationExtras reads the extras of a price negotiation widget.
func ParseNegotiationExtras(msg *Message) (*NegotiationExtras, bool) {
	if msg == nil || !IsNegotiationType(msg.Type) || len(msg.Extras) == 0 {
		return nil, false
	}
	var extras NegotiationExtras
	if err := json.Unmarshal(msg.Extras, &extras); err != nil || extras.NegotiationID == "" {
		return nil, false
	}
	return &extras, true
}

const (
	// Proposal states.
	ProposalPending  = "PENDING"
	ProposalAccepted = "ACCEPTED"
	ProposalReplaced = "REPLACED"

	// What can be done about a proposal.
	NegotiationActionAccept   = "ACCEPT"
	NegotiationActionCounter  = "PROPOSE_NEW_PRICE"
	NegotiationActionDelivery = "BUY_WITH_DELIVERY"
)

// NegotiationMessage is the negotiation service's view of one widget message.
type NegotiationMessage struct {
	MessageID string `json:"messageId"`
	Proposal  struct {
		State string `json:"state"`
		Price Money  `json:"price"`
	} `json:"proposal"`
	Actions []struct {
		Type string `json:"type"`
	} `json:"actions"`
}

// Can reports whether the user may take the given action on the proposal.
func (m *NegotiationMessage) Can(action string) bool {
	if m == nil {
		return false
	}
	for _, a := range m.Actions {
		if a.Type == action {
			return true
		}
	}
	return false
}

// Negotiation is the state of one negotiation.
type Negotiation struct {
	Messages []*NegotiationMessage `json:"messages"`
	// YouAre is the user's role in it.
	YouAre string `json:"youAre"`
}

// Message returns the entry for a chat message, or nil.
func (n *Negotiation) Message(messageID string) *NegotiationMessage {
	if n == nil {
		return nil
	}
	for _, msg := range n.Messages {
		if msg != nil && msg.MessageID == messageID {
			return msg
		}
	}
	return nil
}

// NegotiationConfig says whether an ad takes price proposals and in what range.
type NegotiationConfig struct {
	Enabled                  bool `json:"enabled"`
	EnabledWithPayAndShip    bool `json:"enabledWithPayAndShip"`
	EnabledWithoutPayAndShip bool `json:"enabledWithoutPayAndShip"`
	Constraints              struct {
		Price struct {
			MinPrice *Money `json:"minPrice"`
			MaxPrice *Money `json:"maxPrice"`
		} `json:"price"`
	} `json:"constraints"`
}

// Open reports whether proposals can be made for the ad at all.
func (c *NegotiationConfig) Open() bool {
	return c != nil && (c.Enabled || c.EnabledWithPayAndShip || c.EnabledWithoutPayAndShip)
}

// NegotiationError is the negotiation service refusing something, with the
// reason it gives.
type NegotiationError struct {
	Description string
	Err         error
}

func (e *NegotiationError) Error() string { return e.Description }
func (e *NegotiationError) Unwrap() error { return e.Err }

// negotiationErr turns the service's error body into its own words.
func negotiationErr(err error) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	var body struct {
		Errors []struct {
			Description string `json:"description"`
			Title       string `json:"title"`
		} `json:"errors"`
	}
	if json.Unmarshal([]byte(strings.TrimSuffix(httpErr.Body, "…")), &body) == nil && len(body.Errors) > 0 {
		desc := strings.TrimSpace(body.Errors[0].Description)
		if desc == "" {
			desc = strings.TrimSpace(body.Errors[0].Title)
		}
		if desc != "" {
			return &NegotiationError{Description: desc, Err: err}
		}
	}
	return err
}

func (c *Client) negotiation(ctx context.Context, method, path string, body, out any) error {
	return negotiationErr(c.do(ctx, request{
		method:   method,
		url:      c.cfg.NegotiationURL + "/price-negotiations/v1/" + path,
		body:     body,
		external: true,
	}, out))
}

type priceBody struct {
	Price Money `json:"price"`
}

// GetNegotiationConfig returns an ad's price proposal settings.
func (c *Client) GetNegotiationConfig(ctx context.Context, adID string) (*NegotiationConfig, error) {
	var cfg NegotiationConfig
	err := c.negotiation(ctx, http.MethodGet, "ad/"+url.PathEscape(adID)+"/config", nil, &cfg)
	return &cfg, err
}

// GetNegotiation returns the state of a negotiation.
func (c *Client) GetNegotiation(ctx context.Context, negotiationID string) (*Negotiation, error) {
	var neg Negotiation
	err := c.negotiation(ctx, http.MethodGet, "negotiation/"+url.PathEscape(negotiationID)+"/messages", nil, &neg)
	return &neg, err
}

// ProposePrice starts a negotiation: the buyer's first proposal for an ad.
func (c *Client) ProposePrice(ctx context.Context, adID string, price Money) error {
	return c.negotiation(ctx, http.MethodPost, "ad/"+url.PathEscape(adID), &priceBody{price}, nil)
}

// CounterOffer answers the other side's proposal with another price.
func (c *Client) CounterOffer(ctx context.Context, negotiationID string, price Money) error {
	return c.negotiation(ctx, http.MethodPost, "negotiation/"+url.PathEscape(negotiationID), &priceBody{price}, nil)
}

// AcceptProposal accepts the proposal that is pending, which has to be named
// by its price.
func (c *Client) AcceptProposal(ctx context.Context, negotiationID string, price Money) error {
	return c.negotiation(ctx, http.MethodPost, "negotiation/"+url.PathEscape(negotiationID)+"/accept", &priceBody{price}, nil)
}

// ReportReason is one of the reasons OLX offers for reporting a chat.
type ReportReason struct {
	Key              string `json:"key"`
	Label            string `json:"label"`
	Description      string `json:"description"`
	NeedsDescription bool   `json:"needs_description"`
}

// reportLanguage is the language the reasons are asked for in: the site's
// locale with an underscore ("pl_PL").
func (c *Client) reportLanguage() string {
	locale, _, _ := strings.Cut(c.cfg.Language, ",")
	return strings.ReplaceAll(strings.TrimSpace(locale), "-", "_")
}

// GetReportReasons lists the reasons a chat can be reported for.
func (c *Client) GetReportReasons(ctx context.Context) ([]*ReportReason, error) {
	var reasons []*ReportReason
	err := c.do(ctx, request{
		method:   http.MethodGet,
		url:      c.cfg.ModerationURL + "/moderation/chat/abuse/reasons",
		query:    url.Values{"lang": {c.reportLanguage()}, "siteCode": {c.cfg.SiteCode}},
		external: true,
	}, &reasons)
	return reasons, err
}

// Report is a report of the other person in a chat to OLX's moderation.
type Report struct {
	AdID        json.Number `json:"adId"`
	BuyerID     json.Number `json:"buyerId"`
	SellerID    json.Number `json:"sellerId"`
	ReasonType  string      `json:"reasonType"`
	Description string      `json:"description,omitempty"`
	SiteCode    string      `json:"siteCode"`
}

// ReportChat sends a report. This reaches OLX's moderators and is about a
// real person: it is never to be called to try something out.
func (c *Client) ReportChat(ctx context.Context, report *Report) error {
	report.SiteCode = c.cfg.SiteCode
	return c.do(ctx, request{
		method:   http.MethodPost,
		url:      c.cfg.ModerationURL + "/moderation/chat/abuse/report",
		body:     report,
		headers:  map[string]string{"X-Site-Code": c.cfg.SiteCode},
		external: true,
	}, nil)
}
