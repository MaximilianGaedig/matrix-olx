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
	"fmt"
	"strings"
)

// Site is one of OLX's country sites. They all run the same web app and the
// same chat, each on its own domain with its own user pool, so an account
// belongs to exactly one of them.
type Site struct {
	// Code is the short name the bridge knows the site by: its top-level domain.
	Code string
	// Domain is the site's registrable domain, e.g. "olx.pl".
	Domain string
	// SiteCode is what the web app calls itself in X-Site-Code.
	SiteCode string
	// ClientID is the site's web client in its Cognito user pool.
	ClientID string
	// Language is the Accept-Language the web app sends to the chat API
	// ("<locale>, <language>"), WWWLanguage the one it sends to its own API.
	Language    string
	WWWLanguage string
}

// Name is how the site calls itself.
func (s Site) Name() string { return "OLX." + s.Code }

// Origin is the origin the site's web app runs on.
func (s Site) Origin() string { return "https://www." + s.Domain }

func (s Site) chatURL() string     { return "https://api.chat." + s.Domain }
func (s Site) socketURL() string   { return "wss://ws.chat." + s.Domain }
func (s Site) authHost() string    { return "https://login." + s.Domain }
func (s Site) redirectURI() string { return s.Origin() + "/d/callback/" }

// DefaultSite is the site a login without a site belongs to.
const DefaultSite = "pl"

// Sites are the OLX sites on this platform, in the order they are offered.
// Client IDs are public: the login page of each site carries its own.
var Sites = []Site{
	{Code: "pl", Domain: "olx.pl", SiteCode: "olxpl", ClientID: "6j7elk01p32o648o1io8lvhhab", Language: "pl-PL, pl", WWWLanguage: "pl"},
	{Code: "ua", Domain: "olx.ua", SiteCode: "olxua", ClientID: "309lsgh0deirlo2la9kmrmhe3v", Language: "uk-UA, uk", WWWLanguage: "uk"},
	{Code: "ro", Domain: "olx.ro", SiteCode: "olxro", ClientID: "7gantjdsv7233vniq4dthhm2hh", Language: "ro-RO, ro", WWWLanguage: "ro"},
	{Code: "bg", Domain: "olx.bg", SiteCode: "olxbg", ClientID: "6hp5n9683mr20hp0rh7gianre1", Language: "bg-BG, bg", WWWLanguage: "bg"},
	{Code: "pt", Domain: "olx.pt", SiteCode: "olxpt", ClientID: "3q2gr1va98i7rvd0l3kef2b6qg", Language: "pt-PT, pt", WWWLanguage: "pt"},
	{Code: "kz", Domain: "olx.kz", SiteCode: "olxkz", ClientID: "7jl3ccvll42i5dp6gb1jj98hpg", Language: "ru-KZ, ru", WWWLanguage: "ru"},
	{Code: "uz", Domain: "olx.uz", SiteCode: "olxuz", ClientID: "4b7edpvrarh6co2rp6lhae0jva", Language: "ru-UZ, ru", WWWLanguage: "ru"},
}

// LookupSite finds a site by its code ("pl"), domain ("olx.pl") or site code
// ("olxpl"). An empty name means the default site.
func LookupSite(name string) (Site, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "www.")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		name = DefaultSite
	}
	for _, site := range Sites {
		if name == site.Code || name == site.Domain || name == site.SiteCode {
			return site, nil
		}
	}
	codes := make([]string, len(Sites))
	for i, site := range Sites {
		codes[i] = site.Code
	}
	return Site{}, fmt.Errorf("unknown OLX site %q (known: %s)", name, strings.Join(codes, ", "))
}

// MustSite is LookupSite for names that are known to be valid.
func MustSite(name string) Site {
	site, err := LookupSite(name)
	if err != nil {
		panic(err)
	}
	return site
}
