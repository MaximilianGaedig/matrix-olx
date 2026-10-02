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

const (
	LoginFlowIDBrowser = "browser"
	LoginFlowIDToken   = "token"

	LoginStepIDCallback = "com.github.maximiliangaedig.olx.login.callback"
	LoginStepIDToken    = "com.github.maximiliangaedig.olx.login.token"
	LoginStepIDComplete = "com.github.maximiliangaedig.olx.login.complete"
)

func (oc *OLXConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "OLX login page",
		Description: "Open OLX's own login page in your browser and paste the address it sends you to. The bridge never sees your password.",
		ID:          LoginFlowIDBrowser,
	}, {
		Name:        "Refresh token",
		Description: "Paste a refresh token of an existing OLX session.",
		ID:          LoginFlowIDToken,
	}}
}

func (oc *OLXConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDBrowser, LoginFlowIDToken:
		return &OLXLogin{Main: oc, User: user, FlowID: flowID}, nil
	default:
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
}

type OLXLogin struct {
	Main   *OLXConnector
	User   *bridgev2.User
	FlowID string

	pkce *olxapi.PKCE
}

var _ bridgev2.LoginProcessUserInput = (*OLXLogin)(nil)

func (ol *OLXLogin) authConfig() olxapi.AuthConfig {
	return olxapi.AuthConfig{UserAgent: ol.Main.userAgent()}
}

func (ol *OLXLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	if ol.FlowID == LoginFlowIDToken {
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       LoginStepIDToken,
			Instructions: "Paste the refresh token of an OLX session.",
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{{
					Type: bridgev2.LoginInputFieldTypeToken,
					ID:   "refresh_token",
					Name: "Refresh token",
				}},
			},
		}, nil
	}
	ol.pkce = olxapi.NewPKCE()
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDCallback,
		Instructions: "1. Open this link in a browser and log in to OLX if it asks: " + ol.authConfig().AuthorizeURL(ol.pkce) + "\n" +
			"2. OLX then sends you to an address starting with " + olxapi.DefaultRedirectURI + "?code=… (the page itself may show an error or the OLX home page, that is fine).\n" +
			"3. Copy that whole address from the address bar and send it here. If the page moved on before you could copy it, press Back or open the link again.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:        bridgev2.LoginInputFieldTypeURL,
				ID:          "callback",
				Name:        "Address OLX sent you to",
				Description: "The whole address, or just the value of its code parameter",
			}},
		},
	}, nil
}

func (ol *OLXLogin) Cancel() {}

func (ol *OLXLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	var tokens *olxapi.Tokens
	var err error
	if ol.FlowID == LoginFlowIDToken {
		tokens, err = ol.authConfig().Refresh(ctx, ol.Main.httpClient, strings.TrimSpace(input["refresh_token"]))
		if err != nil {
			return nil, fmt.Errorf("OLX did not accept the refresh token: %w", err)
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
			return nil, fmt.Errorf("OLX did not accept the code (it is single-use and short-lived, open the link again): %w", err)
		}
	}
	return ol.finish(ctx, tokens)
}

func (ol *OLXLogin) finish(ctx context.Context, tokens *olxapi.Tokens) (*bridgev2.LoginStep, error) {
	claims, err := olxapi.ParseClaims(tokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("OLX returned an unreadable token: %w", err)
	}
	remoteName := claims.Email
	if remoteName == "" {
		remoteName = claims.Subject
	}
	ul, err := ol.User.NewLogin(ctx, &database.UserLogin{
		ID:         MakeUserLoginID(claims.Subject),
		RemoteName: remoteName,
		RemoteProfile: status.RemoteProfile{
			Email: claims.Email,
		},
		Metadata: &UserLoginMetadata{
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
		Instructions: fmt.Sprintf("Logged in to OLX as %s. Your chats are being synced.", remoteName),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
