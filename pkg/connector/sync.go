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
	"sort"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

// quickSyncPageSize is how many chats per list a routine resync looks at: the
// lists are newest first, so anything that changed since the last look is at
// the top.
const quickSyncPageSize = 20

// chatLists are the lists OLX's chat is made of. The website always asks for
// one of them, never for everything at once, and so does the bridge.
func (c *OLXClient) chatLists() []olxapi.ListParams {
	lists := []olxapi.ListParams{
		{Archived: boolPtr(false), MyAds: boolPtr(false)},
		{Archived: boolPtr(false), MyAds: boolPtr(true)},
	}
	if c.Main.Config.Sync.Archived {
		lists = append(lists,
			olxapi.ListParams{Archived: boolPtr(true), MyAds: boolPtr(false)},
			olxapi.ListParams{Archived: boolPtr(true), MyAds: boolPtr(true)},
		)
	}
	return lists
}

// syncChats queues a resync for the user's chats: all of them when full is
// set, otherwise the most recent ones. The resync creates missing rooms and
// backfills messages newer than the last bridged one.
func (c *OLXClient) syncChats(ctx context.Context, full bool) error {
	c.syncLock.Lock()
	defer c.syncLock.Unlock()
	log := c.UserLogin.Log.With().Str("action", "sync chats").Bool("full", full).Logger()
	ctx = log.WithContext(ctx)
	seen := make(map[string]struct{})
	var convs []*olxapi.Conversation
	var firstErr error
	for _, params := range c.chatLists() {
		var page []*olxapi.Conversation
		var err error
		if full {
			page, err = c.API.ListAllConversations(ctx, params, 0)
		} else {
			params.Limit = quickSyncPageSize
			page, _, err = c.API.ListConversations(ctx, params)
		}
		if err != nil {
			if c.checkErr(err); errors.Is(err, olxapi.ErrLoggedOut) || ctx.Err() != nil {
				return err
			}
			log.Err(err).Msg("Failed to list chats")
			if firstErr == nil {
				firstErr = err
			}
		}
		for _, conv := range page {
			if conv == nil || conv.ID == "" {
				continue
			} else if _, dup := seen[conv.ID]; dup {
				continue
			}
			seen[conv.ID] = struct{}{}
			convs = append(convs, conv)
		}
	}
	// Oldest first, so that rooms are created in the order the chats happened.
	sort.SliceStable(convs, func(i, j int) bool {
		a, b := convs[i].LatestMessage(), convs[j].LatestMessage()
		if a == nil || b == nil {
			return a == nil && b != nil
		}
		return a.CreatedAt.Before(b.CreatedAt.Time)
	})
	for _, conv := range convs {
		c.queueResync(ctx, conv)
	}
	if full && firstErr == nil {
		c.fullSynced.Store(true)
	}
	log.Info().Int("chats", len(convs)).Msg("Queued chat resync")
	return firstErr
}

// queueResync queues one conversation for a resync and passes on the read
// state its newest message shows.
func (c *OLXClient) queueResync(ctx context.Context, conv *olxapi.Conversation) {
	c.learnOwnUUID(ctx, conv.UserUUID)
	state := c.trackConversation(conv)
	latest := conv.LatestMessage()
	evt := &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventChatResync,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("olx_conversation_id", conv.ID)
			},
			PortalKey:    c.portalKey(conv.ID),
			CreatePortal: true,
		},
		ChatInfo: c.wrapChatInfo(conv),
	}
	if latest != nil {
		evt.LatestMessageTS = latest.CreatedAt.Time
	}
	c.UserLogin.QueueRemoteEvent(evt)
	c.syncBlock(ctx, conv)
	if latest == nil {
		return
	}
	if latest.UserUUID == conv.UserUUID {
		if latest.IsRead() {
			c.queuePeerRead(conv.ID, state, latest.ReadAt.Time)
		}
	} else if conv.UnreadCount == 0 {
		c.queueOwnRead(conv.ID, state, latest.CreatedAt.Time)
	}
}

// SyncChatList goes over every chat again, creating rooms for the ones that
// have none.
func (c *OLXClient) SyncChatList(ctx context.Context) error {
	if !c.IsLoggedIn() {
		return errors.New("not logged in to OLX")
	}
	return c.syncChats(ctx, true)
}

var _ bridgev2.BackfillingNetworkAPI = (*OLXClient)(nil)

// FetchMessages returns a chat's messages. OLX hands out a conversation whole,
// so one request covers both directions and there is never more to page
// through.
func (c *OLXClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	conv, err := c.API.GetConversation(ctx, string(params.Portal.ID))
	if err != nil {
		return nil, c.checkErr(err)
	} else if conv == nil {
		return nil, fmt.Errorf("conversation %s no longer exists on OLX", params.Portal.ID)
	}
	c.learnOwnUUID(ctx, conv.UserUUID)
	c.trackConversation(conv)
	msgs := make([]*olxapi.Message, 0, len(conv.Messages))
	for _, msg := range conv.Messages {
		if msg == nil || msg.ID == "" {
			continue
		}
		if anchor := params.AnchorMessage; anchor != nil {
			if networkid.MessageID(msg.ID) == anchor.ID {
				continue
			} else if params.Forward && !msg.CreatedAt.After(anchor.Timestamp) {
				continue
			} else if !params.Forward && !msg.CreatedAt.Before(anchor.Timestamp) {
				continue
			}
		}
		msgs = append(msgs, msg)
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt.Time)
	})
	resp := &bridgev2.FetchMessagesResponse{
		Forward:  params.Forward,
		HasMore:  false,
		MarkRead: conv.UnreadCount == 0,
	}
	intent := c.Main.Bridge.Bot
	for _, msg := range msgs {
		sender := bridgev2.EventSender{Sender: MakeUserID(msg.UserUUID)}
		if msg.UserUUID == conv.UserUUID {
			sender = c.selfSender()
		}
		converted, err := c.convertMessage(ctx, params.Portal, intent, msg)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Str("olx_message_id", msg.ID).Msg("Failed to convert message for backfill")
			continue
		}
		resp.Messages = append(resp.Messages, &bridgev2.BackfillMessage{
			ConvertedMessage: converted,
			Sender:           sender,
			ID:               networkid.MessageID(msg.ID),
			Timestamp:        msg.CreatedAt.Time,
		})
	}
	return resp, nil
}
