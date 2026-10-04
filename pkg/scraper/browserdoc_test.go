// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The samples below are the pages a browser builds for each kind of
// response, as headless Chrome rendered them, with the listing replaced by
// an invented one.
const (
	browserJSONPage = `<html><head><meta name="color-scheme" content="light dark"><meta charset="utf-8"></head><body><pre>[{"Name":"Example Release","Url":"https://example.invalid/1"}]</pre><div class="json-formatter-container"></div></body></html>`

	browserPlainJSONPage = `<html><head><meta name="color-scheme" content="light dark"></head><body><pre style="word-wrap: break-word; white-space: pre-wrap;">[{"Name":"A &amp; B &lt;tag&gt;","Url":"https://example.invalid/1"}]</pre></body></html>`

	browserRSSPage = `<html><head><meta name="color-scheme" content="light dark"></head><body><pre style="word-wrap: break-word; white-space: pre-wrap;">&lt;?xml version="1.0" encoding="UTF-8"?&gt;&lt;rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"&gt;&lt;channel&gt;&lt;item&gt;&lt;title&gt;Example &amp;amp; Release&lt;/title&gt;&lt;link&gt;https://example.invalid/1&lt;/link&gt;&lt;torznab:attr name="seeders" value="5"/&gt;&lt;/item&gt;&lt;/channel&gt;&lt;/rss&gt;</pre></body></html>`

	browserXMLPage = `<html xmlns="http://www.w3.org/1999/xhtml"><head><style id="xml-viewer-style">div.header { margin: 10px; }</style></head><body>` +
		`<div id="webkit-xml-viewer-source-xml"><rss xmlns="" version="2.0"><channel><item><title>Example Release</title><link>https://example.invalid/1</link></item></channel></rss></div>` +
		`<div class="header"><span>This XML file does not appear to have any style information associated with it. The document tree is shown below.</span><br /></div>` +
		`<div class="pretty-print"><div class="folder" id="folder0"><div class="line"><span class="html-tag">&lt;rss</span></div></div></div></body></html>`
)

// The error pages below keep, of what Chrome renders, the element naming
// what went wrong.
const (
	browserStatusErrorPage  = `<html><body><div id="main-message"><h1>This page isn't working</h1><div class="error-code">HTTP ERROR 500</div></div></body></html>`
	browserNetworkErrorPage = `<html><body><div id="main-message"><h1>This site can't be reached</h1><div class="error-code">ERR_CONNECTION_REFUSED</div></div></body></html>`
)

// FlareSolverr answers with the browser's rendered page, and a browser wraps
// JSON, and XML served as RSS, in a page of its own, so a definition that
// parses either would otherwise be handed markup. XML shown through the
// browser's viewer is left as it is.
func TestUnwrapBrowserDocument(t *testing.T) {
	jsonPath := &SearchPath{Response: &Response{Type: "json"}}
	xmlPath := &SearchPath{Response: &Response{Type: "xml"}}
	htmlPath := &SearchPath{}

	tests := []struct {
		name string
		page string
		path *SearchPath
		want string
	}{
		{name: "JSON in a pre", page: browserJSONPage, path: jsonPath, want: `[{"Name":"Example Release","Url":"https://example.invalid/1"}]`},
		{name: "JSON served as text, with escaped markup", page: browserPlainJSONPage, path: jsonPath, want: `[{"Name":"A & B <tag>","Url":"https://example.invalid/1"}]`},
		{name: "JSON that is not wrapped", page: `[{"Name":"Example Release"}]`, path: jsonPath, want: `[{"Name":"Example Release"}]`},
		{name: "JSON path answered with a page that has no pre", page: `<html><body>Sorry</body></html>`, path: jsonPath, want: `<html><body>Sorry</body></html>`},
		{name: "JSON path answered with two pre elements", page: `<html><body><pre>a</pre><pre>b</pre></body></html>`, path: jsonPath, want: `<html><body><pre>a</pre><pre>b</pre></body></html>`},
		{name: "XML served as RSS, in a pre", page: browserRSSPage, path: xmlPath, want: `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Example &amp; Release</title><link>https://example.invalid/1</link><torznab:attr name="seeders" value="5"/></item></channel></rss>`},
		{name: "XML shown through the viewer is left as it is", page: browserXMLPage, path: xmlPath, want: browserXMLPage},
		{name: "an HTML path is left alone", page: browserJSONPage, path: htmlPath, want: browserJSONPage},
		{name: "a path with no response block", page: `<pre>x</pre>`, path: &SearchPath{Response: nil}, want: `<pre>x</pre>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, unwrapBrowserDocument(tc.page, tc.path))
		})
	}
}

// A JSON definition behind FlareSolverr has to store its rows: parsing the
// browser's page as JSON fails on the first "<".
func TestScraperJSONDefinitionThroughFlareSolverr(t *testing.T) {
	fake := newFakeFlareSolverr(browserJSONPage)
	flare := httptest.NewServer(fake)
	defer flare.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "json-tracker", `
id: json-tracker
name: json-tracker
links:
  - http://example.invalid/
search:
  paths:
    - path: "/"
      response:
        type: json
  rows:
    selector: "$"
  fields:
    title:
      selector: Name
    details:
      selector: Url
`)
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "json-tracker")

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))
	stored, err := store.Recent(t.Context(), "json-tracker", 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, "Example Release", stored[0].Name)
}

// An XML definition behind FlareSolverr reads the feed whichever way the
// browser showed it: as text in a pre when it was served as RSS, or through
// the XML viewer, which keeps the document as real elements.
func TestScraperXMLDefinitionThroughFlareSolverr(t *testing.T) {
	for name, page := range map[string]string{"served as RSS": browserRSSPage, "shown in the viewer": browserXMLPage} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeFlareSolverr(page)
			flare := httptest.NewServer(fake)
			defer flare.Close()

			store := &fakeStore{}

			def := loadTestTracker(t, t.TempDir(), "xml-tracker", `
id: xml-tracker
name: xml-tracker
links:
  - http://example.invalid/
search:
  paths:
    - path: "/"
      response:
        type: xml
  rows:
    selector: "channel > item"
  fields:
    title:
      selector: title
    details:
      selector: link
`)
			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
			scrpr.useFlareSolverr(t, flare.URL, "xml-tracker")

			require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))
			stored, err := store.Recent(t.Context(), "xml-tracker", 10)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Contains(t, stored[0].Name, "Example")
		})
	}
}

// A tracker that answers with an error status and no body leaves the browser
// on its own error page, whose address is not the tracker's. That is a
// failed scrape, as a direct fetch of the same answer would be, and it says
// nothing about the browser, so the session is not counted against.
func TestScraperBrowserErrorPageIsATrackerFailure(t *testing.T) {
	fake := newFakeFlareSolverr("<html><head><title>example.invalid</title></head><body>This page isn't working</body></html>")
	fake.solutionURL = "chrome-error://chromewebdata/"
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	scrpr.useFlareSolverr(t, flare.URL, "example-tracker")

	for range flareSessionMaxFailures + 1 {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "passkey=secret")
		require.ErrorIs(t, err, errBrowserErrorPage)
		require.NotContains(t, err.Error(), "secret", "the tracker's passkey reached the error")
		require.Contains(t, err.Error(), "query redacted")
	}
	require.Empty(t, fake.destroyed, "a tracker's error page counted against a healthy session")
}

func TestBrowserNetworkError(t *testing.T) {
	for _, tc := range []struct {
		name string
		page string
		want string
	}{
		{name: "a network error", page: browserNetworkErrorPage, want: "ERR_CONNECTION_REFUSED"},
		{name: "an error status", page: browserStatusErrorPage},
		{name: "an error status under its network error's name", page: `<div class="error-code">ERR_HTTP_RESPONSE_CODE_FAILURE</div>`},
		{name: "more than a name", page: `<div class="error-code">ERR_CONNECTION_REFUSED and more</div>`},
		{name: "a page naming neither", page: "<html><body>This page isn't working</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, browserNetworkError(tc.page))
		})
	}
}

// A tracker the browser could not reach leaves it on its error page too, but
// that is not a status the tracker answered with, so it fails as itself
// rather than as errBrowserErrorPage, and it says nothing about the browser.
func TestScraperBrowserNetworkErrorIsNotAnErrorStatus(t *testing.T) {
	fake := newFakeFlareSolverr(browserNetworkErrorPage)
	fake.solutionURL = "chrome-error://chromewebdata/"
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	scrpr.useFlareSolverr(t, flare.URL, "example-tracker")

	_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "passkey=secret")
	require.ErrorContains(t, err, "ERR_CONNECTION_REFUSED")
	require.NotErrorIs(t, err, errBrowserErrorPage)
	require.NotContains(t, err.Error(), "secret", "the tracker's passkey reached the error")
	require.Empty(t, fake.destroyed, "an unreachable tracker counted against a healthy session")
}
