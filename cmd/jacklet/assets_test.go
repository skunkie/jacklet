// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// The admin panel names the mark by absolute URL, in its header and in
// every page's tab icon, and the Torznab feed names it in its channel
// image, but neither package's own tests serve static files. Only the
// real server resolves that URL, so only a test against it holds the
// asset in place.
func TestRun_ServesTheMark(t *testing.T) {
	port := freePort(t)
	startServer(t, testSettings(t, port))

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/favicon.svg", port))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "the mark the admin panel points at is not served")
	// Contains, not Equal: the type comes from the platform's own
	// extension database, which on Windows is the registry, so it can
	// carry a charset or differ in case. Anything without svg+xml in it
	// is served as something a browser will not draw.
	require.Contains(t, resp.Header.Get("Content-Type"), "svg+xml",
		"the mark is not served as an SVG")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "<svg", "the mark is not an SVG")
}

// /docs runs whatever script the page names, so a script it loads from a
// CDN has to name an exact release and carry an integrity hash: with a
// floating version or no hash, the CDN chooses the code the page runs.
func TestDocsPageLoadsOnlyPinnedScripts(t *testing.T) {
	page, err := publicFS.ReadFile("public/docs.html")
	require.NoError(t, err)

	var external int
	tokens := html.NewTokenizer(bytes.NewReader(page))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			require.ErrorIs(t, tokens.Err(), io.EOF, "docs.html did not tokenize to its end")
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := tokens.Token()
		if token.Data != "script" {
			continue
		}
		attributes := make(map[string]string, len(token.Attr))
		for _, attribute := range token.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		src, hasSrc := attributes["src"]
		if !hasSrc {
			continue
		}
		// An address with a host is fetched from elsewhere, whether it
		// names a scheme or, as //cdn.example/x does, borrows the page's.
		address, err := url.Parse(src)
		require.NoError(t, err, "docs.html names the script %q, which is not an address", src)
		if address.Host == "" {
			continue
		}
		external++
		// A release is pinned after an @, as npm CDNs write it, or as a
		// path segment, as cdnjs does.
		require.Regexp(t, `[@/]\d+\.\d+\.\d+(/|$)`, src,
			"the script names no exact release, so the CDN chooses which code /docs runs")
		require.Regexp(t, `^sha(384|512)-[A-Za-z0-9+/]+={0,2}$`, attributes["integrity"],
			"%s carries no integrity hash, so a browser runs whatever the CDN serves under it", src)
		require.Equal(t, "anonymous", attributes["crossorigin"],
			"%s needs crossorigin=\"anonymous\", without which a browser cannot check its integrity hash", src)
	}
	require.Positive(t, external, "docs.html loads Scalar from a CDN, and no such script was found to check")
}

// The mark is embedded here and mastered in assets/, where the Windows
// icon and the installer bitmaps derive from it. Nothing in the build
// copies one to the other, so this test is what keeps the two files one
// mark rather than two.
func TestFaviconMatchesTheMasterLogo(t *testing.T) {
	master := filepath.Join("..", "..", "assets", "logo.svg")

	served, err := os.ReadFile(filepath.Join("public", "favicon.svg"))
	require.NoError(t, err)
	mastered, err := os.ReadFile(master)
	require.NoError(t, err)

	require.Equal(t, string(mastered), string(served),
		"cmd/jacklet/public/favicon.svg has drifted from %s; copy the master over it", master)
}

// The wordmark sets the same mark beside the product name for the README.
// It repeats the master's paths rather than referencing them, because an
// SVG that pulled them in would stop rendering on a page that blocks the
// second request, so only this comparison keeps the two files one mark.
func TestWordmarkMatchesTheMasterLogo(t *testing.T) {
	paths := func(t *testing.T, file string) []string {
		t.Helper()
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		// [^>]+ rather than [^>]*: the latter spells the sequence that
		// closes a block comment, and a Go file is read as text by more
		// than the compiler.
		found := regexp.MustCompile(`<path [^>]+/>`).FindAllString(string(data), -1)
		require.NotEmpty(t, found, "%s draws nothing", file)
		return found
	}

	master := filepath.Join("..", "..", "assets", "logo.svg")
	wordmark := filepath.Join("..", "..", "assets", "logo-wordmark.svg")

	require.Equal(t, paths(t, master), paths(t, wordmark),
		"the mark in %s has drifted from %s; copy the master's paths over it", wordmark, master)
}

// The wordmark carries no plate of its own, so it inverts for a reader
// whose preference is dark. The two fills it swaps to are the master's
// own, written out a second time in CSS, which is what this holds them to.
func TestWordmarkInvertsTheMasterFills(t *testing.T) {
	read := func(t *testing.T, file string) string {
		t.Helper()
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		return string(data)
	}

	master := filepath.Join("..", "..", "assets", "logo.svg")
	wordmark := filepath.Join("..", "..", "assets", "logo-wordmark.svg")

	painted := regexp.MustCompile(`<path[^>]*fill="([^"]+)"`).FindAllStringSubmatch(read(t, master), -1)
	require.Len(t, painted, 2, "%s no longer paints a badge and a mark", master)
	badge, knockout := painted[0][1], painted[1][1]

	inverted := read(t, wordmark)
	for _, want := range []struct {
		fill string
		rule string
	}{
		{fill: knockout, rule: "path:nth-of-type(1)"},
		{fill: badge, rule: "path:nth-of-type(2)"},
	} {
		require.Contains(t, inverted, want.rule+" { fill: "+want.fill+"; }",
			"the wordmark does not invert %s to the fill %s uses", want.rule, master)
	}
}
