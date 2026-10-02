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

package olxapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OLX signs users in through an AWS Cognito user pool behind login.olx.pl. The
// web app is a public OAuth client (no secret) that uses the authorization
// code flow with PKCE; the bridge does the same and gets a session of its own,
// separate from any browser's.
const (
	DefaultAuthHost    = "https://login.olx.pl"
	DefaultClientID    = "6j7elk01p32o648o1io8lvhhab"
	DefaultRedirectURI = "https://www.olx.pl/d/callback/"
	authScope          = "openid email profile"
)

// ErrLoggedOut means OLX no longer accepts the refresh token: the user has to
// log in again.
var ErrLoggedOut = errors.New("the OLX session is no longer valid")

// Tokens is a login session. The ID token is what OLX's APIs take as the
// bearer token; it lives for 15 minutes and is renewed with the refresh token.
type Tokens struct {
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	Expiry       time.Time `json:"expiry"`
}

// Claims are the parts of the ID token the bridge reads.
type Claims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Expiry  int64  `json:"exp"`
}

// ParseClaims reads an ID token's payload. The signature is not checked: the
// token came straight from OLX over TLS and is only used to learn who logged
// in and when it expires.
func ParseClaims(idToken string) (*Claims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("invalid JWT payload: %w", err)
	}
	var claims Claims
	if err = json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("invalid JWT payload: %w", err)
	}
	if claims.Subject == "" {
		return nil, errors.New("JWT has no subject")
	}
	return &claims, nil
}

// AuthConfig says which OAuth client to be.
type AuthConfig struct {
	Host        string
	ClientID    string
	RedirectURI string
	UserAgent   string
}

func (ac *AuthConfig) setDefaults() {
	if ac.Host == "" {
		ac.Host = DefaultAuthHost
	}
	if ac.ClientID == "" {
		ac.ClientID = DefaultClientID
	}
	if ac.RedirectURI == "" {
		ac.RedirectURI = DefaultRedirectURI
	}
}

// PKCE is the secret half (verifier) and public half (challenge) of one
// authorization attempt, plus its state value.
type PKCE struct {
	Verifier  string
	Challenge string
	State     string
}

func randomURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func NewPKCE() *PKCE {
	verifier := randomURLSafe(48)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		State:     randomURLSafe(18),
	}
}

// AuthorizeURL is the page the user opens to approve the login.
func (ac AuthConfig) AuthorizeURL(pkce *PKCE) string {
	ac.setDefaults()
	query := url.Values{
		"client_id":             {ac.ClientID},
		"response_type":         {"code"},
		"scope":                 {authScope},
		"redirect_uri":          {ac.RedirectURI},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.State},
	}
	return ac.Host + "/oauth2/authorize?" + query.Encode()
}

// ParseCallback takes what the user pasted after approving the login - the
// whole address they were sent to, or just the code - and returns the code.
// When the address carries a state, it has to be the one this attempt made.
func ParseCallback(input string, pkce *PKCE) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("nothing was pasted")
	}
	if !strings.Contains(input, "://") && !strings.ContainsAny(input, "?&=") {
		return input, nil
	}
	rawQuery := input
	if parsed, err := url.Parse(input); err == nil && (parsed.RawQuery != "" || parsed.Fragment != "") {
		rawQuery = parsed.RawQuery
		if rawQuery == "" {
			rawQuery = parsed.Fragment
		}
	} else if idx := strings.IndexByte(input, '?'); idx >= 0 {
		rawQuery = input[idx+1:]
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("could not read the pasted address: %w", err)
	}
	if errCode := query.Get("error"); errCode != "" {
		return "", fmt.Errorf("OLX refused the login: %s %s", errCode, query.Get("error_description"))
	}
	code := query.Get("code")
	if code == "" {
		return "", errors.New("the pasted address has no code in it")
	}
	if state := query.Get("state"); state != "" && pkce != nil && state != pkce.State {
		return "", errors.New("the pasted address belongs to a different login attempt, start again")
	}
	return code, nil
}

type tokenResponse struct {
	IDToken          string `json:"id_token"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (ac AuthConfig) tokenRequest(ctx context.Context, httpClient *http.Client, form url.Values) (*tokenResponse, error) {
	ac.setDefaults()
	form.Set("client_id", ac.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ac.Host+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if ac.UserAgent != "" {
		req.Header.Set("User-Agent", ac.UserAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed tokenResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return nil, &HTTPError{Method: http.MethodPost, URL: ac.Host + "/oauth2/token", Status: resp.StatusCode, Body: truncate(body)}
	}
	if parsed.Error != "" {
		if parsed.Error == "invalid_grant" {
			return nil, fmt.Errorf("%w: %s", ErrLoggedOut, strings.TrimSpace(parsed.Error+" "+parsed.ErrorDescription))
		}
		return nil, fmt.Errorf("token request failed: %s %s", parsed.Error, parsed.ErrorDescription)
	}
	if resp.StatusCode/100 != 2 || parsed.IDToken == "" {
		return nil, &HTTPError{Method: http.MethodPost, URL: ac.Host + "/oauth2/token", Status: resp.StatusCode, Body: truncate(body)}
	}
	return &parsed, nil
}

func (tr *tokenResponse) tokens(prevRefresh string) (*Tokens, error) {
	claims, err := ParseClaims(tr.IDToken)
	if err != nil {
		return nil, err
	}
	out := &Tokens{RefreshToken: tr.RefreshToken, IDToken: tr.IDToken}
	if out.RefreshToken == "" {
		// Cognito only sends a new refresh token when rotation is on.
		out.RefreshToken = prevRefresh
	}
	switch {
	case claims.Expiry > 0:
		out.Expiry = time.Unix(claims.Expiry, 0)
	case tr.ExpiresIn > 0:
		out.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	default:
		out.Expiry = time.Now().Add(15 * time.Minute)
	}
	return out, nil
}

// ExchangeCode finishes the authorization code flow.
func (ac AuthConfig) ExchangeCode(ctx context.Context, httpClient *http.Client, code string, pkce *PKCE) (*Tokens, error) {
	ac.setDefaults()
	resp, err := ac.tokenRequest(ctx, httpClient, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {ac.RedirectURI},
		"code_verifier": {pkce.Verifier},
	})
	if err != nil {
		return nil, err
	}
	tokens, err := resp.tokens("")
	if err != nil {
		return nil, err
	} else if tokens.RefreshToken == "" {
		return nil, errors.New("OLX returned no refresh token")
	}
	return tokens, nil
}

// Refresh gets a new ID token.
func (ac AuthConfig) Refresh(ctx context.Context, httpClient *http.Client, refreshToken string) (*Tokens, error) {
	if refreshToken == "" {
		return nil, ErrLoggedOut
	}
	resp, err := ac.tokenRequest(ctx, httpClient, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, err
	}
	return resp.tokens(refreshToken)
}

// Revoke ends the session on OLX's side.
func (ac AuthConfig) Revoke(ctx context.Context, httpClient *http.Client, refreshToken string) error {
	ac.setDefaults()
	form := url.Values{"token": {refreshToken}, "client_id": {ac.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ac.Host+"/oauth2/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ac.UserAgent != "" {
		req.Header.Set("User-Agent", ac.UserAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Method: http.MethodPost, URL: ac.Host + "/oauth2/revoke", Status: resp.StatusCode, Body: truncate(body)}
	}
	return nil
}
