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

package connector

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/MaximilianGaedig/matrix-olx/pkg/olxapi"
)

// What OLX's web app is told about itself changes with its releases, which
// come several times a week. The bridge reads it from each site it is used
// with instead of carrying the values of the day it was built.

const (
	// webConfigInterval is how often a site is asked again, webConfigRetry how
	// soon after a failure.
	webConfigInterval = 6 * time.Hour
	webConfigRetry    = 15 * time.Minute
	webConfigTimeout  = 30 * time.Second
)

type siteState struct {
	live  atomic.Pointer[olxapi.WebConfig]
	drift string
}

type siteStates struct {
	lock  sync.Mutex
	sites map[string]*siteState
}

// liveConfig returns the function a client asks for the site's current
// configuration, and starts keeping that configuration current.
func (oc *OLXConnector) liveConfig(site olxapi.Site) func() *olxapi.WebConfig {
	oc.sites.lock.Lock()
	defer oc.sites.lock.Unlock()
	state, ok := oc.sites.sites[site.Code]
	if !ok {
		state = &siteState{}
		if oc.sites.sites == nil {
			oc.sites.sites = make(map[string]*siteState)
		}
		oc.sites.sites[site.Code] = state
		if oc.Bridge != nil && oc.Bridge.BackgroundCtx != nil && oc.wwwClient != nil {
			go oc.watchSite(oc.Bridge.BackgroundCtx, site, state)
		}
	}
	return state.live.Load
}

func (oc *OLXConnector) watchSite(ctx context.Context, site olxapi.Site, state *siteState) {
	log := oc.Bridge.Log.With().Str("component", "web config").Str("site", site.Code).Logger()
	for {
		wait := webConfigInterval
		if !oc.refreshSite(ctx, site, state, log) {
			wait = webConfigRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// refreshSite reads the site's configuration once and reports whether it could.
func (oc *OLXConnector) refreshSite(ctx context.Context, site olxapi.Site, state *siteState, log zerolog.Logger) bool {
	ctx, cancel := context.WithTimeout(ctx, webConfigTimeout)
	defer cancel()
	live, err := olxapi.FetchWebConfig(ctx, oc.wwwClient, site.Origin(), oc.userAgent(), site.Language)
	if err != nil {
		// The bridge works without it, on what it was built with.
		log.Warn().Err(err).Msg("Failed to read the site's web app configuration")
		return false
	}
	previous := state.live.Swap(live)
	if previous == nil || previous.Version != live.Version || previous.APIVersion != live.APIVersion {
		log.Info().Str("web_version", live.Version).Str("api_version", live.APIVersion).Msg("Read the site's web app release")
	}
	// A moved login or API is not followed, only said, and said once.
	if drift := strings.Join(site.Drift(live), "; "); drift != state.drift {
		state.drift = drift
		if drift != "" {
			log.Warn().Str("changes", drift).Msg("OLX changed something the bridge has built in; the bridge may need an update")
		}
	}
	return true
}
