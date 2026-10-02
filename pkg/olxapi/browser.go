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

package olxapi

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/bogdanfinn/tls-client/profiles"
)

// The bridge talks to OLX the way OLX's website does when it runs in Chrome,
// header for header: the User-Agent, the client hints derived from it, and
// the fetch metadata a browser adds to requests a page makes.

// BrowserProfile is the Chrome TLS+HTTP/2 fingerprint the bridge presents on
// www.olx.pl (used by the connector's browser transport): the newest desktop
// Chrome tls-client ships. The library tracks Chrome for us, so a `go get -u`
// on tls-client advances both the fingerprint and, through DefaultChromeMajor
// below, the User-Agent that must match it - with no version to hand-maintain.
var BrowserProfile = latestChromeProfile()

// latestChromeProfile returns the newest desktop Chrome profile tls-client ships
// (newer than its DefaultClientProfile, which trails the newest by design) so we
// stay on current Chrome without picking a version. Falls back to the default.
func latestChromeProfile() profiles.ClientProfile {
	best := profiles.DefaultClientProfile
	bestMajor := profileChromeMajor(best)
	for _, p := range profiles.MappedTLSClients {
		if p.GetClientHelloId().Client != "Chrome" {
			continue
		}
		if major := profileChromeMajor(p); major > bestMajor {
			best, bestMajor = p, major
		}
	}
	return best
}

// DefaultChromeMajor is the Chrome major version the User-Agent and the client
// hints name. It is read from BrowserProfile so the User-Agent can never drift
// from the TLS fingerprint and is never a hand-maintained number. Set user_agent
// in the config to override everything.
var DefaultChromeMajor = profileChromeMajor(BrowserProfile)

// profileChromeMajor reads the Chrome major version a tls-client profile mimics
// from its ClientHelloID version string (e.g. "150" -> 150).
func profileChromeMajor(p profiles.ClientProfile) int {
	version := p.GetClientHelloId().Version
	if i := strings.IndexByte(version, '.'); i >= 0 {
		version = version[:i]
	}
	if major, err := strconv.Atoi(version); err == nil && major > 0 {
		return major
	}
	return 131 // unreachable safety net: real Chrome profiles carry a version
}

// ChromeUserAgent is what desktop Chrome on Linux sends. Chrome freezes
// everything but the major version.
func ChromeUserAgent(major int) string {
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", major)
}

var chromeVersion = regexp.MustCompile(`Chrome/(\d+)\.`)

// chromeMajor returns the Chrome major version a User-Agent names. Edge, Opera
// and other Chromium browsers add their own brand and are not handled.
func chromeMajor(userAgent string) (int, bool) {
	if strings.Contains(userAgent, "Edg/") || strings.Contains(userAgent, "OPR/") {
		return 0, false
	}
	match := chromeVersion.FindStringSubmatch(userAgent)
	if match == nil {
		return 0, false
	}
	major, err := strconv.Atoi(match[1])
	return major, err == nil && major > 0
}

// secCHUA builds the Sec-CH-UA header exactly as Chrome does. The list has a
// made-up "GREASE" brand whose name, version and position all follow from the
// major version (Chromium's GetGreasedUserAgentBrandVersion).
func secCHUA(major int) string {
	greaseChars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVersions := []string{"8", "99", "24"}
	orders := [6][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	version := strconv.Itoa(major)
	brand := func(name, ver string) string {
		return fmt.Sprintf("%q;v=%q", name, ver)
	}
	grease := brand("Not"+greaseChars[major%len(greaseChars)]+"A"+greaseChars[(major+1)%len(greaseChars)]+"Brand", greaseVersions[major%len(greaseVersions)])
	order := orders[major%len(orders)]
	var list [3]string
	list[order[0]] = grease
	list[order[1]] = brand("Chromium", version)
	list[order[2]] = brand("Google Chrome", version)
	return strings.Join(list[:], ", ")
}

func uaPlatform(userAgent string) string {
	switch {
	case strings.Contains(userAgent, "Windows"):
		return `"Windows"`
	case strings.Contains(userAgent, "Macintosh"):
		return `"macOS"`
	case strings.Contains(userAgent, "Android"):
		return `"Android"`
	case strings.Contains(userAgent, "CrOS"):
		return `"Chrome OS"`
	default:
		return `"Linux"`
	}
}

// fetchSite is how the request's target relates to the page making it.
type fetchSite string

const (
	// siteSameOrigin: www.olx.pl itself.
	siteSameOrigin fetchSite = "same-origin"
	// siteSameSite: another host under olx.pl (the chat API, the login host).
	siteSameSite fetchSite = "same-site"
	// siteCrossSite: anything else (the file store on olxcdn.com).
	siteCrossSite fetchSite = "cross-site"
)

// chatPagePath is the page of the web app that the chat lives on.
const chatPagePath = "/myaccount/answers/"

// setBrowserHeaders adds what Chrome adds to a fetch() made by OLX's web app,
// which runs on the given origin.
// With a User-Agent that is not Chrome's, only the User-Agent is sent: client
// hints and fetch metadata from something that says it is not a browser would
// contradict it.
func setBrowserHeaders(header http.Header, userAgent, origin string, site fetchSite, method string) {
	if userAgent == "" {
		return
	}
	header.Set("User-Agent", userAgent)
	major, isChrome := chromeMajor(userAgent)
	if !isChrome {
		return
	}
	if header.Get("Accept") == "" {
		header.Set("Accept", "*/*")
	}
	header.Set("Sec-Ch-Ua", secCHUA(major))
	header.Set("Sec-Ch-Ua-Mobile", "?0")
	if strings.Contains(userAgent, "Android") {
		header.Set("Sec-Ch-Ua-Mobile", "?1")
	}
	header.Set("Sec-Ch-Ua-Platform", uaPlatform(userAgent))
	header.Set("Sec-Fetch-Dest", "empty")
	header.Set("Sec-Fetch-Mode", "cors")
	header.Set("Sec-Fetch-Site", string(site))
	header.Set("Priority", "u=1, i")
	if site == siteSameOrigin {
		// The full address goes to the same origin, and an Origin header only
		// with requests that change something.
		header.Set("Referer", origin+chatPagePath)
		if method != http.MethodGet && method != http.MethodHead {
			header.Set("Origin", origin)
		}
	} else {
		header.Set("Referer", origin+"/")
		header.Set("Origin", origin)
	}
}
