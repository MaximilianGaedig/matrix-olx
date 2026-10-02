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
	"strings"

	"github.com/google/uuid"
	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/MaximilianGaedig/mautrix-olx/pkg/olxapi"
)

// There is one login flow per configured site, named by the site's code. With
// a single site, `login` needs no choice at all.
const (
	LoginStepIDInput    = "com.github.maximiliangaedig.olx.login.input"
	LoginStepIDComplete = "com.github.maximiliangaedig.olx.login.complete"
)

// loginSites are the configured sites, in order, each once.
func (oc *OLXConnector) loginSites() []olxapi.Site {
	var sites []olxapi.Site
	seen := map[string]bool{}
	for _, name := range oc.Config.Sites {
		if site, err := olxapi.LookupSite(name); err == nil && !seen[site.Code] {
			seen[site.Code] = true
			sites = append(sites, site)
		}
	}
	if len(sites) == 0 {
		sites = append(sites, olxapi.MustSite(olxapi.DefaultSite))
	}
	return sites
}

func (oc *OLXConnector) GetLoginFlows() []bridgev2.LoginFlow {
	sites := oc.loginSites()
	flows := make([]bridgev2.LoginFlow, len(sites))
	for i, site := range sites {
		flows[i] = bridgev2.LoginFlow{
			Name:        site.Name(),
			Description: "Log in to " + site.Name(),
			ID:          site.Code,
		}
	}
	return flows
}

func (oc *OLXConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	for _, site := range oc.loginSites() {
		if site.Code == flowID {
			return &OLXLogin{Main: oc, User: user, Site: site}, nil
		}
	}
	return nil, fmt.Errorf("unknown login flow %q", flowID)
}

type OLXLogin struct {
	Main *OLXConnector
	User *bridgev2.User
	Site olxapi.Site

	pkce *olxapi.PKCE
}

var _ bridgev2.LoginProcessUserInput = (*OLXLogin)(nil)

func (ol *OLXLogin) authConfig() olxapi.AuthConfig {
	return olxapi.AuthConfig{Site: ol.Site, UserAgent: ol.Main.userAgent()}
}

// tokenSnippet reads the refresh token the web app keeps in the browser's
// local storage and puts it on the clipboard.
const tokenSnippet = `copy(JSON.parse(localStorage[Object.keys(localStorage).find(k=>k.startsWith("@@auth0spajs@@::")&&!k.includes("@@user@@"))]).body.refresh_token)`

// instructions explains the login. OLX's login ends on a page that jumps to
// the home page at once, so its address is read through view-source:, where
// the page does not run. A browser will not follow a link to view-source:, so
// that address is given whole, to be pasted. Phones have no view-source: the
// plain link and the browser history do the same there. The last way is the
// session token of a logged-in browser.
func (ol *OLXLogin) instructions(link string) string {
	callback := strings.TrimPrefix(ol.authConfig().Redirect(), "https://")
	return "Log in to " + ol.Site.Name() + " in your browser first, then:\n\n" +
		"1. Copy this whole line and paste it into the address bar of a new tab:\n\n" +
		"```\nview-source:" + link + "\n```\n\n" +
		"2. Copy the address the tab ends on (`view-source:https://" + callback + "?code=…`) and send it here.\n\n" +
		"**On a phone** (no `view-source:` there): open [this link](" + link + "), let it land on the OLX home page, " +
		"then copy the `" + callback + "?code=…` entry from the browser's history and send it here.\n\n" +
		"**Or share a browser's session:** on " + ol.Site.Domain + " open the console (F12), run this and send what it copies:\n\n" +
		"```\n" + tokenSnippet + "\n```"
}

func (ol *OLXLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	ol.pkce = olxapi.NewPKCE()
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       LoginStepIDInput,
		Instructions: ol.instructions(ol.authConfig().AuthorizeURL(ol.pkce)),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypeURL,
				ID:   "input",
				Name: "Address or token",
			}},
		},
	}, nil
}

func (ol *OLXLogin) Cancel() {}

// cleanInput strips what tends to come along when something is pasted:
// quotes, backticks, whitespace.
func cleanInput(input string) string {
	return strings.Trim(strings.TrimSpace(input), "\"'` \n\t")
}

// looksLikeToken tells a session token from an address or a code: the code is
// a short UUID, the token a long encrypted JWT.
func looksLikeToken(input string) bool {
	return len(input) > 200 && !strings.Contains(input, "://") && !strings.Contains(input, "code=")
}

func (ol *OLXLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if ol.pkce == nil {
		return nil, fmt.Errorf("login was not started")
	}
	pasted := cleanInput(input["input"])
	if pasted == "undefined" || pasted == "null" {
		return nil, fmt.Errorf("the browser has no %s session to copy: log in there first", ol.Site.Name())
	}
	var tokens *olxapi.Tokens
	if looksLikeToken(pasted) {
		var err error
		tokens, err = ol.authConfig().Refresh(ctx, ol.Main.httpClient, pasted)
		if err != nil {
			return nil, fmt.Errorf("%s did not accept the token: %w", ol.Site.Name(), err)
		}
	} else {
		code, err := olxapi.ParseCallback(pasted, ol.pkce)
		if err != nil {
			return nil, err
		}
		tokens, err = ol.authConfig().ExchangeCode(ctx, ol.Main.httpClient, code, ol.pkce)
		if err != nil {
			return nil, fmt.Errorf("%s did not accept the code (it works once, for a few minutes: send `login` again for a new link): %w", ol.Site.Name(), err)
		}
	}
	return ol.finish(ctx, tokens)
}

func (ol *OLXLogin) finish(ctx context.Context, tokens *olxapi.Tokens) (*bridgev2.LoginStep, error) {
	claims, err := olxapi.ParseClaims(tokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("OLX returned an unreadable token: %w", err)
	}
	account := claims.Email
	if account == "" {
		account = claims.Subject
	}
	remoteName := account + " (" + ol.Site.Name() + ")"
	ul, err := ol.User.NewLogin(ctx, &database.UserLogin{
		ID:         MakeUserLoginID(claims.Subject),
		RemoteName: remoteName,
		RemoteProfile: status.RemoteProfile{
			Email: claims.Email,
		},
		Metadata: &UserLoginMetadata{
			Site:          ol.Site.Code,
			RefreshToken:  tokens.RefreshToken,
			IDToken:       tokens.IDToken,
			IDTokenExpiry: jsontime.U(tokens.Expiry),
			Email:         claims.Email,
			DeviceID:      uuid.NewString(),
		},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save the login: %w", err)
	}
	go ul.Client.Connect(ul.Log.WithContext(ol.Main.Bridge.BackgroundCtx))
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepIDComplete,
		Instructions: fmt.Sprintf("Logged in to %s as %s. Your chats are being synced.", ol.Site.Name(), account),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
