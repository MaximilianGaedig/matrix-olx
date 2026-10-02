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

package olxapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FlexID is an identifier that OLX sends as a number in some places and as a
// string in others (ad IDs above all).
type FlexID string

func (f *FlexID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*f = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = FlexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = FlexID(n.String())
	return nil
}

func (f FlexID) Int() int64 {
	n, _ := strconv.ParseInt(string(f), 10, 64)
	return n
}

// Time is a timestamp in any of the layouts OLX uses across its APIs.
type Time struct{ time.Time }

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
}

func (t *Time) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || bytes.Equal(data, []byte(`""`)) {
		t.Time = time.Time{}
		return nil
	}
	if len(data) > 0 && data[0] != '"' {
		// A unix timestamp, in seconds or milliseconds.
		n, err := strconv.ParseFloat(string(data), 64)
		if err != nil {
			return fmt.Errorf("unsupported timestamp %s", data)
		}
		if n > 1e11 {
			t.Time = time.UnixMilli(int64(n))
		} else {
			t.Time = time.Unix(int64(n), 0)
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("unsupported timestamp %q", s)
}

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time.Format(time.RFC3339Nano))
}

// Conversation is one chat: the logged-in user and one other person, about one ad.
type Conversation struct {
	ID          string     `json:"id"`
	UserID      FlexID     `json:"user_id"`
	UserUUID    string     `json:"user_uuid"`
	ReadOnly    bool       `json:"read_only"`
	Respondent  Respondent `json:"respondent"`
	Archived    bool       `json:"archived"`
	Ad          *Ad        `json:"ad"`
	Context     *AdContext `json:"context"`
	IsObserved  bool       `json:"is_observed"`
	Messages    []*Message `json:"messages"`
	UnreadCount int        `json:"unread_count"`
}

// LatestMessage is the newest message the conversation carries, if any.
func (c *Conversation) LatestMessage() *Message {
	var latest *Message
	for _, msg := range c.Messages {
		if latest == nil || msg.CreatedAt.After(latest.CreatedAt.Time) {
			latest = msg
		}
	}
	return latest
}

// Respondent is the other person in a conversation.
type Respondent struct {
	ID      FlexID `json:"id"`
	UUID    string `json:"uuid"`
	Blocked bool   `json:"blocked"`
	Name    string `json:"name"`
	// Type is the respondent's role in the conversation: "seller" or "buyer".
	Type string `json:"type"`
}

type Ad struct {
	ID    FlexID `json:"id"`
	Title string `json:"title"`
}

// AdContext is the ad a conversation is about.
type AdContext struct {
	ID       FlexID  `json:"id"`
	Title    string  `json:"title"`
	ImageURL string  `json:"image_url"`
	Data     *AdData `json:"context_data"`
}

type AdData struct {
	AdID              FlexID `json:"ad_id"`
	Title             string `json:"title"`
	Description       string `json:"description"`
	Price             *Price `json:"price"`
	PublicationStatus string `json:"publication_status"`
	Extension         struct {
		Location struct {
			CityName     string `json:"city_name"`
			DistrictName string `json:"district_name"`
		} `json:"location"`
	} `json:"extension"`
}

type Price struct {
	MinorAmount  *int64 `json:"minor_amount"`
	CurrencyCode string `json:"currency_code"`
	IsNegotiable bool   `json:"is_negotiable"`
	OnRequest    bool   `json:"on_request"`
}

// String formats the price the way OLX shows it ("1 900 zł").
func (p *Price) String() string {
	if p == nil || p.MinorAmount == nil || p.OnRequest {
		return ""
	}
	whole, minor := *p.MinorAmount/100, *p.MinorAmount%100
	digits := strconv.FormatInt(whole, 10)
	var grouped strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(' ')
		}
		grouped.WriteRune(r)
	}
	if minor != 0 {
		fmt.Fprintf(&grouped, ",%02d", minor)
	}
	currency := p.CurrencyCode
	if currency == "PLN" {
		currency = "zł"
	}
	return strings.TrimSpace(grouped.String() + " " + currency)
}

const (
	MessageTypeStandard = "standard"
	MessageTypeSystem   = "system"
)

type Message struct {
	ID             string          `json:"id"`
	UserID         FlexID          `json:"user_id"`
	UserUUID       string          `json:"user_uuid"`
	CreatedAt      Time            `json:"created_at"`
	ReadAt         *Time           `json:"read_at"`
	Text           string          `json:"text"`
	Attachments    []Attachment    `json:"attachments"`
	SentFromMobile bool            `json:"sent_from_mobile"`
	Type           string          `json:"type"`
	Extras         json.RawMessage `json:"extras"`
}

// IsRead reports whether the recipient has read the message.
func (m *Message) IsRead() bool {
	return m.ReadAt != nil && !m.ReadAt.IsZero()
}

// Attachment is a file attached to a message. OLX tells images from documents
// by the file name's extension only.
type Attachment struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (a *Attachment) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name     string `json:"name"`
		Filename string `json:"filename"`
		URL      string `json:"url"`
		Href     string `json:"href"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Name = raw.Name
	if a.Name == "" {
		a.Name = raw.Filename
	}
	a.URL = raw.URL
	if a.URL == "" {
		a.URL = raw.Href
	}
	return nil
}

type link struct {
	Href string `json:"href"`
}

type conversationList struct {
	Data     []*Conversation `json:"data"`
	Metadata struct {
		TotalElements int `json:"total_elements"`
	} `json:"metadata"`
	Links struct {
		Next *link `json:"next"`
	} `json:"links"`
}

// ListParams filters a conversation list request. Nil means "not filtered".
type ListParams struct {
	// Archived selects the trash (true) or the active conversations (false).
	Archived *bool
	// MyAds selects conversations about the user's own ads (selling) or about
	// other people's ads (buying).
	MyAds    *bool
	Observed *bool
	Unread   *bool
	AdID     string
	Limit    int
	Offset   int
}

// Counters are the unread counts shown on the chat tabs.
type Counters struct {
	Buying struct {
		Read   int `json:"read"`
		Unread int `json:"unread"`
	} `json:"buying"`
	Selling struct {
		Read   int `json:"read"`
		Unread int `json:"unread"`
	} `json:"selling"`
}

// OutgoingAttachment is an uploaded file ready to be attached to a message.
type OutgoingAttachment struct {
	Filename string `json:"filename"`
	FileID   string `json:"file_id"`
}

type OutgoingMessage struct {
	Text        string               `json:"text,omitempty"`
	MessageID   string               `json:"message_id"`
	Attachments []OutgoingAttachment `json:"attachments,omitempty"`
}

// User is a public OLX profile, with the online status OLX shows on ads.
type User struct {
	ID         FlexID `json:"id"`
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	Created    Time   `json:"created"`
	LastLogin  Time   `json:"last_login"`
	LastSeen   Time   `json:"last_seen"`
	IsOnline   bool   `json:"is_online"`
	IsBusiness bool   `json:"is_business"`
	ShowPhoto  bool   `json:"show_photo"`
	UserPhoto  string `json:"user_photo"`
	UserAdsURL string `json:"user_ads_url"`
}

// Event is one frame from the chat WebSocket.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

const (
	EventNewMessage             = "new_message"
	EventMessageSent            = "message_sent"
	EventTypingStarted          = "typing_started"
	EventTypingStopped          = "typing_stopped"
	EventConversationSaved      = "conversation_saved"
	EventConversationUnsaved    = "conversation_unsaved"
	EventConversationTrashed    = "conversation_trashed"
	EventConversationUntrashed  = "conversation_untrashed"
	EventConversationRead       = "conversation_read"
	EventConversationMarkedRead = "conversation_marked_read"
	EventUserBlock              = "user_block"
	EventUserUnblock            = "user_unblock"
)

// EventData holds the fields of every known event type; which ones are set
// depends on the type.
type EventData struct {
	ConversationID  string   `json:"conversation_id"`
	Message         *Message `json:"message"`
	UnreadCounter   *int     `json:"unread_counter"`
	UnreadCount     *int     `json:"unread_count"`
	Timestamp       Time     `json:"timestamp"`
	BlockedUserUUID string   `json:"blocked_user_uuid"`
}
