package olxapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
)

const (
	testRedirect = "https://www.olx.pl/d/callback/"
	testOrigin   = "https://www.olx.pl"
)

const testUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

func fakeJWT(sub string, exp time.Time) string {
	enc := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return enc(map[string]string{"alg": "none"}) + "." +
		enc(map[string]any{"sub": sub, "email": "user@example.com", "exp": exp.Unix()}) + ".sig"
}

// fakeOLX stands in for the token endpoint, the chat API and the socket.
type fakeOLX struct {
	t      *testing.T
	server *httptest.Server

	lock          sync.Mutex
	validToken    string
	refreshes     int
	refreshTokens []string
	requests      []*http.Request
	bodies        []string
	socketProtos  []string
	socketOrigin  string
	socketFrames  chan string
	pings         atomic.Int32
}

func newFakeOLX(t *testing.T) *fakeOLX {
	f := &fakeOLX{t: t, socketFrames: make(chan string, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", f.handleToken)
	mux.HandleFunc("/ws", f.handleSocket)
	mux.HandleFunc("/", f.handleAPI)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOLX) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.lock.Lock()
	defer f.lock.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Form.Get("grant_type") {
	case "refresh_token":
		if r.Form.Get("refresh_token") != "good-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid \"refresh_token\"."}`)
			return
		}
		f.refreshes++
		f.refreshTokens = append(f.refreshTokens, r.Form.Get("refresh_token"))
		f.validToken = fakeJWT("sub-1", time.Now().Add(15*time.Minute)) + fmt.Sprint(f.refreshes)
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": f.validToken, "access_token": "a", "expires_in": 900})
	case "authorization_code":
		if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != testRedirect {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		f.validToken = fakeJWT("sub-1", time.Now().Add(15*time.Minute))
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": f.validToken, "refresh_token": "good-refresh", "expires_in": 900})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unsupported_grant_type"}`)
	}
}

func (f *fakeOLX) handleAPI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.lock.Lock()
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, string(body))
	valid := f.validToken
	f.lock.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/upload" {
		// The file store takes its own token, not the session's.
		if r.Header.Get("Authorization") != "Bearer apollo-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"filename":"stored-file-id"}}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+valid {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/conversations":
		if r.URL.Query().Get("offset") == "40" {
			_, _ = io.WriteString(w, `{"data":[{"id":"c3","user_uuid":"me","respondent":{"id":7,"uuid":"r3","name":"C"}}],"links":{}}`)
			return
		}
		next := f.server.URL + "/api/conversations?offset=40&limit=40"
		_, _ = fmt.Fprintf(w, `{"data":[
			{"id":"c1","user_id":1,"user_uuid":"me","respondent":{"id":5,"uuid":"r1","name":"A","type":"seller","blocked":false},
			 "archived":false,"is_observed":true,"unread_count":2,
			 "ad":{"id":1101945349,"title":"Phone"},
			 "context":{"id":1101945349,"title":"Phone","image_url":"https://cdn.example/p.jpg","context_data":{"ad_id":"1101945349","price":{"minor_amount":190000,"currency_code":"PLN"}}},
			 "messages":[{"id":"m1","user_id":5,"user_uuid":"r1","created_at":"2026-10-02T09:07:11Z","read_at":null,"text":"hi","attachments":[{"name":"a.jpg","url":"https://cdn.example/a.jpg"}],"sent_from_mobile":true}]},
			{"id":"c2","user_uuid":"me","respondent":{"id":6,"uuid":"r2","name":"B"}}],
			"metadata":{"total_elements":3},"links":{"next":{"href":%q}}}`, next)
	case r.Method == http.MethodGet && r.URL.Path == "/api/conversations/gone":
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/conversations/"):
		_, _ = io.WriteString(w, `{"data":{"id":"c1","user_uuid":"me","respondent":{"uuid":"r1","name":"A"},"messages":[{"id":"m1","user_uuid":"r1","created_at":"2026-10-02T09:07:11.123+02:00","text":"hi"}]}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/apollo/token":
		_, _ = io.WriteString(w, `{"data":{"token":"apollo-token"}}`)
	case r.URL.Path == "/api/v1/users/":
		_, _ = io.WriteString(w, `{"data":[{"id":5,"uuid":"r1","name":"A","is_online":true,"last_seen":"2026-10-02T11:20:00+02:00","user_photo":"https://img.example/u.jpg"}]}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeOLX) handleSocket(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	f.socketProtos = append([]string(nil), r.Header.Values("Sec-WebSocket-Protocol")...)
	f.socketOrigin = r.Header.Get("Origin")
	valid := f.validToken
	f.lock.Unlock()
	authorized := false
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(protocol) == url.QueryEscape("access_token="+valid) {
			authorized = true
		}
	}
	if !authorized {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{url.QueryEscape("X-Client=DESKTOP")},
		OriginPatterns: []string{"www.olx.pl"},
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(data), `"ping"`) {
				f.pings.Add(1)
			}
		}
	}()
	for frame := range f.socketFrames {
		if frame == "close" {
			return
		}
		if err = conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return
		}
	}
}

func (f *fakeOLX) client(tokens Tokens) *Client {
	cfg := Config{
		ChatURL:   f.server.URL,
		WWWURL:    f.server.URL,
		UploadURL: f.server.URL + "/upload",
		SocketURL: "ws" + strings.TrimPrefix(f.server.URL, "http") + "/ws",
		UserAgent: testUA,
		DeviceID:  "device-1",
		Auth:      AuthConfig{Host: f.server.URL},
	}
	return NewClient(cfg, tokens, f.server.Client(), nil, zerolog.Nop())
}

func (f *fakeOLX) lastRequest() (*http.Request, string) {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.requests[len(f.requests)-1], f.bodies[len(f.bodies)-1]
}

func TestRefreshOnDemandAndOn401(t *testing.T) {
	f := newFakeOLX(t)
	var saved []Tokens
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	c.OnTokens = func(tok Tokens) { saved = append(saved, tok) }
	ctx := context.Background()

	if _, err := c.GetCounters(ctx); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if f.refreshes != 1 || len(saved) != 1 || saved[0].RefreshToken != "good-refresh" {
		t.Fatalf("expected one refresh that keeps the refresh token, got %d refreshes, saved %+v", f.refreshes, saved)
	}
	if _, err := c.GetCounters(ctx); err != nil || f.refreshes != 1 {
		t.Fatalf("a valid token must be reused: err=%v refreshes=%d", err, f.refreshes)
	}
	// The server stops accepting the token although it has not expired.
	f.lock.Lock()
	f.validToken = "rotated-elsewhere"
	f.lock.Unlock()
	if _, err := c.GetCounters(ctx); err != nil {
		t.Fatalf("request after the token was rejected: %v", err)
	}
	if f.refreshes != 2 {
		t.Fatalf("a 401 must renew the token once, got %d refreshes", f.refreshes)
	}
}

func TestLoggedOut(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "revoked"})
	_, err := c.GetCounters(context.Background())
	if err == nil || !strings.Contains(err.Error(), ErrLoggedOut.Error()) {
		t.Fatalf("expected ErrLoggedOut, got %v", err)
	}
	if !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("error does not wrap ErrLoggedOut: %v", err)
	}
}

func TestListAndPagination(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	archived, mine := false, true
	convs, err := c.ListAllConversations(context.Background(), ListParams{Archived: &archived, MyAds: &mine}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 3 || convs[0].ID != "c1" || convs[2].ID != "c3" {
		t.Fatalf("expected three conversations over two pages, got %+v", convs)
	}
	f.lock.Lock()
	first := f.requests[0]
	f.lock.Unlock()
	q := first.URL.Query()
	if q.Get("archived") != "0" || q.Get("my_ads") != "1" || q.Get("limit") != "40" {
		t.Errorf("unexpected list query: %s", first.URL.RawQuery)
	}
	for header, want := range map[string]string{
		"X-Api-Version": "2", "X-Site-Code": "olxpl", "X-Client": "DESKTOP",
		"X-Client-Version": DefaultClientVersion, "User-Agent": testUA,
		"Accept": "*/*", "Accept-Language": "pl-PL, pl", "Origin": "https://www.olx.pl", "Referer": "https://www.olx.pl/",
		"Sec-Ch-Ua": `"Chromium";v="140", "Not=A?Brand";v="24", "Google Chrome";v="140"`, "Sec-Ch-Ua-Mobile": "?0",
		"Sec-Ch-Ua-Platform": `"Linux"`, "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
	} {
		if got := first.Header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	conv := convs[0]
	if conv.Respondent.UUID != "r1" || conv.Respondent.ID != "5" || !conv.IsObserved || conv.UnreadCount != 2 {
		t.Errorf("conversation parsed wrong: %+v", conv)
	}
	if conv.Ad.ID != "1101945349" || conv.Context.Data.AdID != "1101945349" {
		t.Errorf("ad IDs must parse from numbers and strings alike: %+v %+v", conv.Ad, conv.Context.Data)
	}
	if got := conv.Context.Data.Price.String(); got != "1 900 zł" {
		t.Errorf("price = %q", got)
	}
	msg := conv.LatestMessage()
	if msg == nil || msg.ID != "m1" || msg.IsRead() || len(msg.Attachments) != 1 || msg.Attachments[0].URL == "" {
		t.Errorf("message parsed wrong: %+v", msg)
	}
	if !msg.CreatedAt.Equal(time.Date(2026, 10, 2, 9, 7, 11, 0, time.UTC)) {
		t.Errorf("created_at = %v", msg.CreatedAt)
	}
}

func TestNextPageMustStayOnChatHost(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	if _, _, err := c.ListConversationsPage(context.Background(), "https://evil.example/api/conversations"); err == nil {
		t.Fatal("a next-page link to another host must not be followed with the session token")
	}
}

func TestGetConversation(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	conv, err := c.GetConversation(context.Background(), "c1")
	if err != nil || conv == nil || len(conv.Messages) != 1 {
		t.Fatalf("conv=%+v err=%v", conv, err)
	}
	if conv.Messages[0].CreatedAt.UTC().Hour() != 7 {
		t.Errorf("timestamp with offset parsed wrong: %v", conv.Messages[0].CreatedAt)
	}
	gone, err := c.GetConversation(context.Background(), "gone")
	if err != nil || gone != nil {
		t.Fatalf("a missing conversation is nil without an error, got %+v %v", gone, err)
	}
}

func TestSendAndActions(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	ctx := context.Background()

	msg := &OutgoingMessage{Text: "hello", Attachments: []OutgoingAttachment{{Filename: "a.jpg", FileID: "f1"}}}
	if _, err := c.SendMessage(ctx, "c1", msg); err != nil {
		t.Fatal(err)
	}
	req, body := f.lastRequest()
	if req.Method != http.MethodPost || req.URL.Path != "/api/conversations/c1/messages" {
		t.Errorf("send went to %s %s", req.Method, req.URL.Path)
	}
	var sent struct {
		Message struct {
			Text        string `json:"text"`
			MessageID   string `json:"message_id"`
			Attachments []struct {
				Filename string `json:"filename"`
				FileID   string `json:"file_id"`
			} `json:"attachments"`
		} `json:"message"`
		AdID *int64 `json:"ad_id"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Message.Text != "hello" || sent.Message.MessageID == "" || sent.Message.MessageID != msg.MessageID ||
		len(sent.Message.Attachments) != 1 || sent.Message.Attachments[0].FileID != "f1" || sent.AdID != nil {
		t.Errorf("unexpected send body: %s", body)
	}

	if _, err := c.StartConversation(ctx, 1101945349, &OutgoingMessage{Text: "is it available?"}); err != nil {
		t.Fatal(err)
	}
	req, body = f.lastRequest()
	if req.URL.Path != "/api/conversations" || !strings.Contains(body, `"ad_id":1101945349`) {
		t.Errorf("start conversation: %s %s", req.URL.Path, body)
	}

	checks := []struct {
		name   string
		call   func() error
		method string
		path   string
		body   string
	}{
		{"read", func() error { return c.MarkRead(ctx, "c1") }, "POST", "/api/conversations/c1/read", `"event_uuid"`},
		{"typing on", func() error { return c.SendTyping(ctx, "c1", true) }, "POST", "/api/typing", `"type":"typing_started"`},
		{"typing off", func() error { return c.SendTyping(ctx, "c1", false) }, "POST", "/api/typing", `"type":"typing_stopped"`},
		{"save", func() error { return c.SetSaved(ctx, "c1", true) }, "POST", "/api/conversations/saved", `"conversation_id":"c1"`},
		{"unsave", func() error { return c.SetSaved(ctx, "c1", false) }, "DELETE", "/api/conversations/saved/c1", ``},
		{"trash", func() error { return c.SetTrashed(ctx, "c1", true) }, "POST", "/api/conversations/trash", `"conversation_id":"c1"`},
		{"untrash", func() error { return c.SetTrashed(ctx, "c1", false) }, "DELETE", "/api/conversations/trash/c1", `"event_uuid"`},
		{"delete", func() error { return c.DeleteConversation(ctx, "c1") }, "DELETE", "/api/conversations/c1", `"event_uuid"`},
		{"block", func() error { return c.SetBlocked(ctx, "r1", true) }, "POST", "/api/respondents/blocked", `"respondent_uuid":"r1"`},
		{"unblock", func() error { return c.SetBlocked(ctx, "r1", false) }, "DELETE", "/api/respondents/blocked/r1", ``},
	}
	for _, check := range checks {
		if err := check.call(); err != nil {
			t.Errorf("%s: %v", check.name, err)
			continue
		}
		req, body = f.lastRequest()
		if req.Method != check.method || req.URL.Path != check.path || !strings.Contains(body, check.body) {
			t.Errorf("%s: got %s %s %s", check.name, req.Method, req.URL.Path, body)
		}
	}
}

func TestUploadAndUsers(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	ctx := context.Background()
	att, err := c.Upload(ctx, []byte("jpegdata"), "photo.jpg", "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if att.FileID != "stored-file-id" || att.Filename != "photo.jpg" {
		t.Errorf("attachment = %+v", att)
	}
	req, body := f.lastRequest()
	if req.Header.Get("Sec-Fetch-Site") != "cross-site" || req.Header.Get("Origin") != "https://www.olx.pl" {
		t.Errorf("the upload goes to another site: %v", req.Header)
	}
	if req.URL.Path != "/upload" || body != "jpegdata" || req.Header.Get("Content-Type") != "image/jpeg" || req.Header.Get("Expires") == "" {
		t.Errorf("upload request wrong: %s %q %v", req.URL.Path, body, req.Header)
	}

	users, err := c.GetUsers(ctx, []string{"r1", "r2"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ = f.lastRequest()
	if req.URL.Query().Get("user_uuids[0]") != "r1" || req.URL.Query().Get("user_uuids[1]") != "r2" {
		t.Errorf("users query = %s", req.URL.RawQuery)
	}
	if req.Header.Get("Version") != "v1.19" || req.Header.Get("X-Platform-Type") != "mobile-html5" {
		t.Errorf("www requests name the API version and platform the website does: %v", req.Header)
	}
	if req.Header.Get("X-Device-Id") != "device-1" || req.Header.Get("X-Site-Code") != "" {
		t.Errorf("www requests carry the device ID and no chat headers: %v", req.Header)
	}
	if req.Header.Get("Sec-Fetch-Site") != "same-origin" || req.Header.Get("Origin") != "" ||
		req.Header.Get("Referer") != "https://www.olx.pl/myaccount/answers/" || req.Header.Get("Accept-Language") != "pl" {
		t.Errorf("a GET to www.olx.pl is same-origin: full referrer, no Origin: %v", req.Header)
	}
	if len(users) != 1 || !users[0].IsOnline || users[0].LastSeen.IsZero() || users[0].UserPhoto == "" {
		t.Errorf("users = %+v", users[0])
	}
}

type recordingHandler struct {
	events    chan *Event
	connected chan bool
	down      chan error
}

func (h *recordingHandler) HandleEvent(ctx context.Context, evt *Event) { h.events <- evt }
func (h *recordingHandler) HandleConnected(ctx context.Context, reconnect bool) {
	h.connected <- reconnect
}
func (h *recordingHandler) HandleDisconnected(ctx context.Context, err error, fatal bool) {
	select {
	case h.down <- err:
	default:
	}
}

func TestSocket(t *testing.T) {
	f := newFakeOLX(t)
	c := f.client(Tokens{RefreshToken: "good-refresh"})
	h := &recordingHandler{events: make(chan *Event, 4), connected: make(chan bool, 4), down: make(chan error, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.RunSocket(ctx, h)

	select {
	case reconnect := <-h.connected:
		if reconnect {
			t.Fatal("the first connection is not a reconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("socket did not connect")
	}
	f.lock.Lock()
	protos := strings.Join(f.socketProtos, ", ")
	token := f.validToken
	origin := f.socketOrigin
	f.lock.Unlock()
	if origin != testOrigin {
		t.Errorf("the socket handshake names the web app as its origin, got %q", origin)
	}
	for _, want := range []string{"X-Client%3DDESKTOP", "X-Client-Version%3D" + DefaultClientVersion, "access_token%3D" + url.QueryEscape(token)} {
		if !strings.Contains(protos, want) {
			t.Errorf("socket subprotocols %q lack %q", protos, want)
		}
	}

	f.socketFrames <- `{"pong":true}`
	f.socketFrames <- `{"type":"new_message","data":{"conversation_id":"c1","unread_counter":1,"message":{"id":"m9","user_uuid":"r1","created_at":"2026-10-02T10:00:00Z","text":"yo"}}}`
	select {
	case evt := <-h.events:
		var data EventData
		if err := json.Unmarshal(evt.Data, &data); err != nil {
			t.Fatal(err)
		}
		if evt.Type != EventNewMessage || data.ConversationID != "c1" || data.Message == nil || data.Message.Text != "yo" || *data.UnreadCounter != 1 {
			t.Errorf("event = %s %+v", evt.Type, data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event (frames without a type must be skipped, not block)")
	}

	f.socketFrames <- "close"
	select {
	case <-h.down:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect was not reported")
	}
	select {
	case reconnect := <-h.connected:
		if !reconnect {
			t.Fatal("the second connection is a reconnect")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("socket did not reconnect")
	}
}

func TestAuthCodeFlow(t *testing.T) {
	f := newFakeOLX(t)
	auth := AuthConfig{Host: f.server.URL}
	pkce := NewPKCE()
	authorize, err := url.Parse(auth.AuthorizeURL(pkce))
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	if authorize.Path != "/oauth2/authorize" || q.Get("client_id") != MustSite("pl").ClientID || q.Get("redirect_uri") != testRedirect ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != pkce.Challenge || q.Get("state") != pkce.State {
		t.Errorf("authorize URL wrong: %s", authorize)
	}
	if strings.Contains(authorize.String(), pkce.Verifier) {
		t.Error("the verifier must never be in the authorize URL")
	}

	callback := testRedirect + "?code=the-code&state=" + pkce.State
	code, err := ParseCallback(callback, pkce)
	if err != nil || code != "the-code" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if code, err = ParseCallback("view-source:"+callback, pkce); err != nil || code != "the-code" {
		t.Errorf("the address as the source view shows it must be accepted: %q %v", code, err)
	}
	if code, err = ParseCallback("  the-code \n", pkce); err != nil || code != "the-code" {
		t.Errorf("a bare code must be accepted: %q %v", code, err)
	}
	if _, err = ParseCallback(testRedirect+"?code=x&state=other", pkce); err == nil {
		t.Error("a callback from another attempt must be refused")
	}
	if _, err = ParseCallback(testRedirect+"?error=access_denied", pkce); err == nil {
		t.Error("an error callback must be reported")
	}
	if _, err = ParseCallback(testRedirect, pkce); err == nil {
		t.Error("a callback without a code must be refused")
	}

	tokens, err := auth.ExchangeCode(context.Background(), f.server.Client(), "the-code", pkce)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken != "good-refresh" || tokens.IDToken == "" || time.Until(tokens.Expiry) < 10*time.Minute {
		t.Errorf("tokens = %+v", tokens)
	}
	claims, err := ParseClaims(tokens.IDToken)
	if err != nil || claims.Subject != "sub-1" || claims.Email != "user@example.com" {
		t.Errorf("claims = %+v err=%v", claims, err)
	}
	if _, err = auth.ExchangeCode(context.Background(), f.server.Client(), "wrong", pkce); err == nil {
		t.Error("a wrong code must fail")
	}
}

func TestFlexibleTypes(t *testing.T) {
	var id FlexID
	for input, want := range map[string]FlexID{`123`: "123", `"456"`: "456", `null`: ""} {
		if err := json.Unmarshal([]byte(input), &id); err != nil || id != want {
			t.Errorf("FlexID(%s) = %q, %v", input, id, err)
		}
	}
	var ts Time
	for _, input := range []string{`"2026-10-02T09:07:11Z"`, `"2026-10-02T11:07:11+02:00"`, `"2026-10-02T11:07:11.000+0200"`, `1790932031`, `1790932031000`} {
		if err := json.Unmarshal([]byte(input), &ts); err != nil {
			t.Errorf("Time(%s): %v", input, err)
		} else if ts.Unix() != 1790932031 {
			t.Errorf("Time(%s) = %d", input, ts.Unix())
		}
	}
	if err := json.Unmarshal([]byte(`null`), &ts); err != nil || !ts.IsZero() {
		t.Errorf("null time: %v %v", ts, err)
	}
	cents := func(n int64) *int64 { return &n }
	for want, price := range map[string]*Price{
		"1 900 zł":    {MinorAmount: cents(190000), CurrencyCode: "PLN"},
		"12,50 zł":    {MinorAmount: cents(1250), CurrencyCode: "PLN"},
		"1 234 567 €": {MinorAmount: cents(123456700), CurrencyCode: "€"},
		"":            nil,
	} {
		if got := price.String(); got != want {
			t.Errorf("price = %q, want %q", got, want)
		}
	}
}

func TestBrowserHeaders(t *testing.T) {
	// Sec-CH-UA as real Chrome releases send it.
	for major, want := range map[int]string{
		131: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		140: `"Chromium";v="140", "Not=A?Brand";v="24", "Google Chrome";v="140"`,
	} {
		if got := secCHUA(major); got != want {
			t.Errorf("secCHUA(%d) = %s, want %s", major, got, want)
		}
	}
	if major, ok := chromeMajor(ChromeUserAgent(DefaultChromeMajor)); !ok || major != DefaultChromeMajor {
		t.Errorf("the default User-Agent must name Chrome %d, got %d %v", DefaultChromeMajor, major, ok)
	}
	for _, ua := range []string{"mautrix-olx/26.10", "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"} {
		header := http.Header{}
		setBrowserHeaders(header, ua, testOrigin, siteSameSite, http.MethodGet)
		if header.Get("User-Agent") != ua || len(header) != 1 {
			t.Errorf("a User-Agent that is not Chrome's gets no Chrome headers: %v", header)
		}
	}
	header := http.Header{}
	setBrowserHeaders(header, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36", "https://www.olx.ua", siteSameOrigin, http.MethodPost)
	if header.Get("Sec-Ch-Ua-Platform") != `"macOS"` || header.Get("Origin") != "https://www.olx.ua" {
		t.Errorf("platform follows the User-Agent, and a same-origin POST names its origin: %v", header)
	}
}

func TestSites(t *testing.T) {
	seenClient := map[string]string{}
	for _, site := range Sites {
		if other, dup := seenClient[site.ClientID]; dup {
			t.Errorf("%s and %s share a client ID", site.Code, other)
		}
		seenClient[site.ClientID] = site.Code
		if site.Domain != "olx."+site.Code || site.SiteCode != "olx"+site.Code || site.Language == "" || site.WWWLanguage == "" {
			t.Errorf("site %s is inconsistent: %+v", site.Code, site)
		}
		for _, name := range []string{site.Code, site.Domain, site.SiteCode, "https://www." + site.Domain + "/", " " + strings.ToUpper(site.Code) + " "} {
			if got, err := LookupSite(name); err != nil || got.Code != site.Code {
				t.Errorf("LookupSite(%q) = %v, %v", name, got.Code, err)
			}
		}
	}
	if site, err := LookupSite(""); err != nil || site.Code != DefaultSite {
		t.Errorf("no site means the default one, got %v %v", site.Code, err)
	}
	if _, err := LookupSite("olx.com.br"); err == nil {
		t.Error("an OLX on another platform must be refused")
	}

	cfg := Config{Site: MustSite("ua")}
	cfg.setDefaults()
	if cfg.ChatURL != "https://api.chat.olx.ua" || cfg.SocketURL != "wss://ws.chat.olx.ua" || cfg.WWWURL != "https://www.olx.ua" ||
		cfg.SiteCode != "olxua" || cfg.Language != "uk-UA, uk" || cfg.WWWLanguage != "uk" ||
		cfg.Auth.Host != "https://login.olx.ua" || cfg.Auth.ClientID != "309lsgh0deirlo2la9kmrmhe3v" ||
		cfg.Auth.RedirectURI != "https://www.olx.ua/d/callback/" {
		t.Errorf("a site's configuration follows from its domain: %+v", cfg)
	}
	authorize := AuthConfig{Site: MustSite("ro")}.AuthorizeURL(NewPKCE())
	if !strings.HasPrefix(authorize, "https://login.olx.ro/oauth2/authorize?") || !strings.Contains(authorize, "client_id=7gantjdsv7233vniq4dthhm2hh") ||
		!strings.Contains(authorize, url.QueryEscape("https://www.olx.ro/d/callback/")) {
		t.Errorf("authorize URL for olx.ro: %s", authorize)
	}
}

// OLX's gateway only finds the token under the exact header name browsers use.
// A Go HTTP server cannot tell (it normalizes names as it reads them), so this
// looks at the bytes on the wire.
func TestSocketHandshakeOnTheWire(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 16384)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _ := conn.Read(buf)
		got <- string(buf[:n])
	}()
	c := NewClient(Config{SocketURL: "ws://" + ln.Addr().String()},
		Tokens{IDToken: "TOKEN.abc-def_ghi", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}, nil, nil, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() { _, _ = c.dialSocket(ctx) }()
	var raw string
	select {
	case raw = <-got:
	case <-ctx.Done():
		t.Fatal("no handshake arrived")
	}
	for _, want := range []string{
		"GET / HTTP/1.1\r\n",
		"\r\nSec-WebSocket-Protocol: X-Client%3DDESKTOP, X-Client-Version%3D" + DefaultClientVersion + ", access_token%3DTOKEN.abc-def_ghi\r\n",
		"\r\nSec-WebSocket-Key: ",
		"\r\nSec-WebSocket-Version: 13\r\n",
		"\r\nOrigin: https://www.olx.pl\r\n",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("handshake lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "Sec-Websocket-") {
		t.Errorf("handshake headers must not be in Go's canonical spelling:\n%s", raw)
	}
}
