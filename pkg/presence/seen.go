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

package presence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// SeenFunc tells the homeserver when the remote network held a user as last
// active.
type SeenFunc func(ctx context.Context, remoteUserID string, at time.Time) error

// ErrSeenUnsupported is returned by a SeenFunc when the homeserver has no
// place for last-active times; the reporter then stops asking.
var ErrSeenUnsupported = errors.New("homeserver doesn't take last-active times")

// SeenReporter passes on when the remote network says a user was last active.
//
// Matrix presence has "online" and "offline" and nothing for "was here at
// 14:05", which is what a network tells a bridge about someone who has come
// and gone. A homeserver that keeps an activity log (tuwunel's
// im.mxg.activity) takes that time on its own endpoint. Each time is sent
// once, in order, on one goroutine.
type SeenReporter struct {
	send SeenFunc

	lock sync.Mutex
	last map[string]time.Time

	queue    chan seenItem
	disabled atomic.Bool
}

type seenItem struct {
	remoteUserID string
	at           time.Time
}

func NewSeenReporter(send SeenFunc) *SeenReporter {
	return &SeenReporter{
		send:  send,
		last:  make(map[string]time.Time),
		queue: make(chan seenItem, 4096),
	}
}

// Note queues a last-active time, unless it is no news: the network repeats
// the same time in every snapshot until the user is active again.
func (r *SeenReporter) Note(remoteUserID string, at time.Time) {
	if r == nil || r.disabled.Load() || !r.isNews(remoteUserID, at, time.Now()) {
		return
	}
	select {
	case r.queue <- seenItem{remoteUserID, at}:
	default:
		// Full: the homeserver is not keeping up. Forget that it was noted, so a later snapshot
		// brings it again.
		r.lock.Lock()
		delete(r.last, remoteUserID)
		r.lock.Unlock()
	}
}

// isNews reports whether at is a later last-active time than the one already
// noted for the user, and remembers it if so.
func (r *SeenReporter) isNews(remoteUserID string, at, now time.Time) bool {
	if remoteUserID == "" || at.IsZero() || at.After(now.Add(time.Minute)) {
		return false
	}
	at = at.Truncate(time.Second)
	r.lock.Lock()
	defer r.lock.Unlock()
	if prev, ok := r.last[remoteUserID]; ok && !at.After(prev) {
		return false
	}
	r.last[remoteUserID] = at
	return true
}

// Run sends queued times until ctx is done, or the homeserver says it has no
// place for them.
func (r *SeenReporter) Run(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-r.queue:
			err := r.send(ctx, item.remoteUserID, item.at)
			if errors.Is(err, ErrSeenUnsupported) {
				log.Info().Msg("Homeserver keeps no activity log, not reporting last-active times")
				r.disabled.Store(true)
				return
			} else if err != nil && ctx.Err() == nil {
				log.Warn().Err(err).Str("remote_user_id", item.remoteUserID).Msg("Failed to report last-active time")
			}
		}
	}
}

// GhostSeenSender reports last-active times as the user's ghost. Like
// GhostSender it only looks ghosts up, never creates them.
func GhostSeenSender(br *bridgev2.Bridge) SeenFunc {
	return func(ctx context.Context, remoteUserID string, at time.Time) error {
		ghost, err := br.GetExistingGhostByID(ctx, networkid.UserID(remoteUserID))
		if err != nil {
			return fmt.Errorf("failed to get ghost: %w", err)
		} else if ghost == nil || ghost.Intent == nil {
			return nil
		}
		as, ok := ghost.Intent.(*matrix.ASIntent)
		if !ok || as.Matrix == nil {
			return fmt.Errorf("ghost intent is %T, not an appservice intent", ghost.Intent)
		}
		if err = as.Matrix.EnsureRegistered(ctx); err != nil {
			return fmt.Errorf("failed to ensure ghost is registered: %w", err)
		}
		url := as.Matrix.BuildClientURL("unstable", "im.mxg.activity", "users", as.Matrix.UserID, "seen")
		_, err = as.Matrix.MakeRequest(ctx, http.MethodPut, url, map[string]any{"ts": at.UnixMilli()}, nil)
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil &&
			(httpErr.Response.StatusCode == http.StatusNotFound || httpErr.Response.StatusCode == http.StatusMethodNotAllowed) {
			return ErrSeenUnsupported
		}
		return err
	}
}
