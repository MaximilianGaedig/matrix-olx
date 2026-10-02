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

package olxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// WebConfig is what a site's web app is told about itself: every page of the
// site carries the app's release and its configuration. Reading it keeps the
// bridge from reporting a release that is long gone, and shows when OLX moves
// something the bridge has built in.
type WebConfig struct {
	// Version is the web app's release, which it sends as X-Client-Version.
	Version string
	// APIVersion is the version of the website's own API the app asks for.
	APIVersion string

	ChatURL       string
	SocketURL     string
	ModerationURL string
	// AuthHost and ClientID are the site's login: its Cognito domain and the
	// web app's client in it.
	AuthHost string
	ClientID string
}

var (
	webVersionPattern = regexp.MustCompile(`<meta\s+name="version"\s+content="([\w.-]{1,64})"`)
	initConfigPattern = regexp.MustCompile(`window\.__INIT_CONFIG__\s*=\s*("(?:[^"\\]|\\.)*")`)
	apiVersionPattern = regexp.MustCompile(`^v\d+(\.\d+)*$`)
)

// ErrNoWebConfig is a page that has neither the release nor the configuration.
var ErrNoWebConfig = errors.New("the page carries no web app release or configuration")

// ParseWebConfig reads the release and configuration out of a page of the site.
// Whatever part is missing or unreadable is left empty.
func ParseWebConfig(page []byte) (*WebConfig, error) {
	var cfg WebConfig
	if match := webVersionPattern.FindSubmatch(page); match != nil {
		cfg.Version = string(match[1])
	}
	var init struct {
		AppConfig struct {
			AuthConfig struct {
				Cognito struct {
					Host     string `json:"host"`
					ClientID string `json:"client_id"`
				} `json:"cognito"`
			} `json:"authConfig"`
			ChatAPIConfig struct {
				API struct {
					Core string `json:"core"`
				} `json:"api"`
				WS struct {
					URL string `json:"url"`
				} `json:"ws"`
			} `json:"chatApiConfig"`
			AtlasAuthConfig struct {
				APIVersion       string `json:"apiVersion"`
				ModerationAPIURL string `json:"moderationAPIUrl"`
			} `json:"atlasAuthConfig"`
		} `json:"appConfig"`
	}
	found := false
	if match := initConfigPattern.FindSubmatch(page); match != nil {
		// The configuration is JSON inside a JavaScript string.
		var encoded string
		if json.Unmarshal(match[1], &encoded) == nil && json.Unmarshal([]byte(encoded), &init) == nil {
			found = true
			app := init.AppConfig
			cfg.ChatURL = strings.TrimSuffix(app.ChatAPIConfig.API.Core, "/")
			cfg.SocketURL = strings.TrimSuffix(app.ChatAPIConfig.WS.URL, "/")
			cfg.ModerationURL = strings.TrimSuffix(app.AtlasAuthConfig.ModerationAPIURL, "/")
			cfg.ClientID = app.AuthConfig.Cognito.ClientID
			if host := app.AuthConfig.Cognito.Host; host != "" {
				cfg.AuthHost = "https://" + strings.TrimSuffix(strings.TrimPrefix(host, "https://"), "/")
			}
			if apiVersionPattern.MatchString(app.AtlasAuthConfig.APIVersion) {
				cfg.APIVersion = app.AtlasAuthConfig.APIVersion
			}
		}
	}
	if cfg.Version == "" && !found {
		return nil, ErrNoWebConfig
	}
	return &cfg, nil
}

// FetchWebConfig reads a site's current release and configuration from its
// front page.
func FetchWebConfig(ctx context.Context, client *http.Client, origin, userAgent, language string) (*WebConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if language != "" {
		req.Header.Set("Accept-Language", language)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/: HTTP %d", origin, resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return ParseWebConfig(page)
}

// Drift lists where the site's own configuration differs from what the bridge
// has built in for it: things the bridge does not follow on its own, because a
// wrong one would send logins or messages to the wrong place.
func (s Site) Drift(live *WebConfig) []string {
	if live == nil {
		return nil
	}
	var drift []string
	check := func(what, builtIn, now string) {
		if now != "" && now != builtIn {
			drift = append(drift, fmt.Sprintf("%s is now %s (built in: %s)", what, now, builtIn))
		}
	}
	check("the login client ID", s.ClientID, live.ClientID)
	check("the login host", s.authHost(), live.AuthHost)
	check("the chat API", s.chatURL(), live.ChatURL)
	check("the chat socket", s.socketURL(), live.SocketURL)
	check("the moderation API", DefaultModerationURL, live.ModerationURL)
	return drift
}
