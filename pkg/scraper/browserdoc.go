// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// errBrowserErrorPage is what a request through FlareSolverr fails with when
// the browser ended up on its own error page: a tracker that answered with an
// error status and no body leaves the browser with nothing to show.
var errBrowserErrorPage = errors.New("the browser showed its error page")

// browserErrorPageScheme is the scheme of the address a browser reports for
// its own error page.
const browserErrorPageScheme = "chrome-error:"

// unwrapBrowserDocument returns the document a search path asked for from
// the page a browser made of it. FlareSolverr answers with the browser's
// rendered page, and a browser does not show JSON or XML as the text it was
// sent: JSON, and XML served as RSS or Atom, are wrapped in a page of their
// own with the text escaped inside a <pre>, so the body a definition expects
// to parse is inside markup that would not parse. XML served as plain XML is
// shown through a viewer that keeps the document as real elements, which the
// XML reader already selects from, and comes back as it is, like HTML paths
// and a body that is not wrapped.
func unwrapBrowserDocument(page string, path *SearchPath) string {
	if path.IsJSON() || path.IsXML() {
		return unwrapBrowserText(page)
	}
	return page
}

// unwrapBrowserText returns the text of the single <pre> a browser puts a
// document it shows as text in, undoing the escaping of the markup around
// it. A page that is not that shape comes back as it is.
func unwrapBrowserText(page string) string {
	if !strings.HasPrefix(strings.TrimSpace(page), "<") {
		return page
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		return page
	}
	pre := doc.Find("body > pre")
	if pre.Length() != 1 {
		return page
	}
	return pre.Text()
}
