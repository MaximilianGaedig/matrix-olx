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
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
	"github.com/MaximilianGaedig/mautrix-olx/pkg/presence"
)

// Version is the bridge's version, set by main for the default User-Agent.
var Version = "dev"

type OLXConnector struct {
	Bridge *bridgev2.Bridge
	Config Config

	presence *presence.Manager
	seen     *presence.SeenReporter

	httpClient *http.Client
	wwwClient  *http.Client
}

var (
	_ bridgev2.NetworkConnector            = (*OLXConnector)(nil)
	_ bridgev2.IdentifierValidatingNetwork = (*OLXConnector)(nil)
)

func (oc *OLXConnector) Init(bridge *bridgev2.Bridge) {
	oc.Bridge = bridge
}

func newHTTPClient(proxy string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy address: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: 3 * time.Minute}, nil
}

func (oc *OLXConnector) Start(ctx context.Context) (err error) {
	oc.httpClient, err = newHTTPClient(oc.Config.Proxy)
	if err != nil {
		return err
	}
	oc.wwwClient = oc.httpClient
	if oc.Config.WWWProxy != "" && oc.Config.WWWProxy != oc.Config.Proxy {
		oc.wwwClient, err = newHTTPClient(oc.Config.WWWProxy)
		if err != nil {
			return fmt.Errorf("www_proxy: %w", err)
		}
	}
	if oc.Config.Presence.Enabled {
		oc.presence = presence.NewManager(presence.Config{}, presence.GhostSender(oc.Bridge))
		oc.seen = presence.NewSeenReporter(presence.GhostSeenSender(oc.Bridge))
		go oc.presence.Run(oc.Bridge.BackgroundCtx)
		go oc.seen.Run(oc.Bridge.BackgroundCtx)
	}
	oc.Bridge.Commands.(*commands.Processor).AddHandlers(cmdMessageAd)
	return nil
}

func (oc *OLXConnector) userAgent() string {
	if oc.Config.UserAgent != "" {
		return oc.Config.UserAgent
	}
	return "mautrix-olx/" + Version
}

func (oc *OLXConnector) apiConfig(deviceID string) olxapi.Config {
	return olxapi.Config{
		ClientVersion: oc.Config.ClientVersion,
		UserAgent:     oc.userAgent(),
		DeviceID:      deviceID,
	}
}

func (oc *OLXConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "OLX",
		NetworkURL:       "https://www.olx.pl",
		NetworkIcon:      "",
		NetworkID:        "olx",
		BeeperBridgeType: "github.com/MaximilianGaedig/mautrix-olx",
		DefaultPort:      29341,
	}
}

func (oc *OLXConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}

func (oc *OLXConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

// ValidateUserID accepts what OLX identifies people by: a UUID.
func (oc *OLXConnector) ValidateUserID(id networkid.UserID) bool {
	_, err := uuid.Parse(string(id))
	return err == nil
}

func (oc *OLXConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	if meta.DeviceID == "" {
		meta.DeviceID = uuid.NewString()
	}
	client := &OLXClient{
		Main:      oc,
		UserLogin: login,
		convs:     make(map[string]*convState),
		profiles:  make(map[string]*olxapi.User),
	}
	client.API = olxapi.NewClient(oc.apiConfig(meta.DeviceID), olxapi.Tokens{
		RefreshToken: meta.RefreshToken,
		IDToken:      meta.IDToken,
		Expiry:       meta.IDTokenExpiry.Time,
	}, oc.httpClient, oc.wwwClient, login.Log.With().Str("component", "olxapi").Logger())
	client.API.OnTokens = client.saveTokens
	login.Client = client
	return nil
}
