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
	"net/url"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

const maxAvatarSize = 10 << 20

// avatarID identifies a picture by its address without the query, so that a
// changed signature or size parameter doesn't count as a new picture.
func avatarID(rawURL string) networkid.AvatarID {
	if parsed, err := url.Parse(rawURL); err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return networkid.AvatarID(parsed.String())
	}
	return networkid.AvatarID(rawURL)
}

func (c *OLXClient) wrapAvatar(rawURL string) *bridgev2.Avatar {
	if rawURL == "" {
		return &bridgev2.Avatar{Remove: true}
	}
	return &bridgev2.Avatar{
		ID: avatarID(rawURL),
		Get: func(ctx context.Context) ([]byte, error) {
			data, _, err := c.API.Download(ctx, rawURL, maxAvatarSize)
			return data, err
		},
	}
}

// tagFor is the one room tag a chat's state on OLX maps to. The trash wins
// over the saved list: a trashed chat is out of the way whatever else it is.
func (c *OLXClient) tagFor(archived, observed bool) *event.RoomTag {
	switch {
	case archived && c.Main.Config.ArchiveTag != "":
		return ptr.Ptr(c.Main.Config.ArchiveTag)
	case observed && c.Main.Config.SavedTag != "":
		return ptr.Ptr(c.Main.Config.SavedTag)
	default:
		return ptr.Ptr(event.RoomTag(""))
	}
}

func adTitle(conv *olxapi.Conversation) string {
	switch {
	case conv.Context != nil && conv.Context.Title != "":
		return conv.Context.Title
	case conv.Ad != nil:
		return conv.Ad.Title
	default:
		return ""
	}
}

func adID(conv *olxapi.Conversation) string {
	switch {
	case conv.Context != nil && conv.Context.Data != nil && conv.Context.Data.AdID != "":
		return string(conv.Context.Data.AdID)
	case conv.Ad != nil && conv.Ad.ID != "":
		return string(conv.Ad.ID)
	case conv.Context != nil:
		return string(conv.Context.ID)
	default:
		return ""
	}
}

var publicationStatuses = map[string]string{
	"active":   "",
	"inactive": "inactive",
	"removed":  "removed",
	"outdated": "expired",
	"sold":     "sold",
}

// adTopic describes the ad a chat is about, for the room topic.
func adTopic(site olxapi.Site, conv *olxapi.Conversation) string {
	title := adTitle(conv)
	if title == "" {
		return ""
	}
	var parts []string
	headline := title
	if conv.Context != nil && conv.Context.Data != nil {
		data := conv.Context.Data
		if price := data.Price.String(); price != "" {
			headline += " — " + price
			if data.Price.IsNegotiable {
				headline += " (negotiable)"
			}
		}
		parts = append(parts, headline)
		var details []string
		if city := data.Extension.Location.CityName; city != "" {
			if district := data.Extension.Location.DistrictName; district != "" {
				city += ", " + district
			}
			details = append(details, city)
		}
		if data.PublicationStatus != "" {
			label, known := publicationStatuses[strings.ToLower(data.PublicationStatus)]
			if !known {
				label = strings.ToLower(data.PublicationStatus)
			}
			if label != "" {
				details = append(details, "ad "+label)
			}
		}
		if id := adID(conv); id != "" {
			details = append(details, "ID "+id)
		}
		if len(details) > 0 {
			parts = append(parts, strings.Join(details, " · "))
		}
	} else {
		parts = append(parts, headline)
		if id := adID(conv); id != "" {
			parts = append(parts, "ID "+id)
		}
	}
	if link := AdURL(site, adID(conv)); link != "" {
		parts = append(parts, link)
	}
	return strings.Join(parts, "\n")
}

func (c *OLXClient) respondentInfo(resp *olxapi.Respondent) *bridgev2.UserInfo {
	c.stateLock.Lock()
	profile := c.profiles[resp.UUID]
	c.stateLock.Unlock()
	if profile != nil {
		return c.profileInfo(profile)
	}
	numericID := string(resp.ID)
	return &bridgev2.UserInfo{
		Name: ptr.Ptr(c.Main.Config.FormatDisplayname(DisplaynameParams{Name: resp.Name})),
		ExtraUpdates: func(ctx context.Context, ghost *bridgev2.Ghost) bool {
			meta := ghost.Metadata.(*GhostMetadata)
			if numericID == "" || meta.NumericID == numericID {
				return false
			}
			meta.NumericID = numericID
			return true
		},
	}
}

func (c *OLXClient) profileInfo(profile *olxapi.User) *bridgev2.UserInfo {
	info := &bridgev2.UserInfo{
		Name: ptr.Ptr(c.Main.Config.FormatDisplayname(DisplaynameParams{Name: profile.Name, Business: profile.IsBusiness})),
	}
	if profile.UserPhoto != "" {
		info.Avatar = c.wrapAvatar(profile.UserPhoto)
	} else {
		info.Avatar = &bridgev2.Avatar{Remove: true}
	}
	numericID := string(profile.ID)
	info.ExtraUpdates = func(ctx context.Context, ghost *bridgev2.Ghost) bool {
		meta := ghost.Metadata.(*GhostMetadata)
		if numericID == "" || meta.NumericID == numericID {
			return false
		}
		meta.NumericID = numericID
		return true
	}
	return info
}

// wrapChatInfo turns a conversation into a room: a direct chat with the
// respondent, named after them and the ad, with the ad's photo as its avatar.
func (c *OLXClient) wrapChatInfo(conv *olxapi.Conversation) *bridgev2.ChatInfo {
	price := ""
	if conv.Context != nil && conv.Context.Data != nil {
		price = conv.Context.Data.Price.String()
	}
	name := c.Main.Config.FormatRoomName(RoomNameParams{
		Name:  conv.Respondent.Name,
		Title: adTitle(conv),
		Price: price,
	})
	members := bridgev2.ChatMemberMap{}
	members.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      MakeUserID(conv.UserUUID),
		},
		Membership: event.MembershipJoin,
	})
	members.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{Sender: MakeUserID(conv.Respondent.UUID)},
		Membership:  event.MembershipJoin,
		UserInfo:    c.respondentInfo(&conv.Respondent),
	})
	info := &bridgev2.ChatInfo{
		Name:  &name,
		Topic: ptr.Ptr(adTopic(c.Site, conv)),
		Members: &bridgev2.ChatMemberList{
			IsFull:           true,
			TotalMemberCount: 2,
			OtherUserID:      MakeUserID(conv.Respondent.UUID),
			MemberMap:        members,
		},
		Type:        ptr.Ptr(database.RoomTypeDM),
		CanBackfill: true,
		UserLocal: &bridgev2.UserLocalPortalInfo{
			Tag: c.tagFor(conv.Archived, conv.IsObserved),
		},
	}
	if conv.Context != nil {
		info.Avatar = c.wrapAvatar(conv.Context.ImageURL)
	}
	ad, archived, observed, readOnly := adID(conv), conv.Archived, conv.IsObserved, conv.ReadOnly
	info.ExtraUpdates = func(ctx context.Context, portal *bridgev2.Portal) bool {
		meta := portal.Metadata.(*PortalMetadata)
		if meta.AdID == ad && meta.Archived == archived && meta.Observed == observed && meta.ReadOnly == readOnly {
			return false
		}
		meta.AdID, meta.Archived, meta.Observed, meta.ReadOnly = ad, archived, observed, readOnly
		return true
	}
	return info
}

func (c *OLXClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	conv, err := c.API.GetConversation(ctx, string(portal.ID))
	if err != nil {
		return nil, c.checkErr(err)
	} else if conv == nil {
		return nil, fmt.Errorf("conversation %s no longer exists on OLX", portal.ID)
	}
	c.learnOwnUUID(ctx, conv.UserUUID)
	c.trackConversation(conv)
	return c.wrapChatInfo(conv), nil
}

func (c *OLXClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	userUUID := string(ghost.ID)
	c.stateLock.Lock()
	profile := c.profiles[userUUID]
	var name string
	for _, state := range c.convs {
		if state.RespondentUUID == userUUID && state.RespondentName != "" {
			name = state.RespondentName
			break
		}
	}
	c.stateLock.Unlock()
	if profile == nil {
		users, err := c.fetchProfiles(ctx, []string{userUUID})
		if err == nil && len(users) > 0 {
			profile = users[0]
		} else if err != nil && !errors.Is(err, errProfilesUnavailable) {
			return nil, err
		}
	}
	if profile != nil {
		return c.profileInfo(profile), nil
	}
	if name == "" {
		// Nothing known: leave the ghost as it is.
		return nil, nil
	}
	return &bridgev2.UserInfo{
		Name: ptr.Ptr(c.Main.Config.FormatDisplayname(DisplaynameParams{Name: name})),
	}, nil
}
