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
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	// socketPingInterval is how often the application-level ping is sent. The
	// website sends one after five minutes of silence; the bridge pings more
	// often so that a dead connection is noticed sooner.
	socketPingInterval = 2 * time.Minute
	// socketReadTimeout is how long the socket may stay silent before it is
	// taken for dead: two missed pings.
	socketReadTimeout  = 2*socketPingInterval + 30*time.Second
	socketMinBackoff   = 2 * time.Second
	socketMaxBackoff   = 2 * time.Minute
	socketStableAfter  = time.Minute
	socketDialTimeout  = 30 * time.Second
	socketMaxFrameSize = 8 << 20
)

// SocketHandler receives what happens on the chat WebSocket.
type SocketHandler interface {
	// HandleEvent is called for every event, in order, on one goroutine.
	HandleEvent(ctx context.Context, evt *Event)
	// HandleConnected is called every time the socket has (re)connected.
	// Events sent while it was down are lost, so this is the time to resync.
	HandleConnected(ctx context.Context, reconnect bool)
	// HandleDisconnected is called when the socket went down; the client keeps
	// trying to reconnect unless fatal is set, which means the session is gone.
	HandleDisconnected(ctx context.Context, err error, fatal bool)
}

// The website passes its client name, version and token as WebSocket
// subprotocols, each URI-encoded.
func (c *Client) socketProtocols(token string) []string {
	return []string{
		url.QueryEscape("X-Client=" + c.cfg.Platform),
		url.QueryEscape("X-Client-Version=" + c.cfg.ClientVersion),
		url.QueryEscape("access_token=" + token),
	}
}

// browserProtocolList writes the WebSocket subprotocol list the way browsers
// do, "a, b, c". The WebSocket library writes "a,b,c", which is just as valid,
// but OLX's gateway takes the list apart at ", " and then does not find the
// token in it: the same handshake is answered 403 instead of 101.
type browserProtocolList struct {
	base http.RoundTripper
}

func (b browserProtocolList) RoundTrip(req *http.Request) (*http.Response, error) {
	if protocols := req.Header.Values("Sec-WebSocket-Protocol"); len(protocols) > 0 {
		var all []string
		for _, value := range protocols {
			for _, protocol := range strings.Split(value, ",") {
				if protocol = strings.TrimSpace(protocol); protocol != "" {
					all = append(all, protocol)
				}
			}
		}
		req = req.Clone(req.Context())
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(all, ", "))
	}
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func (c *Client) dialSocket(ctx context.Context) (*websocket.Conn, error) {
	token, err := c.IDToken(ctx)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, socketDialTimeout)
	defer cancel()
	// A browser's WebSocket handshake carries the page's origin and no fetch
	// metadata.
	header := http.Header{}
	header.Set("User-Agent", c.cfg.UserAgent)
	if _, isChrome := chromeMajor(c.cfg.UserAgent); isChrome {
		header.Set("Origin", c.cfg.Site.Origin())
		header.Set("Accept-Language", c.cfg.Language)
		header.Set("Cache-Control", "no-cache")
		header.Set("Pragma", "no-cache")
	}
	conn, _, err := websocket.Dial(dialCtx, c.cfg.SocketURL, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: browserProtocolList{c.HTTP.Transport}},
		HTTPHeader:   header,
		Subprotocols: c.socketProtocols(token),
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(socketMaxFrameSize)
	return conn, nil
}

func (c *Client) readSocket(ctx context.Context, conn *websocket.Conn, handler SocketHandler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(socketPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeCtx, writeCancel := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, []byte(`{"action":"ping"}`))
				writeCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		readCtx, readCancel := context.WithTimeout(ctx, socketReadTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			return err
		}
		var evt Event
		if err = json.Unmarshal(data, &evt); err != nil {
			c.log.Debug().Err(err).Int("frame_size", len(data)).Msg("Ignoring unparseable socket frame")
			continue
		}
		if evt.Type == "" {
			// Ping replies and other frames without a type.
			continue
		}
		handler.HandleEvent(ctx, &evt)
	}
}

// RunSocket keeps the chat WebSocket connected until ctx is done or the
// session turns out to be gone, reconnecting with backoff in between.
func (c *Client) RunSocket(ctx context.Context, handler SocketHandler) {
	backoff := socketMinBackoff
	connectedBefore := false
	for ctx.Err() == nil {
		conn, err := c.dialSocket(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fatal := errors.Is(err, ErrLoggedOut)
			handler.HandleDisconnected(ctx, err, fatal)
			if fatal {
				return
			}
		} else {
			connectedAt := time.Now()
			handler.HandleConnected(ctx, connectedBefore)
			connectedBefore = true
			err = c.readSocket(ctx, conn, handler)
			_ = conn.CloseNow()
			if ctx.Err() != nil {
				return
			}
			if time.Since(connectedAt) > socketStableAfter {
				backoff = socketMinBackoff
			}
			handler.HandleDisconnected(ctx, err, false)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, socketMaxBackoff)
	}
}
