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

// www.olx.pl sits behind AWS WAF/CloudFront, which allowlists real browser
// HTTP/2 fingerprints and 403s anything else. Go's net/http has a distinct,
// non-browser h2 fingerprint, so the only way the stock client gets through is
// to drop to HTTP/1.1 (which has no comparable fingerprint surface). This file
// gives the www client a real Chrome TLS+HTTP/2 fingerprint via bogdanfinn/
// tls-client instead, so it can speak HTTP/2 to www.olx.pl the way the browser
// does. It is an http.RoundTripper adapter, so the rest of the bridge keeps
// using a plain *http.Client. Only the www client uses this; the main client
// stays stock because the WebSocket upgrade rides on its transport, which
// tls-client cannot hijack.

import (
	"io"
	"net/http"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// browserProfile is the Chrome fingerprint the www client impersonates. Chrome
// 133's fingerprint is verified to pass OLX's WAF over HTTP/2. Keep it broadly
// aligned with olxapi's Chrome User-Agent; the exact minor version need not match.
var browserProfile = profiles.Chrome_133

// browserRoundTripper is an http.RoundTripper that forwards each request through
// a tls-client HTTP client, so the TLS ClientHello and HTTP/2 SETTINGS look like
// Chrome's. It converts between net/http and fhttp request/response types.
type browserRoundTripper struct {
	client tls_client.HttpClient
}

func (b *browserRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}
	freq, err := fhttp.NewRequest(req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	freq.Header = make(fhttp.Header, len(req.Header))
	for k, v := range req.Header {
		freq.Header[k] = v
	}
	// Take gzip only, so the body comes back in an encoding we can hand on as
	// plain bytes (see below) rather than br/zstd.
	freq.Header.Set("Accept-Encoding", "gzip")
	if req.Host != "" {
		freq.Host = req.Host
	}
	if req.ContentLength > 0 {
		freq.ContentLength = req.ContentLength
	}

	fresp, err := b.client.Do(freq)
	if err != nil {
		return nil, err
	}
	resp := &http.Response{
		Status:        fresp.Status,
		StatusCode:    fresp.StatusCode,
		Proto:         fresp.Proto,
		ProtoMajor:    fresp.ProtoMajor,
		ProtoMinor:    fresp.ProtoMinor,
		Header:        make(http.Header, len(fresp.Header)),
		Body:          fresp.Body,
		ContentLength: fresp.ContentLength,
		Request:       req,
	}
	for k, v := range fresp.Header {
		resp.Header[k] = v
	}
	// tls-client already decompressed the gzip body but keeps Content-Encoding;
	// present it as plain so callers (and any wrapping transport) do not try to
	// decompress it again.
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// newBrowserTransport builds a RoundTripper that impersonates Chrome, optionally
// through a proxy. Redirects are not followed here so the wrapping *http.Client
// keeps its own redirect and cookie handling.
func newBrowserTransport(proxy string) (http.RoundTripper, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(browserProfile),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithTimeoutSeconds(180),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	return &browserRoundTripper{client: c}, nil
}

// newBrowserHTTPClient returns an *http.Client whose transport impersonates
// Chrome's TLS+HTTP/2 fingerprint. Used for www.olx.pl.
func newBrowserHTTPClient(proxy string) (*http.Client, error) {
	rt, err := newBrowserTransport(proxy)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: rt, Timeout: 3 * time.Minute}, nil
}
