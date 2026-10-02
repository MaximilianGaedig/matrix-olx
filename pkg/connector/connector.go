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
	"crypto/tls"
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

// Version is the bridge's version, set by main.
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

// newHTTPClient makes an HTTP client, optionally through a proxy. With http1
// set it never speaks HTTP/2: www.olx.pl turns away HTTP/2 from anything that
// is not a browser, and serves the same client over HTTP/1.1.
func newHTTPClient(proxy string, http1 bool) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	if http1 {
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	}
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
	oc.httpClient, err = newHTTPClient(oc.Config.Proxy, false)
	if err != nil {
		return err
	}
	wwwProxy := oc.Config.WWWProxy
	if wwwProxy == "" {
		wwwProxy = oc.Config.Proxy
	}
	oc.wwwClient, err = newHTTPClient(wwwProxy, true)
	if err != nil {
		return fmt.Errorf("www_proxy: %w", err)
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

// userAgent is the User-Agent sent to OLX: by default the one of the browser
// its web app runs in.
func (oc *OLXConnector) userAgent() string {
	if oc.Config.UserAgent != "" {
		return oc.Config.UserAgent
	}
	return olxapi.ChromeUserAgent(olxapi.DefaultChromeMajor)
}

func (oc *OLXConnector) apiConfig(deviceID string, site olxapi.Site) olxapi.Config {
	return olxapi.Config{
		Site:          site,
		ClientVersion: oc.Config.ClientVersion,
		UserAgent:     oc.userAgent(),
		DeviceID:      deviceID,
	}
}

func (oc *OLXConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "OLX",
		NetworkURL:       "https://www.olx.com",
		NetworkIcon:      "mxc://maximiliangaedig.com/d12qMg90lGPcR6xOWPJ5UhS0E8g4PHzN",
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
	return 2, 1
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
	site, err := olxapi.LookupSite(meta.Site)
	if err != nil {
		return fmt.Errorf("login %s: %w", login.ID, err)
	}
	client := &OLXClient{
		Main:      oc,
		UserLogin: login,
		Site:      site,
		convs:     make(map[string]*convState),
		profiles:  make(map[string]*olxapi.User),
	}
	client.API = olxapi.NewClient(oc.apiConfig(meta.DeviceID, site), olxapi.Tokens{
		RefreshToken: meta.RefreshToken,
		IDToken:      meta.IDToken,
		Expiry:       meta.IDTokenExpiry.Time,
	}, oc.httpClient, oc.wwwClient, login.Log.With().Str("component", "olxapi").Logger())
	client.API.OnTokens = client.saveTokens
	login.Client = client
	return nil
}
