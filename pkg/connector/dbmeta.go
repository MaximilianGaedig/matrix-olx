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
	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type UserLoginMetadata struct {
	// Site is the OLX site the account is on ("pl", "ua", ...). Logins from
	// before the bridge knew more than one site have none and are on olx.pl.
	Site          string        `json:"site,omitempty"`
	RefreshToken  string        `json:"refresh_token"`
	IDToken       string        `json:"id_token,omitempty"`
	IDTokenExpiry jsontime.Unix `json:"id_token_expiry,omitempty"`
	Email         string        `json:"email,omitempty"`
	// DeviceID is the random ID this login identifies itself with to www.olx.pl.
	DeviceID string `json:"device_id,omitempty"`
	// UserUUID is the account's OLX user UUID, which is what messages name
	// their sender by. It is learned from the first conversation seen.
	UserUUID string `json:"user_uuid,omitempty"`
}

type PortalMetadata struct {
	AdID string `json:"ad_id,omitempty"`
	// Archived is whether the chat is in OLX's trash, Observed whether it is
	// on the saved list. Both map to room tags.
	Archived bool `json:"archived,omitempty"`
	Observed bool `json:"observed,omitempty"`
	ReadOnly bool `json:"read_only,omitempty"`
}

type GhostMetadata struct {
	// NumericID is OLX's older numeric user ID.
	NumericID string `json:"numeric_id,omitempty"`
}

// MakeUserID is the ghost ID for an OLX user UUID.
func MakeUserID(userUUID string) networkid.UserID {
	return networkid.UserID(userUUID)
}

// MakePortalKey is the portal for a conversation. Conversations belong to
// one account, so the login is always the receiver.
func MakePortalKey(conversationID string, loginID networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(conversationID), Receiver: loginID}
}

// MakeUserLoginID is the login ID for a session: the account's subject in
// OLX's identity provider, which is stable and known right after login.
func MakeUserLoginID(subject string) networkid.UserLoginID {
	return networkid.UserLoginID(subject)
}
