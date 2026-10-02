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

// Package olxapi is a client for the chat that OLX's website uses: a REST API
// on api.chat.olx.pl, a WebSocket that pushes events, the Apollo file store
// for attachments, and the public profile API on www.olx.pl.
package olxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

const (
	DefaultUploadURL     = "https://ireland.apollo.olxcdn.com/v1/temp-files"
	DefaultModerationURL = "https://content.css.olx.io/api/v1"
	DefaultClientVersion = "f09f5710_10127260"
	DefaultPlatform      = "DESKTOP"

	// MaxPageSize is the largest conversation list page the website asks for.
	MaxPageSize = 40
	// tokenLeeway is how long before its expiry an ID token is renewed.
	tokenLeeway = 90 * time.Second
)

// Config is everything about OLX that could differ between sites or change
// with a new release of their web app.
type Config struct {
	// Site is the OLX site the account is on. Everything below that is left
	// empty follows from it.
	Site Site

	ChatURL   string
	SocketURL string
	WWWURL    string
	UploadURL string
	// NegotiationURL is the price negotiation service, ModerationURL the one
	// that takes reports about chats.
	NegotiationURL string
	ModerationURL  string
	SiteCode       string
	// ClientVersion pins the web app release the bridge reports. Left empty,
	// the release is the site's current one as far as Live knows it, and the
	// one the bridge was built against otherwise.
	ClientVersion string
	// Live returns the site's own current configuration, or nil while it is
	// not known.
	Live        func() *WebConfig
	Platform    string
	Language    string
	WWWLanguage string
	UserAgent   string
	DeviceID    string
	Auth        AuthConfig
}

func (c *Config) setDefaults() {
	if c.Site.Code == "" {
		c.Site = MustSite(DefaultSite)
	}
	def := func(field *string, value string) {
		if *field == "" {
			*field = value
		}
	}
	def(&c.ChatURL, c.Site.chatURL())
	def(&c.SocketURL, c.Site.socketURL())
	def(&c.WWWURL, c.Site.Origin())
	def(&c.UploadURL, DefaultUploadURL)
	def(&c.NegotiationURL, c.Site.negotiationURL())
	def(&c.ModerationURL, DefaultModerationURL)
	def(&c.SiteCode, c.Site.SiteCode)
	def(&c.Platform, DefaultPlatform)
	def(&c.Language, c.Site.Language)
	def(&c.WWWLanguage, c.Site.WWWLanguage)
	def(&c.UserAgent, ChromeUserAgent(DefaultChromeMajor))
	c.Auth.Site = c.Site
	c.Auth.UserAgent = c.UserAgent
	c.Auth.setDefaults()
}

// HTTPError is a response OLX answered with a non-2xx status.
type HTTPError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// IsStatus reports whether err is an HTTPError with the given status.
func IsStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == status
}

func truncate(body []byte) string {
	const max = 300
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Client talks to OLX as one logged-in user.
type Client struct {
	cfg Config
	log zerolog.Logger

	// HTTP is used for the chat API, the file store and the token endpoint.
	HTTP *http.Client
	// WWW is used for www.olx.pl, which OLX guards more strictly than the chat
	// API (it refuses datacenter addresses), so it may need its own proxy.
	WWW *http.Client

	// OnTokens is called whenever the session's tokens change, so that they
	// can be stored.
	OnTokens func(Tokens)

	tokenLock sync.Mutex
	tokens    Tokens

	apolloLock  sync.Mutex
	apolloToken string
}

func NewClient(cfg Config, tokens Tokens, httpClient, wwwClient *http.Client, log zerolog.Logger) *Client {
	cfg.setDefaults()
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if wwwClient == nil {
		wwwClient = httpClient
	}
	return &Client{cfg: cfg, log: log, HTTP: httpClient, WWW: wwwClient, tokens: tokens}
}

// Tokens returns the current session.
func (c *Client) Tokens() Tokens {
	c.tokenLock.Lock()
	defer c.tokenLock.Unlock()
	return c.tokens
}

// IDToken returns a bearer token that is valid for at least tokenLeeway,
// renewing it first if needed.
func (c *Client) IDToken(ctx context.Context) (string, error) {
	return c.idToken(ctx, "")
}

// idToken is IDToken, except that a token equal to `rejected` counts as
// expired whatever its expiry says (the server just refused it).
func (c *Client) idToken(ctx context.Context, rejected string) (string, error) {
	c.tokenLock.Lock()
	defer c.tokenLock.Unlock()
	if c.tokens.IDToken != "" && c.tokens.IDToken != rejected && time.Until(c.tokens.Expiry) > tokenLeeway {
		return c.tokens.IDToken, nil
	}
	fresh, err := c.cfg.Auth.Refresh(ctx, c.HTTP, c.tokens.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("failed to renew the OLX session: %w", err)
	}
	c.tokens = *fresh
	if c.OnTokens != nil {
		c.OnTokens(*fresh)
	}
	c.log.Debug().Time("expiry", fresh.Expiry).Msg("Renewed OLX ID token")
	return fresh.IDToken, nil
}

// Logout ends the session on OLX's side.
func (c *Client) Logout(ctx context.Context) error {
	c.tokenLock.Lock()
	refresh := c.tokens.RefreshToken
	c.tokens = Tokens{}
	c.tokenLock.Unlock()
	if refresh == "" {
		return nil
	}
	return c.cfg.Auth.Revoke(ctx, c.HTTP, refresh)
}

type request struct {
	method  string
	url     string
	query   url.Values
	body    any
	headers map[string]string
	www     bool
	// external is a request to one of OLX's services outside the site's own
	// domain (price negotiation, moderation): cross-site to the web app, and
	// without the chat API's client headers.
	external bool
	// noToken leaves the login's token out: for addresses that are not OLX's
	// own API.
	noToken bool
}

func (c *Client) do(ctx context.Context, req request, out any) error {
	var rejected string
	for attempt := 0; ; attempt++ {
		token, err := c.idToken(ctx, rejected)
		if err != nil {
			return err
		}
		err = c.doOnce(ctx, req, token, out)
		if attempt == 0 && IsStatus(err, http.StatusUnauthorized) {
			rejected = token
			continue
		}
		return err
	}
}

func (c *Client) doOnce(ctx context.Context, req request, token string, out any) error {
	fullURL := req.url
	if len(req.query) > 0 {
		fullURL += "?" + req.query.Encode()
	}
	var body io.Reader
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, fullURL, body)
	if err != nil {
		return err
	}
	if !req.noToken {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	if req.external {
		httpReq.Header.Set("Accept-Language", c.cfg.Language)
		httpReq.Header.Set("Content-Type", "application/json")
		setBrowserHeaders(httpReq.Header, c.cfg.UserAgent, c.cfg.Site.Origin(), siteCrossSite, req.method)
	} else if httpReq.Header.Set("X-Client", c.cfg.Platform); req.www {
		httpReq.Header.Set("Accept-Language", c.cfg.WWWLanguage)
		httpReq.Header.Set("X-Platform-Type", "mobile-html5")
		httpReq.Header.Set("Version", c.wwwAPIVersion())
		if c.cfg.DeviceID != "" {
			httpReq.Header.Set("X-Device-Id", c.cfg.DeviceID)
		}
		setBrowserHeaders(httpReq.Header, c.cfg.UserAgent, c.cfg.Site.Origin(), siteSameOrigin, req.method)
	} else {
		httpReq.Header.Set("Accept-Language", c.cfg.Language)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-Site-Code", c.cfg.SiteCode)
		httpReq.Header.Set("X-Client-Version", c.ClientVersion())
		setBrowserHeaders(httpReq.Header, c.cfg.UserAgent, c.cfg.Site.Origin(), siteSameSite, req.method)
	}
	for key, value := range req.headers {
		httpReq.Header.Set(key, value)
	}
	httpClient := c.HTTP
	if req.www {
		httpClient = c.WWW
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	// The query can hold IDs, never secrets, but keep it out of error text anyway.
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Method: req.method, URL: req.url, Status: resp.StatusCode, Body: truncate(respBody)}
	}
	if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	if err = json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s %s: unexpected response: %w", req.method, req.url, err)
	}
	return nil
}

var apiV2 = map[string]string{"X-Api-Version": "2"}

func boolParam(query url.Values, key string, value *bool) {
	if value == nil {
		return
	}
	if *value {
		query.Set(key, "1")
	} else {
		query.Set(key, "0")
	}
}

// ListConversations returns one page of conversations, newest first, and the
// address of the next page ("" on the last one).
func (c *Client) ListConversations(ctx context.Context, params ListParams) ([]*Conversation, string, error) {
	query := url.Values{}
	boolParam(query, "archived", params.Archived)
	boolParam(query, "my_ads", params.MyAds)
	boolParam(query, "observed", params.Observed)
	boolParam(query, "unread", params.Unread)
	if params.AdID != "" {
		query.Set("ad_id", params.AdID)
	}
	if params.Limit > 0 {
		query.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Offset > 0 {
		query.Set("offset", strconv.Itoa(params.Offset))
	}
	return c.listPage(ctx, request{method: http.MethodGet, url: c.cfg.ChatURL + "/api/conversations", query: query, headers: apiV2})
}

// ListConversationsPage follows a next-page address from ListConversations.
func (c *Client) ListConversationsPage(ctx context.Context, nextURL string) ([]*Conversation, string, error) {
	// Only ever send the session's token to the chat API itself.
	if !strings.HasPrefix(nextURL, c.cfg.ChatURL+"/") {
		return nil, "", fmt.Errorf("refusing to follow next page link to another host")
	}
	return c.listPage(ctx, request{method: http.MethodGet, url: nextURL, headers: apiV2})
}

func (c *Client) listPage(ctx context.Context, req request) ([]*Conversation, string, error) {
	var resp conversationList
	if err := c.do(ctx, req, &resp); err != nil {
		return nil, "", err
	}
	next := ""
	if resp.Links.Next != nil {
		next = resp.Links.Next.Href
	}
	return resp.Data, next, nil
}

// ListAllConversations walks every page of a list.
func (c *Client) ListAllConversations(ctx context.Context, params ListParams, maxPages int) ([]*Conversation, error) {
	if params.Limit == 0 {
		params.Limit = MaxPageSize
	}
	var all []*Conversation
	page, next, err := c.ListConversations(ctx, params)
	for pages := 1; ; pages++ {
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if next == "" || len(page) == 0 || (maxPages > 0 && pages >= maxPages) {
			return all, nil
		}
		page, next, err = c.ListConversationsPage(ctx, next)
	}
}

// GetConversation returns a conversation with its messages, or nil if there
// is no such conversation (any more).
func (c *Client) GetConversation(ctx context.Context, conversationID string) (*Conversation, error) {
	var resp struct {
		Data *Conversation `json:"data"`
	}
	err := c.do(ctx, request{
		method:  http.MethodGet,
		url:     c.cfg.ChatURL + "/api/conversations/" + url.PathEscape(conversationID),
		headers: apiV2,
	}, &resp)
	if IsStatus(err, http.StatusNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) GetCounters(ctx context.Context) (*Counters, error) {
	var resp struct {
		Data *Counters `json:"data"`
	}
	err := c.do(ctx, request{method: http.MethodGet, url: c.cfg.ChatURL + "/api/conversations/counters"}, &resp)
	return resp.Data, err
}

type sendRequest struct {
	AdID    int64           `json:"ad_id,omitempty"`
	Message OutgoingMessage `json:"message"`
}

type sendResponse struct {
	Data json.RawMessage `json:"data"`
}

// SendMessage posts a message to a conversation. The message ID is chosen by
// the sender: msg.MessageID is filled in when empty.
func (c *Client) SendMessage(ctx context.Context, conversationID string, msg *OutgoingMessage) (json.RawMessage, error) {
	if msg.MessageID == "" {
		msg.MessageID = uuid.NewString()
	}
	var resp sendResponse
	err := c.do(ctx, request{
		method: http.MethodPost,
		url:    c.cfg.ChatURL + "/api/conversations/" + url.PathEscape(conversationID) + "/messages",
		body:   &sendRequest{Message: *msg},
	}, &resp)
	return resp.Data, err
}

// StartConversation sends the first message about an ad, which is what
// creates a conversation. It returns the server's answer as is.
func (c *Client) StartConversation(ctx context.Context, adID int64, msg *OutgoingMessage) (json.RawMessage, error) {
	if msg.MessageID == "" {
		msg.MessageID = uuid.NewString()
	}
	var resp sendResponse
	err := c.do(ctx, request{
		method: http.MethodPost,
		url:    c.cfg.ChatURL + "/api/conversations",
		body:   &sendRequest{AdID: adID, Message: *msg},
	}, &resp)
	return resp.Data, err
}

type eventUUID struct {
	EventUUID string `json:"event_uuid"`
}

// MarkRead marks every message in the conversation as read.
func (c *Client) MarkRead(ctx context.Context, conversationID string) error {
	return c.do(ctx, request{
		method: http.MethodPost,
		url:    c.cfg.ChatURL + "/api/conversations/" + url.PathEscape(conversationID) + "/read",
		body:   &eventUUID{EventUUID: uuid.NewString()},
	}, nil)
}

const (
	TypingStarted = "typing_started"
	TypingStopped = "typing_stopped"
)

// SendTyping tells the other person the user started or stopped typing.
func (c *Client) SendTyping(ctx context.Context, conversationID string, typing bool) error {
	kind := TypingStopped
	if typing {
		kind = TypingStarted
	}
	return c.do(ctx, request{
		method: http.MethodPost,
		url:    c.cfg.ChatURL + "/api/typing",
		body:   map[string]string{"conversation_id": conversationID, "type": kind},
	}, nil)
}

// SetSaved adds the conversation to the saved ("Zapisane") list or takes it
// off it.
func (c *Client) SetSaved(ctx context.Context, conversationID string, saved bool) error {
	if saved {
		return c.do(ctx, request{
			method: http.MethodPost,
			url:    c.cfg.ChatURL + "/api/conversations/saved",
			body:   map[string]string{"conversation_id": conversationID},
		}, nil)
	}
	return c.do(ctx, request{
		method: http.MethodDelete,
		url:    c.cfg.ChatURL + "/api/conversations/saved/" + url.PathEscape(conversationID),
		body:   struct{}{},
	}, nil)
}

// SetTrashed moves the conversation to the trash ("Kosz") or back out of it.
func (c *Client) SetTrashed(ctx context.Context, conversationID string, trashed bool) error {
	if trashed {
		return c.do(ctx, request{
			method: http.MethodPost,
			url:    c.cfg.ChatURL + "/api/conversations/trash",
			body:   map[string]string{"conversation_id": conversationID, "event_uuid": uuid.NewString()},
		}, nil)
	}
	return c.do(ctx, request{
		method: http.MethodDelete,
		url:    c.cfg.ChatURL + "/api/conversations/trash/" + url.PathEscape(conversationID),
		body:   &eventUUID{EventUUID: uuid.NewString()},
	}, nil)
}

// DeleteConversation removes the conversation for good.
func (c *Client) DeleteConversation(ctx context.Context, conversationID string) error {
	return c.do(ctx, request{
		method: http.MethodDelete,
		url:    c.cfg.ChatURL + "/api/conversations/" + url.PathEscape(conversationID),
		body:   &eventUUID{EventUUID: uuid.NewString()},
	}, nil)
}

// SetBlocked blocks or unblocks a person.
func (c *Client) SetBlocked(ctx context.Context, respondentUUID string, blocked bool) error {
	if blocked {
		return c.do(ctx, request{
			method: http.MethodPost,
			url:    c.cfg.ChatURL + "/api/respondents/blocked",
			body:   map[string]string{"respondent_uuid": respondentUUID},
		}, nil)
	}
	return c.do(ctx, request{
		method: http.MethodDelete,
		url:    c.cfg.ChatURL + "/api/respondents/blocked/" + url.PathEscape(respondentUUID),
		body:   struct{}{},
	}, nil)
}

func (c *Client) fetchApolloToken(ctx context.Context) (string, error) {
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	err := c.do(ctx, request{method: http.MethodPost, url: c.cfg.ChatURL + "/api/apollo/token", body: struct{}{}}, &resp)
	if err != nil {
		return "", err
	} else if resp.Data.Token == "" {
		return "", errors.New("OLX returned no upload token")
	}
	return resp.Data.Token, nil
}

func (c *Client) uploadOnce(ctx context.Context, token string, data []byte, mimeType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Expires", time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02T15:04:05.000Z"))
	setBrowserHeaders(req.Header, c.cfg.UserAgent, c.cfg.Site.Origin(), siteCrossSite, http.MethodPost)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{Method: http.MethodPost, URL: c.cfg.UploadURL, Status: resp.StatusCode, Body: truncate(body)}
	}
	var parsed struct {
		Data struct {
			Filename string `json:"filename"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil || parsed.Data.Filename == "" {
		return "", fmt.Errorf("unexpected upload response: %s", truncate(body))
	}
	return parsed.Data.Filename, nil
}

// Upload stores a file and returns the attachment to send with a message.
func (c *Client) Upload(ctx context.Context, data []byte, filename, mimeType string) (*OutgoingAttachment, error) {
	c.apolloLock.Lock()
	token := c.apolloToken
	c.apolloLock.Unlock()
	var err error
	fresh := token == ""
	if fresh {
		if token, err = c.fetchApolloToken(ctx); err != nil {
			return nil, fmt.Errorf("failed to get upload token: %w", err)
		}
	}
	fileID, err := c.uploadOnce(ctx, token, data, mimeType)
	if err != nil && !fresh {
		// The cached token may have expired: try once more with a new one.
		if token, err = c.fetchApolloToken(ctx); err != nil {
			return nil, fmt.Errorf("failed to get upload token: %w", err)
		}
		fileID, err = c.uploadOnce(ctx, token, data, mimeType)
	}
	if err != nil {
		return nil, err
	}
	c.apolloLock.Lock()
	c.apolloToken = token
	c.apolloLock.Unlock()
	return &OutgoingAttachment{Filename: filename, FileID: fileID}, nil
}

// Download fetches an attachment or an image. OLX's file links are
// pre-authorized, so no session token is sent with the request.
func (c *Client) Download(ctx context.Context, fileURL string, maxSize int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, "", err
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", &HTTPError{Method: http.MethodGet, URL: req.URL.Host + req.URL.Path, Status: resp.StatusCode, Body: truncate(body)}
	}
	if maxSize <= 0 {
		maxSize = 100 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", err
	} else if int64(len(data)) > maxSize {
		return nil, "", fmt.Errorf("file is larger than %d bytes", maxSize)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// DefaultWWWAPIVersion is the version of its own API the website asked
// www.olx.pl for when the bridge was built.
const DefaultWWWAPIVersion = "v1.19"

func (c *Client) live() *WebConfig {
	if c.cfg.Live == nil {
		return nil
	}
	return c.cfg.Live()
}

// ClientVersion is the web app release the client reports: the pinned one,
// else the site's current one, else the one the bridge was built against.
func (c *Client) ClientVersion() string {
	if c.cfg.ClientVersion != "" {
		return c.cfg.ClientVersion
	}
	if live := c.live(); live != nil && live.Version != "" {
		return live.Version
	}
	return DefaultClientVersion
}

func (c *Client) wwwAPIVersion() string {
	if live := c.live(); live != nil && live.APIVersion != "" {
		return live.APIVersion
	}
	return DefaultWWWAPIVersion
}

// MaxUsersPerRequest is how many profiles OLX lets one request ask for; more
// is answered with a validation error.
const MaxUsersPerRequest = 10

// GetUsers returns the public profiles of the given users, with their online
// status. This is on www.olx.pl, see [Client.WWW].
func (c *Client) GetUsers(ctx context.Context, uuids []string) ([]*User, error) {
	var all []*User
	for start := 0; start < len(uuids); start += MaxUsersPerRequest {
		batch := uuids[start:min(start+MaxUsersPerRequest, len(uuids))]
		query := url.Values{}
		for i, id := range batch {
			query.Set("user_uuids["+strconv.Itoa(i)+"]", id)
		}
		var resp struct {
			Data []*User `json:"data"`
		}
		err := c.do(ctx, request{method: http.MethodGet, url: c.cfg.WWWURL + "/api/v1/users/", query: query, www: true}, &resp)
		if err != nil {
			return all, err
		}
		all = append(all, resp.Data...)
	}
	return all, nil
}
