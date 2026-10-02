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
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

const (
	StateErrorLoggedOut status.BridgeStateErrorCode = "olx-logged-out"
	StateErrorNoSocket  status.BridgeStateErrorCode = "olx-connection-failed"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		StateErrorLoggedOut: "OLX ended the bridge's session. Log in again.",
		StateErrorNoSocket:  "Can't reach OLX's chat server. The bridge keeps trying.",
	})
}

// convState is what the bridge remembers about a conversation between syncs:
// who it is with (events only name the conversation) and the state that maps
// to room tags and receipts.
type convState struct {
	RespondentUUID string
	RespondentName string
	Archived       bool
	Observed       bool
	LastActivity   time.Time
	// The newest read state already passed on to Matrix, so that a resync
	// doesn't repeat receipts.
	PeerReadUpTo time.Time
	SelfReadUpTo time.Time
}

type OLXClient struct {
	Main      *OLXConnector
	UserLogin *bridgev2.UserLogin
	API       *olxapi.Client
	// Site is the OLX site the account is on.
	Site olxapi.Site

	connectLock sync.Mutex
	cancel      context.CancelFunc
	loggedOut   atomic.Bool
	socketUp    atomic.Bool
	fullSynced  atomic.Bool

	syncLock sync.Mutex

	stateLock sync.Mutex
	convs     map[string]*convState
	profiles  map[string]*olxapi.User
	// blocked is who the user has blocked on OLX, as far as the bridge has
	// seen, so that the ignore list is only touched when that changes.
	blocked map[string]bool
	// polledOnline is who the last presence poll found online.
	polledOnline map[string]bool

	profileGate profileGate
}

var (
	_ bridgev2.NetworkAPI                = (*OLXClient)(nil)
	_ bridgev2.NetworkAPIWithUserID      = (*OLXClient)(nil)
	_ bridgev2.ChatListSyncingNetworkAPI = (*OLXClient)(nil)
	_ olxapi.SocketHandler               = (*OLXClient)(nil)
)

func (c *OLXClient) meta() *UserLoginMetadata {
	return c.UserLogin.Metadata.(*UserLoginMetadata)
}

func (c *OLXClient) saveTokens(tokens olxapi.Tokens) {
	meta := c.meta()
	meta.RefreshToken = tokens.RefreshToken
	meta.IDToken = tokens.IDToken
	meta.IDTokenExpiry = jsontime.U(tokens.Expiry)
	ctx, cancel := context.WithTimeout(c.Main.Bridge.BackgroundCtx, 30*time.Second)
	defer cancel()
	if err := c.UserLogin.Save(ctx); err != nil {
		c.UserLogin.Log.Err(err).Msg("Failed to save renewed OLX session")
	}
}

// learnOwnUUID remembers the account's OLX user UUID the first time a
// conversation or an own message reveals it.
func (c *OLXClient) learnOwnUUID(ctx context.Context, userUUID string) {
	meta := c.meta()
	if userUUID == "" || meta.UserUUID == userUUID {
		return
	}
	meta.UserUUID = userUUID
	if err := c.UserLogin.Save(ctx); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to save own OLX user UUID")
	}
}

func (c *OLXClient) Connect(ctx context.Context) {
	c.connectLock.Lock()
	defer c.connectLock.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	if c.meta().RefreshToken == "" {
		c.loggedOut.Store(true)
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: StateErrorLoggedOut})
		return
	}
	c.loggedOut.Store(false)
	ctx, c.cancel = context.WithCancel(ctx)
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	go c.API.RunSocket(ctx, c)
	go c.periodicSyncLoop(ctx)
	if c.Main.presence != nil {
		go c.presenceLoop(ctx)
	}
}

func (c *OLXClient) Disconnect() {
	c.connectLock.Lock()
	defer c.connectLock.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.socketUp.Store(false)
}

func (c *OLXClient) IsLoggedIn() bool {
	return c.meta().RefreshToken != "" && !c.loggedOut.Load()
}

func (c *OLXClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
	if err := c.API.Logout(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to end the OLX session")
	}
	meta := c.meta()
	meta.RefreshToken = ""
	meta.IDToken = ""
	c.loggedOut.Store(true)
}

func (c *OLXClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	own := c.meta().UserUUID
	return own != "" && string(userID) == own
}

func (c *OLXClient) GetUserID() networkid.UserID {
	return MakeUserID(c.meta().UserUUID)
}

// handleLoggedOut reacts to OLX refusing the refresh token.
func (c *OLXClient) handleLoggedOut(err error) {
	if c.loggedOut.Swap(true) {
		return
	}
	c.UserLogin.Log.Warn().Err(err).Msg("OLX session is gone")
	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      StateErrorLoggedOut,
	})
	c.Disconnect()
}

// checkErr tells whether err means the session is gone, and reacts if so.
func (c *OLXClient) checkErr(err error) error {
	if err != nil && errors.Is(err, olxapi.ErrLoggedOut) {
		go c.handleLoggedOut(err)
	}
	return err
}

func (c *OLXClient) HandleConnected(ctx context.Context, reconnect bool) {
	c.socketUp.Store(true)
	c.UserLogin.Log.Debug().Bool("reconnect", reconnect).Msg("Connected to OLX chat socket")
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	// Events are not replayed, so everything missed while the socket was down
	// has to come from the conversation lists.
	go func() {
		if err := c.syncChats(ctx, !c.fullSynced.Load()); err != nil && ctx.Err() == nil {
			c.UserLogin.Log.Err(err).Msg("Failed to sync chats after connecting")
		}
	}()
}

func (c *OLXClient) HandleDisconnected(ctx context.Context, err error, fatal bool) {
	wasUp := c.socketUp.Swap(false)
	if fatal {
		c.handleLoggedOut(err)
		return
	}
	if wasUp {
		// OLX closes the socket now and then (at the latest when the token it
		// was opened with expires); the reconnect that follows is routine.
		c.UserLogin.Log.Debug().Err(err).Msg("OLX chat socket closed, reconnecting")
		return
	}
	c.UserLogin.Log.Warn().Err(err).Msg("Failed to connect to OLX chat socket")
	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      StateErrorNoSocket,
	})
}

func (c *OLXClient) periodicSyncLoop(ctx context.Context) {
	interval := c.Main.Config.Sync.Interval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.syncChats(ctx, false); err != nil && ctx.Err() == nil {
				c.UserLogin.Log.Warn().Err(err).Msg("Periodic chat sync failed")
			}
		}
	}
}

// trackConversation records a conversation's state and returns it.
func (c *OLXClient) trackConversation(conv *olxapi.Conversation) *convState {
	c.stateLock.Lock()
	defer c.stateLock.Unlock()
	state, ok := c.convs[conv.ID]
	if !ok {
		state = &convState{}
		c.convs[conv.ID] = state
	}
	state.RespondentUUID = conv.Respondent.UUID
	state.RespondentName = conv.Respondent.Name
	state.Archived = conv.Archived
	state.Observed = conv.IsObserved
	if latest := conv.LatestMessage(); latest != nil && latest.CreatedAt.After(state.LastActivity) {
		state.LastActivity = latest.CreatedAt.Time
	}
	return state
}

// conversationState returns what is known about a conversation, asking the
// database and then OLX when the bridge has not seen it since it started.
func (c *OLXClient) conversationState(ctx context.Context, conversationID string) (*convState, error) {
	c.stateLock.Lock()
	state, ok := c.convs[conversationID]
	c.stateLock.Unlock()
	if ok {
		return state, nil
	}
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, MakePortalKey(conversationID, c.UserLogin.ID))
	if err != nil {
		return nil, err
	}
	if portal != nil && portal.OtherUserID != "" {
		meta := portal.Metadata.(*PortalMetadata)
		c.stateLock.Lock()
		defer c.stateLock.Unlock()
		if state, ok = c.convs[conversationID]; !ok {
			state = &convState{
				RespondentUUID: string(portal.OtherUserID),
				Archived:       meta.Archived,
				Observed:       meta.Observed,
			}
			c.convs[conversationID] = state
		}
		return state, nil
	}
	conv, err := c.API.GetConversation(ctx, conversationID)
	if err != nil {
		return nil, c.checkErr(err)
	} else if conv == nil {
		return nil, errors.New("conversation not found on OLX")
	}
	c.learnOwnUUID(ctx, conv.UserUUID)
	return c.trackConversation(conv), nil
}

func (c *OLXClient) touchConversation(conversationID string, at time.Time) {
	c.stateLock.Lock()
	defer c.stateLock.Unlock()
	if state, ok := c.convs[conversationID]; ok && at.After(state.LastActivity) {
		state.LastActivity = at
	}
}

func (c *OLXClient) selfSender() bridgev2.EventSender {
	return bridgev2.EventSender{
		IsFromMe:    true,
		SenderLogin: c.UserLogin.ID,
		Sender:      c.GetUserID(),
	}
}

func (c *OLXClient) senderFor(userUUID string) bridgev2.EventSender {
	if own := c.meta().UserUUID; own != "" && userUUID == own {
		return c.selfSender()
	}
	return bridgev2.EventSender{Sender: MakeUserID(userUUID)}
}
