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
	"net/http"
	"sort"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
	"github.com/MaximilianGaedig/mautrix-olx/pkg/presence"
)

// OLX's chat has no presence of its own. What it does have is the "online" /
// "last seen" line on every ad, which comes from the public profile API. The
// bridge asks that API about the people from the most recent chats, and on
// top of that counts anyone who writes, types or reads as online for a few
// minutes (presence.Manager.Activity), which works even when the profile API
// is out of reach.

// errProfilesUnavailable means www.olx.pl refuses the bridge (it does that to
// datacenter addresses) and is not asked again for a while.
var errProfilesUnavailable = errors.New("OLX profiles are not reachable from here")

const (
	profilesBlockedRetry = time.Hour
	minPresencePoll      = 20 * time.Second
)

// profileGate keeps the bridge from asking www.olx.pl again right after it
// was refused.
type profileGate struct {
	lock         sync.Mutex
	blockedUntil time.Time
	warned       bool
}

// fetchProfiles gets public profiles and caches them.
func (c *OLXClient) fetchProfiles(ctx context.Context, uuids []string) ([]*olxapi.User, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	gate := &c.profileGate
	gate.lock.Lock()
	blocked := time.Now().Before(gate.blockedUntil)
	gate.lock.Unlock()
	if blocked {
		return nil, errProfilesUnavailable
	}
	users, err := c.API.GetUsers(ctx, uuids)
	if olxapi.IsStatus(err, http.StatusForbidden) {
		gate.lock.Lock()
		gate.blockedUntil = time.Now().Add(profilesBlockedRetry)
		warned := gate.warned
		gate.warned = true
		gate.lock.Unlock()
		if !warned {
			c.UserLogin.Log.Warn().Err(err).
				Msg("www.olx.pl refuses the bridge's address: no profile pictures or polled presence until www_proxy points at a connection OLX accepts. Chatting is not affected.")
		}
		return nil, errProfilesUnavailable
	} else if err != nil {
		return nil, c.checkErr(err)
	}
	gate.lock.Lock()
	if gate.warned {
		gate.warned = false
		c.UserLogin.Log.Info().Msg("www.olx.pl is reachable again")
	}
	gate.lock.Unlock()
	c.stateLock.Lock()
	for _, user := range users {
		if user != nil && user.UUID != "" {
			c.profiles[user.UUID] = user
		}
	}
	c.stateLock.Unlock()
	return users, nil
}

// recentRespondents lists the people from the most recently active chats.
func (c *OLXClient) recentRespondents(limit int) []string {
	type entry struct {
		uuid string
		at   time.Time
	}
	c.stateLock.Lock()
	latest := make(map[string]time.Time, len(c.convs))
	for _, state := range c.convs {
		if state.RespondentUUID == "" {
			continue
		}
		if at, ok := latest[state.RespondentUUID]; !ok || state.LastActivity.After(at) {
			latest[state.RespondentUUID] = state.LastActivity
		}
	}
	c.stateLock.Unlock()
	entries := make([]entry, 0, len(latest))
	for uuid, at := range latest {
		entries = append(entries, entry{uuid, at})
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].at.Equal(entries[j].at) {
			return entries[i].at.After(entries[j].at)
		}
		return entries[i].uuid < entries[j].uuid
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.uuid
	}
	return out
}

func (c *OLXClient) presenceLoop(ctx context.Context) {
	interval := max(c.Main.Config.Presence.PollInterval, minPresencePoll)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// The first sync fills the list of people to ask about.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	for {
		c.pollPresence(ctx, interval)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *OLXClient) pollPresence(ctx context.Context, interval time.Duration) {
	if !c.socketUp.Load() || c.loggedOut.Load() {
		return
	}
	uuids := c.recentRespondents(c.Main.Config.Presence.MaxUsers)
	if len(uuids) == 0 {
		return
	}
	users, err := c.fetchProfiles(ctx, uuids)
	if err != nil {
		if !errors.Is(err, errProfilesUnavailable) && ctx.Err() == nil {
			c.UserLogin.Log.Debug().Err(err).Msg("Failed to poll OLX presence")
		}
		return
	}
	now := time.Now()
	for _, user := range users {
		if user == nil || user.UUID == "" {
			continue
		}
		c.Main.presence.Update(user.UUID, mapPresence(user, now, interval))
		if !user.IsOnline && !user.LastSeen.IsZero() {
			c.Main.seen.Note(user.UUID, user.LastSeen.Time)
		}
		c.updateGhostProfile(ctx, user)
	}
}

// mapPresence turns a profile into Matrix presence. An online state is good
// until two polls from now: if OLX stops being reachable, people don't stay
// online forever.
func mapPresence(user *olxapi.User, now time.Time, interval time.Duration) presence.State {
	if user.IsOnline {
		return presence.State{Presence: event.PresenceOnline, Until: now.Add(2*interval + 30*time.Second)}
	}
	return presence.State{Presence: event.PresenceOffline}
}

// updateGhostProfile refreshes the name and picture of a ghost that exists.
func (c *OLXClient) updateGhostProfile(ctx context.Context, user *olxapi.User) {
	ghost, err := c.Main.Bridge.GetExistingGhostByID(ctx, MakeUserID(user.UUID))
	if err != nil || ghost == nil {
		return
	}
	ghost.UpdateInfo(ctx, c.profileInfo(user))
}

// noteActivity counts someone as online because they just did something.
func (c *OLXClient) noteActivity(userUUID string, at time.Time) {
	if c.Main.presence == nil || userUUID == "" || userUUID == c.meta().UserUUID {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	c.Main.presence.Activity(userUUID, at)
}
