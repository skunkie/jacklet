// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// errNotTorrent is a download selector's link answering with something
// other than a torrent file, which hands the turn to the next selector.
var errNotTorrent = errors.New("the link did not serve a torrent file")

// downloadPages holds the pages a download block reads: the release's own
// page, fetched once when first needed, and the before request's answer.
type downloadPages struct {
	before  *goquery.Document
	link    *url.URL
	release *goquery.Document
}

// followDownloadBlock follows def's download block from link, the link a
// search row stored, to what a client is given: Jackett's order, which is
// the before request first, then a magnet built from the info hash block
// or the first selector whose link serves a torrent file, and otherwise
// link itself. Every request goes out with the tracker's session, so each
// one must name the tracker's own hosts, as link already does.
func (s *Scraper) followDownloadBlock(ctx context.Context, def *Tracker, baseURL, link *url.URL, cfg map[string]any) (*Download, error) {
	block := def.Download
	data := templateData{
		Config:      withSiteLink(cfg, baseURL),
		DownloadUri: newDownloadURIVars(link),
		False:       cardigannFalse,
		Today:       newTodayVars(time.Now()),
		True:        cardigannTrue,
		encoding:    def.Encoding,
	}
	pages := &downloadPages{link: link}

	if block.Before != nil {
		answer, err := s.requestBefore(ctx, def, baseURL, block.Before, pages, data)
		if err != nil {
			return nil, fmt.Errorf("the download's before request: %w", err)
		}
		pages.before = answer
	}

	method := http.MethodGet
	if strings.EqualFold(block.Method, "post") {
		method = http.MethodPost
	}
	switch {
	case block.InfoHash != nil:
		return s.downloadInfoHash(ctx, def, block.InfoHash, pages, data)
	case block.Selectors != nil:
		return s.downloadSelected(ctx, def, block.Selectors, method, pages, data)
	}
	return s.fetchTorrent(ctx, def, link, method)
}

// requestBefore makes the before request and returns its answer. Its path
// is resolved against the site, after being read from the release's page
// when the block names a path selector, and its inputs travel as the query
// string or, for a POST, as the form.
func (s *Scraper) requestBefore(ctx context.Context, def *Tracker, baseURL *url.URL, before *DownloadBefore, pages *downloadPages, data templateData) (*goquery.Document, error) {
	path := before.Path
	if before.PathSelector != nil {
		page, err := s.downloadPage(ctx, def, pages, false)
		if err != nil {
			return nil, err
		}
		selected, ok := selectDownloadValue(page, *before.PathSelector, data, s.logger)
		if !ok {
			return nil, errors.New("the path selector matched nothing")
		}
		path = selected
	}
	rendered, err := renderTemplateStrict(path, data)
	if err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	ref, err := url.Parse(strings.TrimSpace(rendered))
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	form := url.Values{}
	for key, value := range before.Inputs {
		rendered, err := renderTemplateStrict(value, data)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", key, err)
		}
		form.Set(key, rendered)
	}
	method := "get"
	if strings.EqualFold(before.Method, "post") {
		method = "post"
	}
	answer, err := s.fetchDownloadPage(ctx, def, &SearchPath{Method: method}, baseURL.ResolveReference(ref), form)
	if errors.Is(err, errTrackerStatus) || errors.Is(err, errBrowserErrorPage) {
		// Jackett goes on whatever the before request is answered with:
		// a tracker may refuse a second "thanks" for a release with an
		// error status and still serve the file. Through FlareSolverr the
		// same answer with no body leaves the browser on its error page.
		// The answer has no page for a selector to read.
		s.logger.Debug("download's before request failed, going on without its answer", "tracker", def.Name, "error", err)
		return goquery.NewDocumentFromReader(strings.NewReader(""))
	}
	return answer, err
}

// downloadPage returns the page a download selector reads: the before
// request's answer when the selector asks for it and there is one, and the
// release's page otherwise.
func (s *Scraper) downloadPage(ctx context.Context, def *Tracker, pages *downloadPages, isBeforeResponse bool) (*goquery.Document, error) {
	if isBeforeResponse && pages.before != nil {
		return pages.before, nil
	}
	if pages.release == nil {
		page, err := s.fetchDownloadPage(ctx, def, &SearchPath{FollowRedirect: true, Method: "get"}, pages.link, nil)
		if err != nil {
			return nil, fmt.Errorf("release page: %w", err)
		}
		pages.release = page
	}
	return pages.release, nil
}

// fetchDownloadPage fetches a page a download block reads, as a search
// fetches one, once target is known to be the tracker's own.
func (s *Scraper) fetchDownloadPage(ctx context.Context, def *Tracker, request *SearchPath, target *url.URL, form url.Values) (*goquery.Document, error) {
	if _, err := trackerSiteOf(def, target); err != nil {
		return nil, err
	}
	body, err := s.fetch(ctx, def, request, target, form)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(strings.NewReader(body))
}

// downloadInfoHash builds a magnet from the info hash and title the block
// reads, with the operator's announce URLs as for one built from a row.
// Unlike that one, it is built for a tracker of any type: the definition
// asks for it, as it is the only download the tracker offers.
func (s *Scraper) downloadInfoHash(ctx context.Context, def *Tracker, block *DownloadInfoHash, pages *downloadPages, data templateData) (*Download, error) {
	page, err := s.downloadPage(ctx, def, pages, block.UseBeforeResponse)
	if err != nil {
		return nil, err
	}
	// The hash is read off a page and written into the magnet as it
	// stands, so anything but a hash would add to the link the client is
	// sent to.
	hash, ok := selectDownloadValue(page, block.Hash, data, s.logger)
	if hash = strings.TrimSpace(hash); !ok || !isInfoHash(hash) {
		return nil, errors.New("the download's info hash selector matched no info hash")
	}
	title, ok := selectDownloadValue(page, block.Title, data, s.logger)
	if !ok {
		return nil, errors.New("the download's info hash selector matched no title")
	}
	return &Download{Magnet: infoHashMagnet(hash, strings.TrimSpace(title), s.magnetTrackers)}, nil
}

// downloadSelected tries each selector in turn, resolving the link it
// reads against the stored link as Jackett does. A magnet is the answer as
// it stands; any other link is fetched, and a link that fails, or that is
// not a torrent file when the definition tests for one, gives the next
// selector its turn.
func (s *Scraper) downloadSelected(ctx context.Context, def *Tracker, selectors []DownloadSelector, method string, pages *downloadPages, data templateData) (*Download, error) {
	var lastErr error
	for _, selector := range selectors {
		page, err := s.downloadPage(ctx, def, pages, selector.UseBeforeResponse)
		if err != nil {
			return nil, err
		}
		href, ok := selectDownloadValue(page, selector, data, s.logger)
		if href = strings.TrimSpace(href); !ok || href == "" {
			continue
		}
		if isMagnetURI(href) {
			return &Download{Magnet: href}, nil
		}
		ref, err := url.Parse(href)
		if err != nil {
			lastErr = err
			continue
		}
		download, err := s.fetchTorrent(ctx, def, pages.link.ResolveReference(ref), method)
		if err == nil && def.testsLinkTorrent() {
			err = keepIfTorrent(download)
		}
		if err != nil {
			s.logger.Debug("download selector's link failed, trying the next one",
				"tracker", def.Name, "selector", selector.Selector, "error", err)
			lastErr = err
			continue
		}
		return download, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no download selector led to a torrent: %w", lastErr)
	}
	return nil, errors.New("no download selector matched")
}

// keepIfTorrent checks that download starts as a torrent file does, with
// the "d" of a bencoded dictionary, and closes it when it does not. An
// empty body passes, as Jackett lets it. The byte read is put back, so the
// body still streams whole.
func keepIfTorrent(download *Download) error {
	if download.Magnet != "" {
		return nil
	}
	// A body fetchTorrent returns is already buffered, and is peeked there
	// rather than through a buffer of its own.
	body, isBuffered := download.Body.(peekedBody)
	if !isBuffered {
		body = peekedBody{Closer: download.Body, Reader: bufio.NewReader(download.Body)}
	}
	first, err := body.Peek(1)
	if err != nil && !errors.Is(err, io.EOF) {
		download.Body.Close()
		return err
	}
	if len(first) > 0 && first[0] != 'd' {
		download.Body.Close()
		return errNotTorrent
	}
	download.Body = body
	return nil
}

// peekedBody is a body read through a buffer that has already looked at
// its start, closing the body it buffers.
type peekedBody struct {
	io.Closer
	*bufio.Reader
}

// selectDownloadValue reads selector's value from page, as a row's field
// is read from its row, with the document's root element standing for the
// row: Jackett runs a download selector over the whole document, where
// ":root" is that element and a selector reads its first match.
func selectDownloadValue(page *goquery.Document, selector DownloadSelector, data templateData, logger *slog.Logger) (string, bool) {
	row := htmlRow{sel: page.Children().First()}
	value, ok := row.lookup(Field{
		Attribute: selector.Attribute,
		Selector:  renderTemplate(selector.Selector, data, logger),
	})
	if !ok {
		return "", false
	}
	return applyFilters(value, selector.Filters, data, logger), true
}
