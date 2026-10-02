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

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

var generalCaps = &bridgev2.NetworkGeneralCapabilities{
	Provisioning: bridgev2.ProvisioningCapabilities{
		ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{
			CreateDM: true,
		},
	},
}

func (oc *OLXConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return generalCaps
}

const maxUploadSize = 25 << 20

// OLX's chat is plain: text, photos and office documents, typing and read
// state. It has no replies, edits, reactions or deleting of single messages.
var roomCaps = &event.RoomFeatures{
	ID: "com.github.maximiliangaedig.olx.capabilities.2026_10_02",

	File: map[event.CapabilityMsgType]*event.FileFeatures{
		event.MsgImage: {
			MimeTypes: map[string]event.CapabilitySupportLevel{
				"image/jpeg": event.CapLevelFullySupported,
				"image/png":  event.CapLevelFullySupported,
				// Re-encoded as JPEG.
				"image/webp": event.CapLevelPartialSupport,
				"image/gif":  event.CapLevelPartialSupport,
			},
			Caption: event.CapLevelFullySupported,
			MaxSize: maxUploadSize,
		},
		event.MsgFile: {
			MimeTypes: func() map[string]event.CapabilitySupportLevel {
				out := make(map[string]event.CapabilitySupportLevel, len(documentMimes))
				for mimeType := range documentMimes {
					out[mimeType] = event.CapLevelFullySupported
				}
				return out
			}(),
			Caption: event.CapLevelFullySupported,
			MaxSize: maxUploadSize,
		},
	},

	Reply:               event.CapLevelRejected,
	Edit:                event.CapLevelRejected,
	Delete:              event.CapLevelRejected,
	Reaction:            event.CapLevelRejected,
	ReadReceipts:        true,
	TypingNotifications: true,
	DeleteChat:          true,
}

func (c *OLXClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	return roomCaps
}
