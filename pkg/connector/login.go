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

// Login flows are named "<method>-<site>": "page-pl", "token-ua". The plain
// method names stand for the default site.
const (
	LoginMethodPage  = "page"
	LoginMethodToken = "token"

	LoginStepIDCallback = "com.github.maximiliangaedig.olx.login.callback"
	LoginStepIDToken    = "com.github.maximiliangaedig.olx.login.token"
	LoginStepIDComplete = "com.github.maximiliangaedig.olx.login.complete"
)

func (oc *OLXConnector) defaultSite() olxapi.Site {
	site, err := olxapi.LookupSite(oc.Config.DefaultSite)
	if err != nil {
		return olxapi.MustSite(olxapi.DefaultSite)
	}
	return site
}

func (oc *OLXConnector) GetLoginFlows() []bridgev2.LoginFlow {
	def := oc.defaultSite()
	sites := []olxapi.Site{def}
	for _, site := range olxapi.Sites {
		if site.Code != def.Code {
			sites = append(sites, site)
		}
	}
	flows := make([]bridgev2.LoginFlow, 0, 2*len(sites))
	for _, site := range sites {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        site.Name() + " login page",
			Description: "Log in on " + site.Name() + "'s own login page and paste the address it sends you to. The bridge gets a session of its own and never sees your password.",
			ID:          LoginMethodPage + "-" + site.Code,
		})
	}
	for _, site := range sites {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        site.Name() + " session token",
			Description: "Copy the session token out of a browser that is logged in to " + site.Name() + ". The bridge then shares that browser's session.",
			ID:          LoginMethodToken + "-" + site.Code,
		})
	}
	return flows
}

// parseFlowID splits a flow ID into its method and site.
func (oc *OLXConnector) parseFlowID(flowID string) (method string, site olxapi.Site, err error) {
	method, siteName, hasSite := strings.Cut(flowID, "-")
	if !hasSite {
		site = oc.defaultSite()
	} else if site, err = olxapi.LookupSite(siteName); err != nil {
		return "", site, err
	}
	switch method {
	case LoginMethodPage, "browser":
		return LoginMethodPage, site, nil
	case LoginMethodToken:
		return LoginMethodToken, site, nil
	default:
		return "", site, fmt.Errorf("unknown login flow %q", flowID)
	}
}

func (oc *OLXConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	method, site, err := oc.parseFlowID(flowID)
	if err != nil {
		return nil, err
	}
	return &OLXLogin{Main: oc, User: user, Method: method, Site: site}, nil
}

type OLXLogin struct {
	Main   *OLXConnector
	User   *bridgev2.User
	Method string
	Site   olxapi.Site

	pkce *olxapi.PKCE
}

var _ bridgev2.LoginProcessUserInput = (*OLXLogin)(nil)

func (ol *OLXLogin) authConfig() olxapi.AuthConfig {
	return olxapi.AuthConfig{Site: ol.Site, UserAgent: ol.Main.userAgent()}
}

// tokenSnippet reads the refresh token the web app keeps in the browser's
// local storage and puts it on the clipboard.
const tokenSnippet = `copy(JSON.parse(localStorage[Object.keys(localStorage).find(k=>k.startsWith("@@auth0spajs@@::")&&!k.includes("@@user@@"))]).body.refresh_token)`

func (ol *OLXLogin) pageInstructions(link string) string {
	callback := ol.authConfig().Redirect()
	return "OLX's login ends on a page that jumps to the home page right away, so the address has to be read without letting that page run:\n\n" +
		"1. Be logged in to " + ol.Site.Name() + " in your browser.\n" +
		"2. Copy this link (copy it, don't open it):\n\n" + link + "\n\n" +
		"3. Open a new tab, type `view-source:` into the address bar, paste the link directly after it and press Enter.\n" +
		"4. The tab shows a page of code, and the address bar now reads `view-source:" + callback + "?code=…`. Copy that whole address and send it here.\n\n" +
		"If you opened the link the normal way instead: the address you need is in the browser's history (Ctrl+H), the entry that starts with `" +
		strings.TrimPrefix(callback, "https://") + "?code=`. It works once and for a few minutes; `login` again gives a new link."
}

func (ol *OLXLogin) tokenInstructions() string {
	return "1. Open " + ol.Site.Origin() + " in a browser where you are logged in.\n" +
		"2. Open the developer console (F12, then the Console tab). If the browser asks you to, type `allow pasting` first.\n" +
		"3. Paste this line and press Enter. It copies the session token to your clipboard:\n\n" +
		"`" + tokenSnippet + "`\n\n" +
		"4. Paste the clipboard here.\n\n" +
		"The bridge then uses the same session as that browser: logging out there can log the bridge out too. The login page method gives the bridge a session of its own."
}

func (ol *OLXLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	if ol.Method == LoginMethodToken {
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       LoginStepIDToken,
			Instructions: ol.tokenInstructions(),
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{{
					Type: bridgev2.LoginInputFieldTypeToken,
					ID:   "refresh_token",
					Name: "Session token",
				}},
			},
		}, nil
	}
	ol.pkce = olxapi.NewPKCE()
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       LoginStepIDCallback,
		Instructions: ol.pageInstructions(ol.authConfig().AuthorizeURL(ol.pkce)),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:        bridgev2.LoginInputFieldTypeURL,
				ID:          "callback",
				Name:        "Address from the address bar",
				Description: "The whole address, with or without view-source: in front",
			}},
		},
	}, nil
}

func (ol *OLXLogin) Cancel() {}

// cleanToken strips what tends to come along when a token is pasted: quotes,
// backticks, whitespace.
func cleanToken(input string) string {
	return strings.Trim(strings.TrimSpace(input), "\"'` \n\t")
}

func (ol *OLXLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	var tokens *olxapi.Tokens
	var err error
	if ol.Method == LoginMethodToken {
		token := cleanToken(input["refresh_token"])
		if token == "" || token == "undefined" || token == "null" {
			return nil, fmt.Errorf("that is not a token: the browser has no %s session to copy (are you logged in there?)", ol.Site.Name())
		}
		tokens, err = ol.authConfig().Refresh(ctx, ol.Main.httpClient, token)
		if err != nil {
			return nil, fmt.Errorf("%s did not accept the token: %w", ol.Site.Name(), err)
		}
	} else {
		if ol.pkce == nil {
			return nil, fmt.Errorf("login was not started")
		}
		code, err := olxapi.ParseCallback(input["callback"], ol.pkce)
		if err != nil {
			return nil, err
		}
		tokens, err = ol.authConfig().ExchangeCode(ctx, ol.Main.httpClient, code, ol.pkce)
		if err != nil {
			return nil, fmt.Errorf("%s did not accept the code (it works once and only for a few minutes: send `login` again for a new link): %w", ol.Site.Name(), err)
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
