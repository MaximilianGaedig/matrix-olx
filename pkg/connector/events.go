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
	"encoding/json"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

// typingTimeout is how long a typing notification lasts without a stop
// event. OLX sends an explicit stop, so this only covers a lost one.
const typingTimeout = 15 * time.Second

func (c *OLXClient) HandleEvent(ctx context.Context, evt *olxapi.Event) {
	log := c.UserLogin.Log.With().Str("olx_event", evt.Type).Logger()
	ctx = log.WithContext(ctx)
	var data olxapi.EventData
	if len(evt.Data) > 0 {
		if err := json.Unmarshal(evt.Data, &data); err != nil {
			log.Warn().Err(err).Msg("Failed to parse OLX event")
			return
		}
	}
	log.Trace().Str("conversation_id", data.ConversationID).Msg("Received OLX event")
	switch evt.Type {
	case olxapi.EventNewMessage:
		c.handleMessage(ctx, &data, false)
	case olxapi.EventMessageSent:
		c.handleMessage(ctx, &data, true)
	case olxapi.EventTypingStarted:
		c.handleTyping(ctx, &data, true)
	case olxapi.EventTypingStopped:
		c.handleTyping(ctx, &data, false)
	case olxapi.EventConversationRead:
		c.handlePeerRead(ctx, &data)
	case olxapi.EventConversationMarkedRead:
		c.handleOwnRead(ctx, &data)
	case olxapi.EventConversationSaved:
		c.handleFlag(ctx, &data, nil, boolPtr(true))
	case olxapi.EventConversationUnsaved:
		c.handleFlag(ctx, &data, nil, boolPtr(false))
	case olxapi.EventConversationTrashed:
		c.handleFlag(ctx, &data, boolPtr(true), nil)
	case olxapi.EventConversationUntrashed:
		c.handleFlag(ctx, &data, boolPtr(false), nil)
	case olxapi.EventUserBlock:
		c.handleBlock(ctx, &data, true)
	case olxapi.EventUserUnblock:
		c.handleBlock(ctx, &data, false)
	default:
		log.Debug().RawJSON("data", nonEmptyJSON(evt.Data)).Msg("Unhandled OLX event")
	}
}

func boolPtr(v bool) *bool { return &v }

func nonEmptyJSON(data json.RawMessage) json.RawMessage {
	if len(data) == 0 || !json.Valid(data) {
		return json.RawMessage("null")
	}
	return data
}

func (c *OLXClient) portalKey(conversationID string) networkid.PortalKey {
	return MakePortalKey(conversationID, c.UserLogin.ID)
}

// messageEvent wraps a message for the bridge. An own message is matched to
// what Matrix sent by its ID, which the sender picks.
func (c *OLXClient) messageEvent(conversationID string, msg *olxapi.Message, sender bridgev2.EventSender) *simplevent.Message[*olxapi.Message] {
	evt := &simplevent.Message[*olxapi.Message]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("olx_message_id", msg.ID)
			},
			PortalKey:    c.portalKey(conversationID),
			Sender:       sender,
			CreatePortal: true,
			Timestamp:    msg.CreatedAt.Time,
		},
		Data:               msg,
		ID:                 networkid.MessageID(msg.ID),
		ConvertMessageFunc: c.convertMessage,
	}
	if sender.IsFromMe {
		evt.TransactionID = networkid.TransactionID(msg.ID)
	}
	return evt
}

func (c *OLXClient) handleMessage(ctx context.Context, data *olxapi.EventData, own bool) {
	msg := data.Message
	if msg == nil || msg.ID == "" || data.ConversationID == "" {
		zerolog.Ctx(ctx).Warn().Msg("OLX message event without a message")
		return
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt.Time = time.Now()
	}
	var sender bridgev2.EventSender
	if own {
		c.learnOwnUUID(ctx, msg.UserUUID)
		sender = c.selfSender()
	} else {
		sender = c.senderFor(msg.UserUUID)
		c.noteActivity(msg.UserUUID, msg.CreatedAt.Time)
		// Whoever sent a message has stopped typing it.
		c.UserLogin.QueueRemoteEvent(&simplevent.Typing{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventTyping,
				PortalKey: c.portalKey(data.ConversationID),
				Sender:    sender,
			},
			Timeout: 0,
		})
	}
	c.touchConversation(data.ConversationID, msg.CreatedAt.Time)
	c.UserLogin.QueueRemoteEvent(c.messageEvent(data.ConversationID, msg, sender))
}

func (c *OLXClient) handleTyping(ctx context.Context, data *olxapi.EventData, typing bool) {
	state, err := c.conversationState(ctx, data.ConversationID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Typing event for unknown conversation")
		return
	}
	timeout := time.Duration(0)
	if typing {
		timeout = typingTimeout
		c.noteActivity(state.RespondentUUID, time.Now())
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventTyping,
			PortalKey: c.portalKey(data.ConversationID),
			Sender:    bridgev2.EventSender{Sender: MakeUserID(state.RespondentUUID)},
		},
		Timeout: timeout,
	})
}

func eventTime(data *olxapi.EventData) time.Time {
	if data.Timestamp.IsZero() {
		return time.Now()
	}
	return data.Timestamp.Time
}

// handlePeerRead is the other person reading the user's messages.
func (c *OLXClient) handlePeerRead(ctx context.Context, data *olxapi.EventData) {
	state, err := c.conversationState(ctx, data.ConversationID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Read event for unknown conversation")
		return
	}
	at := eventTime(data)
	c.noteActivity(state.RespondentUUID, at)
	c.queuePeerRead(data.ConversationID, state, at)
}

func (c *OLXClient) queuePeerRead(conversationID string, state *convState, at time.Time) {
	c.stateLock.Lock()
	stale := !at.After(state.PeerReadUpTo)
	if !stale {
		state.PeerReadUpTo = at
	}
	respondent := state.RespondentUUID
	c.stateLock.Unlock()
	if stale || respondent == "" {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: c.portalKey(conversationID),
			Sender:    bridgev2.EventSender{Sender: MakeUserID(respondent)},
			Timestamp: at,
		},
		ReadUpTo: at,
	})
}

// handleOwnRead is the user reading a chat on another device.
func (c *OLXClient) handleOwnRead(ctx context.Context, data *olxapi.EventData) {
	state, err := c.conversationState(ctx, data.ConversationID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Read event for unknown conversation")
		return
	}
	c.queueOwnRead(data.ConversationID, state, eventTime(data))
}

func (c *OLXClient) queueOwnRead(conversationID string, state *convState, at time.Time) {
	c.stateLock.Lock()
	stale := !at.After(state.SelfReadUpTo)
	if !stale {
		state.SelfReadUpTo = at
	}
	c.stateLock.Unlock()
	if stale {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: c.portalKey(conversationID),
			Sender:    c.selfSender(),
			Timestamp: at,
		},
		ReadUpTo: at,
	})
}

// handleFlag is a chat moving in or out of the trash or the saved list, which
// the room shows as a tag.
func (c *OLXClient) handleFlag(ctx context.Context, data *olxapi.EventData, archived, observed *bool) {
	state, err := c.conversationState(ctx, data.ConversationID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Flag event for unknown conversation")
		return
	}
	c.stateLock.Lock()
	if archived != nil {
		state.Archived = *archived
	}
	if observed != nil {
		state.Observed = *observed
	}
	nowArchived, nowObserved := state.Archived, state.Observed
	c.stateLock.Unlock()
	c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: c.portalKey(data.ConversationID),
			Sender:    c.selfSender(),
			Timestamp: eventTime(data),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				UserLocal: &bridgev2.UserLocalPortalInfo{Tag: c.tagFor(nowArchived, nowObserved)},
				ExtraUpdates: func(ctx context.Context, portal *bridgev2.Portal) bool {
					meta := portal.Metadata.(*PortalMetadata)
					if meta.Archived == nowArchived && meta.Observed == nowObserved {
						return false
					}
					meta.Archived, meta.Observed = nowArchived, nowObserved
					return true
				},
			},
		},
	})
}

// handleBlock mirrors a block made on OLX into the Matrix ignore list.
func (c *OLXClient) handleBlock(ctx context.Context, data *olxapi.EventData, blocked bool) {
	if data.BlockedUserUUID == "" {
		return
	}
	if err := c.UserLogin.SetGhostBlocked(ctx, MakeUserID(data.BlockedUserUUID), blocked); err != nil {
		zerolog.Ctx(ctx).Err(err).Bool("blocked", blocked).Msg("Failed to mirror OLX block to Matrix")
	}
}
